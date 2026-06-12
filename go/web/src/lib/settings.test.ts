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
    const groups = groupByPrefix([
      view({ key: 'chat.host', type: 'string' }),
      view({ key: 'chat.model', type: 'string' }),
      view({ key: 'rerank.enabled', type: 'bool' }),
      view({ key: 'chat_fallback.host', type: 'string' }),
    ])
    expect(groups.map((g) => g.prefix)).toEqual(['chat', 'rerank', 'chat_fallback'])
    expect(groups[0].settings.map((s) => s.key)).toEqual(['chat.host', 'chat.model'])
  })
})

describe('draftFor', () => {
  it('uses the value text for conforming values', () => {
    expect(draftFor(view({ key: 'a.b', type: 'int', value: 50 }))).toBe('50')
    expect(draftFor(view({ key: 'a.b', type: 'bool', value: true }))).toBe('true')
    expect(draftFor(view({ key: 'a.b', type: 'scopes', value: ['private', 'shared'] }))).toBe('private,shared')
  })

  it('starts empty for sensitive keys — the PUT takes a secret name', () => {
    expect(draftFor(view({ key: 'chat.api_key', type: 'string', value: '(set via env)', sensitive: true }))).toBe('')
  })

  it('starts empty for display artifacts (string rendering on an int key)', () => {
    expect(draftFor(view({ key: 'dream_embed.num_ctx', type: 'int', value: '(inherit embed)' }))).toBe('')
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

describe('crossFieldIssues (mirror of validate.go V1/V2/V3)', () => {
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

  it('V1: num_ctx split on the same host warns, errors under ollama', () => {
    const base = {
      'dream.num_ctx': 4096,
      'chat.num_ctx': 8192,
      'dream.host': 'http://gpu:8089',
      'chat.host': 'http://gpu:8089',
      'dream.protocol': 'openai',
    }
    expect(crossFieldIssues(effectiveOf(base))[0]).toMatchObject({
      key: 'dream.num_ctx',
      severity: 'warn',
    })
    expect(crossFieldIssues(effectiveOf({ ...base, 'dream.protocol': 'ollama' }))[0]).toMatchObject({
      severity: 'error',
    })
    expect(crossFieldIssues(effectiveOf({ ...base, 'chat.host': 'http://other:8089' }))).toEqual([])
    expect(crossFieldIssues(effectiveOf({ ...base, 'dream.num_ctx': 0 }))).toEqual([])
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
