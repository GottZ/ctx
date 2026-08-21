// Pins the pure settings-UI logic against the as-built F2 contract: widget
// dispatch incl. the unknown-type read-only fallback, prefix grouping in
// registry order, draft round-trips for the display-artifact cases, and the
// client mirror of the three validate.go cross-field rules.

import { describe, expect, it } from 'vitest'
import type { SettingView } from './api/types'
import {
  crossFieldIssues,
  draftFor,
  formatValue,
  groupByPrefix,
  groupDomId,
  isEditable,
  mutabilityNote,
  parseDraft,
  selectOptions,
  widgetFor,
} from './settings'

function view(partial: Partial<SettingView> & Pick<SettingView, 'key' | 'type'>): SettingView {
  return { mutability: 'hot', value: '', source: 'env', default: '', ...partial }
}

describe('widgetFor', () => {
  it('dispatches every registry type', () => {
    expect(widgetFor('bool')).toBe('switch')
    expect(widgetFor('int')).toBe('number')
    expect(widgetFor('float')).toBe('number')
    expect(widgetFor('protocol')).toBe('select')
    expect(widgetFor('think')).toBe('select')
    for (const t of ['string', 'seconds', 'hours', 'timezone', 'scopes']) {
      expect(widgetFor(t)).toBe('text')
    }
  })

  it('renders unknown future types read-only', () => {
    expect(widgetFor('quantum')).toBe('readonly')
  })
})

describe('selectOptions', () => {
  it('mirrors the Go enums', () => {
    expect(selectOptions('protocol')).toEqual(['ollama', 'openai'])
    expect(selectOptions('think')).toEqual(['true', 'false'])
    expect(selectOptions('string')).toEqual([])
  })
})

describe('isEditable / mutabilityNote', () => {
  it('lets hot and coupled:embed-cache through, blocks restart and coupled', () => {
    expect(isEditable(view({ key: 'a.b', type: 'string', mutability: 'hot' }))).toBe(true)
    expect(isEditable(view({ key: 'a.b', type: 'string', mutability: 'coupled:embed-cache' }))).toBe(true)
    expect(isEditable(view({ key: 'a.b', type: 'string', mutability: 'restart' }))).toBe(false)
    expect(isEditable(view({ key: 'a.b', type: 'string', mutability: 'coupled' }))).toBe(false)
  })

  it('treats an unknown mutability class as read-only', () => {
    const s = view({ key: 'a.b', type: 'string', mutability: 'pending-restart' })
    expect(isEditable(s)).toBe(false)
    expect(mutabilityNote(s)).toContain('unknown mutability')
  })

  it('names the env var in the restart note', () => {
    const s = view({ key: 'server.db', type: 'string', mutability: 'restart', env_var: 'CONTEXT_DB' })
    expect(mutabilityNote(s)).toContain('CONTEXT_DB')
  })
})

describe('groupByPrefix', () => {
  it('groups on the prefix before the first dot, preserving order', () => {
    // Vehicle swap (β10): the chat/chat_fallback keys left the registry with
    // their tuples. The shape the case needs is unchanged — two keys sharing a
    // prefix, a foreign key between them, and one prefix carrying an underscore
    // so the split is provably on the first DOT, not on the separator that
    // looks like one. graph_overview.* is a live carrier of that shape.
    const groups = groupByPrefix([
      view({ key: 'dream.language', type: 'string' }),
      view({ key: 'dream.parallelism', type: 'int' }),
      view({ key: 'rerank.enabled', type: 'bool' }),
      view({ key: 'graph_overview.engine', type: 'string' }),
    ])
    expect(groups.map((g) => g.prefix)).toEqual(['dream', 'rerank', 'graph_overview'])
    expect(groups[0].settings.map((s) => s.key)).toEqual(['dream.language', 'dream.parallelism'])
  })
})

describe('draftFor', () => {
  it('uses the value text for conforming values', () => {
    expect(draftFor(view({ key: 'a.b', type: 'int', value: 50 }))).toBe('50')
    expect(draftFor(view({ key: 'a.b', type: 'bool', value: true }))).toBe('true')
    expect(draftFor(view({ key: 'a.b', type: 'scopes', value: ['private', 'shared'] }))).toBe('private,shared')
  })

  it('starts empty for sensitive keys — the PUT takes a secret name', () => {
    // Vehicle swap (β10): chat.api_key left the registry with the chat tuple.
    // server.db_password is the one sensitive key the cut left standing, so the
    // fixture now names a key the server would actually mask.
    expect(draftFor(view({ key: 'server.db_password', type: 'string', value: 'set', sensitive: true }))).toBe('')
  })

  it('starts empty for display artifacts (string rendering on an int key)', () => {
    // The concrete artifact this case was written for — dream_embed.num_ctx
    // rendering "(inherit embed)" — died with the inherit markers in β5/β6.
    // The guard it pins did not: widgetFor('int') still refuses any non-number
    // rendering, whatever the server sends, and dream.parallelism is a live int
    // key to state that on.
    expect(draftFor(view({ key: 'dream.parallelism', type: 'int', value: '(unset)' }))).toBe('')
  })
})

describe('parseDraft', () => {
  const intView = view({ key: 'a.b', type: 'int' })

  it('rejects empty drafts toward the reset affordance', () => {
    const r = parseDraft(intView, '  ')
    expect(r.ok).toBe(false)
  })

  it('types the scalar for the PUT body', () => {
    expect(parseDraft(intView, '42')).toEqual({ ok: true, value: 42 })
    expect(parseDraft(view({ key: 'a.b', type: 'float' }), '0.7')).toEqual({ ok: true, value: 0.7 })
    expect(parseDraft(view({ key: 'a.b', type: 'bool' }), 'true')).toEqual({ ok: true, value: true })
    expect(parseDraft(view({ key: 'a.b', type: 'hours' }), '45d')).toEqual({ ok: true, value: '45d' })
  })

  it('rejects type garbage client-side', () => {
    expect(parseDraft(intView, '1.5').ok).toBe(false)
    expect(parseDraft(intView, 'abc').ok).toBe(false)
    expect(parseDraft(view({ key: 'a.b', type: 'float' }), 'abc').ok).toBe(false)
    expect(parseDraft(view({ key: 'a.b', type: 'bool' }), 'yes').ok).toBe(false)
  })
})

describe('formatValue', () => {
  it('renders arrays, empties and scalars', () => {
    expect(formatValue(['a', 'b'])).toBe('a,b')
    expect(formatValue('')).toBe('(empty)')
    expect(formatValue(0.5)).toBe('0.5')
  })
})

// The V1 case (dream/chat num_ctx split on one host) left with the mirror in β9:
// its server rule retired with the dream tuple in β6 and every key it read is
// out of the registry, so the branch was unreachable.
describe('crossFieldIssues (mirror of validate.go V2/V3)', () => {
  function effectiveOf(values: Record<string, unknown>) {
    return (key: string) => values[key]
  }

  it('V2: inverted thresholds are an error', () => {
    const issues = crossFieldIssues(
      effectiveOf({ 'query.score_threshold': 0.01, 'query.confident_threshold': 0.008 }),
    )
    expect(issues).toHaveLength(1)
    expect(issues[0]).toMatchObject({ key: 'query.score_threshold', severity: 'error' })
  })

  it('V3: blend 1.0 with graph enabled warns', () => {
    const issues = crossFieldIssues(effectiveOf({ 'rerank.blend_weight': 1, 'graph.enabled': true }))
    expect(issues).toHaveLength(1)
    expect(issues[0]).toMatchObject({ key: 'rerank.blend_weight', severity: 'warn' })
    expect(crossFieldIssues(effectiveOf({ 'rerank.blend_weight': 0.5, 'graph.enabled': true }))).toEqual([])
  })

  it('stays silent on an incomplete catalog (missing keys)', () => {
    expect(crossFieldIssues(effectiveOf({}))).toEqual([])
  })
})

// The 'superseded keys (Entflechtungs-Welle Stufe 1)' block died with its
// subject in β9 (E11): it pinned the read-only rendering, the backend-pool note
// and the trailing "legacy (superseded)" pseudo-group. No API field, no marker,
// no legacy card exists any more — every key renders in its own prefix card,
// which the groupByPrefix block above asserts. The absence of the legacy card in
// the rendered page is asserted at the e2e tier (e2e/contract/registry.ts).
describe('groupDomId', () => {
  it('derives the card/jump-nav anchor from the prefix', () => {
    expect(groupDomId('dream')).toBe('settings-dream')
    expect(groupDomId('embed_backfill')).toBe('settings-embed_backfill')
  })

  it('sanitises characters an HTML id must not carry', () => {
    expect(groupDomId('two words')).toBe('settings-two-words')
  })
})
