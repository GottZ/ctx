// Pool-editor pure-logic gates: trust ranks + elevation (the confirm trigger),
// the priority sort, model_map row<->object conversion, the create spec and the
// update diff (raw-key-presence semantics: only changed keys, an emptied list
// clears), the referenced_by split (settings keys vs "backend:" pool rows) and
// the dangling scan (refs whose secret is gone).

import { describe, expect, it } from 'vitest'
import { ApiError } from './api'
import type { BackendListItem, BackendView, SecretMeta, SettingView } from './api/types'
import {
  backendDiff,
  createSpec,
  draftFromBackend,
  fieldErrors,
  isEmptySpec,
  isTrustElevation,
  modelMapToRows,
  rowsToModelMap,
  secretUsage,
  sortBackends,
  splitSecretRefs,
  trustRank,
} from './backends'

function bv(p: Partial<BackendView> & Pick<BackendView, 'name'>): BackendView {
  return {
    id: p.id ?? `id-${p.name}`,
    base_url: 'http://x',
    protocol: 'openai',
    provider_class: 'generic',
    scope: '_global',
    api_key_ref: '',
    trust: 'public',
    locality: 'local',
    roles: [],
    model_map: {},
    timeouts: {},
    num_ctx: 0,
    priority: 0,
    enabled: true,
    extra_headers: {},
    extra_body: {},
    limits: {},
    metadata: {},
    ...p,
  }
}

function item(p: Partial<BackendListItem> & Pick<BackendListItem, 'name'>): BackendListItem {
  return { ...bv(p), effective_state: 'active', cooldown_remaining_s: 0, consecutive_fails: 0, ...p }
}

function setting(p: Partial<SettingView> & Pick<SettingView, 'key'>): SettingView {
  return { type: 'string', mutability: 'hot', value: '', source: 'env', default: '', ...p }
}

describe('trust', () => {
  it('ranks the four FIX levels, unknown admits nothing', () => {
    expect(trustRank('full-trust')).toBe(3)
    expect(trustRank('no-credentials')).toBe(2)
    expect(trustRank('non-personal')).toBe(1)
    expect(trustRank('public')).toBe(0)
    expect(trustRank('bogus')).toBe(-1)
  })

  it('detects elevation only when the rank rises', () => {
    expect(isTrustElevation('public', 'full-trust')).toBe(true)
    expect(isTrustElevation('non-personal', 'no-credentials')).toBe(true)
    expect(isTrustElevation('full-trust', 'public')).toBe(false)
    expect(isTrustElevation('no-credentials', 'no-credentials')).toBe(false)
  })
})

describe('sortBackends', () => {
  it('orders priority DESC then name ASC (the server Chain order)', () => {
    const list = [
      item({ name: 'b', priority: 5 }),
      item({ name: 'a', priority: 10 }),
      item({ name: 'c', priority: 5 }),
    ]
    expect(sortBackends(list).map((b) => b.name)).toEqual(['a', 'b', 'c'])
  })

  it('does not mutate the input', () => {
    const list = [item({ name: 'b', priority: 1 }), item({ name: 'a', priority: 2 })]
    sortBackends(list)
    expect(list.map((b) => b.name)).toEqual(['b', 'a'])
  })
})

describe('model_map conversion', () => {
  it('round-trips object form and preserves params', () => {
    const mm = { chat: { model: 'qwen', params: { temp: 0.7 } }, default: { model: 'fallback' } }
    const rows = modelMapToRows(mm)
    expect(rows).toContainEqual({ role: 'chat', model: 'qwen', params: { temp: 0.7 } })
    const back = rowsToModelMap(rows)
    expect(back.chat).toEqual({ model: 'qwen', params: { temp: 0.7 } })
    expect(back.default).toEqual({ model: 'fallback' })
  })

  it('drops blank rows and trims', () => {
    const out = rowsToModelMap([
      { role: ' chat ', model: ' m ' },
      { role: '', model: 'x' },
      { role: 'dream', model: '' },
    ])
    expect(out).toEqual({ chat: { model: 'm' } })
  })
})

describe('createSpec', () => {
  it('includes name + core fields, drops auto-locality and empty api_key_ref', () => {
    const draft = draftFromBackend(bv({ name: 'x', trust: 'public', locality: '', api_key_ref: '' }))
    const spec = createSpec('herbert-chat', draft)
    expect(spec.name).toBe('herbert-chat')
    expect(spec.locality).toBeUndefined()
    expect(spec.api_key_ref).toBeUndefined()
    expect(spec.trust).toBe('public')
  })

  it('keeps an explicit locality and api_key_ref', () => {
    const draft = draftFromBackend(bv({ name: 'x', locality: 'external', api_key_ref: 'or.key' }))
    const spec = createSpec('or', draft)
    expect(spec.locality).toBe('external')
    expect(spec.api_key_ref).toBe('or.key')
  })

  it('carries an in-dialog metadata patch, omits it when absent', () => {
    const draft = draftFromBackend(bv({ name: 'x' }))
    expect(createSpec('x', draft).metadata).toBeUndefined()
    draft.metadata = { embed_equivalence_verified: true }
    expect(createSpec('x', draft).metadata).toEqual({ embed_equivalence_verified: true })
  })
})

describe('backendDiff', () => {
  it('emits only the changed scalar fields', () => {
    const orig = bv({ name: 'x', base_url: 'http://a', num_ctx: 4096, trust: 'public' })
    const draft = draftFromBackend(orig)
    draft.base_url = 'http://b'
    draft.trust = 'no-credentials'
    const spec = backendDiff(orig, draft)
    expect(spec).toEqual({ base_url: 'http://b', trust: 'no-credentials' })
  })

  it('treats roles as an order-insensitive set', () => {
    const orig = bv({ name: 'x', roles: ['chat', 'synthesis'] })
    const draft = draftFromBackend(orig)
    draft.roles = ['synthesis', 'chat']
    expect(isEmptySpec(backendDiff(orig, draft))).toBe(true)
    draft.roles = ['synthesis', 'chat', 'dream']
    expect(backendDiff(orig, draft).roles).toEqual(['synthesis', 'chat', 'dream'])
  })

  it('sends an explicit empty roles to CLEAR a previously set list', () => {
    const orig = bv({ name: 'x', roles: ['chat'] })
    const draft = draftFromBackend(orig)
    draft.roles = []
    expect(backendDiff(orig, draft).roles).toEqual([])
  })

  it('detects model_map changes incl. params', () => {
    const orig = bv({ name: 'x', model_map: { chat: { model: 'a' } } })
    const draft = draftFromBackend(orig)
    expect(isEmptySpec(backendDiff(orig, draft))).toBe(true)
    draft.model_map = { chat: { model: 'b' } }
    expect(backendDiff(orig, draft).model_map).toEqual({ chat: { model: 'b' } })
  })

  it('MERGES a metadata patch over the stored object (applySpec replaces wholesale)', () => {
    const orig = bv({ name: 'x', metadata: { score_domain: 'logit', other: 1 } })
    const draft = draftFromBackend(orig)
    expect(backendDiff(orig, draft).metadata).toBeUndefined()
    draft.metadata = { embed_equivalence_verified: true }
    expect(backendDiff(orig, draft).metadata).toEqual({
      score_domain: 'logit',
      other: 1,
      embed_equivalence_verified: true,
    })
  })
})

describe('fieldErrors', () => {
  it('extracts the 422 fields array off ApiError.details', () => {
    const err = new ApiError(422, 'validation', 'validation failed', null, {
      success: false,
      error: 'validation failed',
      fields: [{ field: 'model_map', message: 'role "chat" has no model_map entry and no default' }],
    })
    expect(fieldErrors(err)).toEqual([
      { field: 'model_map', message: 'role "chat" has no model_map entry and no default' },
    ])
  })

  it('returns empty for a failure without fields', () => {
    expect(fieldErrors(new ApiError(400, 'bad_request', 'nope'))).toEqual([])
    expect(fieldErrors(new Error('network'))).toEqual([])
  })
})

describe('splitSecretRefs', () => {
  it('splits the flat referenced_by by its "backend:" prefix', () => {
    // Vehicle swap (β10): the six *.api_key keys left the registry, so
    // server.db_password is the only live settings key that can appear in a
    // referenced_by list at all. The function is key-agnostic — it splits on
    // the prefix, not on the registry — and the second settings entry stays
    // because multiplicity and order are part of what it must preserve.
    expect(splitSecretRefs(['server.db_password', 'backend:openrouter', 'dream.language'])).toEqual({
      settings: ['server.db_password', 'dream.language'],
      backends: ['openrouter'],
    })
  })

  it('treats a settings key that merely CONTAINS "backend" as a settings key', () => {
    // Only the prefix marks a pool row; a key like backend.pool.enabled is a
    // settings key and must not land in the backends bucket.
    expect(splitSecretRefs(['backend.pool.enabled', 'backends:weird'])).toEqual({
      settings: ['backend.pool.enabled', 'backends:weird'],
      backends: [],
    })
  })

  it('strips only the first prefix — a row named like a prefix stays intact', () => {
    expect(splitSecretRefs(['backend:backend:odd']).backends).toEqual(['backend:odd'])
  })

  it('drops a bare prefix instead of rendering an anonymous reference', () => {
    expect(splitSecretRefs(['backend:'])).toEqual({ settings: [], backends: [] })
  })

  it('handles the empty and the absent list', () => {
    expect(splitSecretRefs([])).toEqual({ settings: [], backends: [] })
    expect(splitSecretRefs(undefined)).toEqual({ settings: [], backends: [] })
  })
})

describe('secretUsage', () => {
  const secrets: SecretMeta[] = [
    { name: 'or.key', key_version: 1, created_at: 't', referenced_by: ['server.db_password'] },
  ]

  // An INTACT backend ref is not the client's business any more: the server
  // reports it in referenced_by as "backend:router" (splitSecretRefs below).
  // Re-deriving it here showed the same reference twice under two labels.
  it('stays silent about a backend ref whose secret exists', () => {
    const backends = [item({ name: 'router', api_key_ref: 'or.key' })]
    const u = secretUsage(secrets, backends, [])
    expect(u.dangling).toEqual([])
  })

  it('flags a backend referencing a missing secret', () => {
    const backends = [item({ name: 'router', api_key_ref: 'gone' })]
    const u = secretUsage(secrets, backends, [])
    expect(u.dangling).toContainEqual({ source: 'backend', ref: 'router', secret: 'gone' })
  })

  it('flags a db-sourced sensitive setting pointing at a missing secret', () => {
    const settings = [setting({ key: 'server.db_password', sensitive: true, source: 'db', value: 'gone' })]
    const u = secretUsage(secrets, [], settings)
    expect(u.dangling).toContainEqual({ source: 'setting', ref: 'server.db_password', secret: 'gone' })
  })

  it('ignores env-sourced and non-sensitive settings (no secret name there)', () => {
    const settings = [
      setting({ key: 'server.db_password', sensitive: true, source: 'env', value: 'set' }),
      setting({ key: 'dream.language', sensitive: false, source: 'db', value: 'something' }),
    ]
    const u = secretUsage(secrets, [], settings)
    expect(u.dangling).toEqual([])
  })
})
