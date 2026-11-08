// W05-13 — alias catalog editor logic (design/05 §3.3, §4.6 ModelsPage, §5
// B-8/B-17/B-23; RC-D C5). Gates:
//   - `_global` rows carry no write control for a tenant-admin — red against
//     controls gated on `writable` alone (inv05 D8: one lock is no lock);
//   - name and kind are read-only on edit and never in the update patch
//     (V-02-a, 400 immutable_field);
//   - "Suggest from pool" drafts are enabled:false — red against omitting
//     the field (the store default would be live); every create sends
//     `enabled` explicitly;
//   - lowering the sensitivity needs a confirm, and only the confirmed
//     request carries confirm_sensitivity_downgrade:true.

import { describe, expect, it } from 'vitest'
import { ApiError } from '../../lib/api'
import {
  LOCKED_ON_EDIT,
  POOL_ROLES,
  aliasSensitivityOptions,
  buildAliasCreate,
  buildAliasUpdate,
  fieldErrors,
  needsDowngradeConfirm,
  policyKeys,
  rowControls,
  suggestDraft,
  type AliasForm,
} from './catalog'
import { aliasOf } from './forms.fixtures'

const FORM: AliasForm = {
  name: 'team-chat',
  kind: 'chat',
  role: 'chat',
  scope: '',
  sensitivity: 'credentials',
  description: '',
  enabled: true,
  policy: {},
}

describe('W05-13 row controls', () => {
  it('W05-13 gate: a _global row carries no write control for a tenant-admin — even when writable is (wrongly) true', () => {
    const home = aliasOf('a-home', 'home', 'chat')
    expect(rowControls(home, false), 'positive control: the home-scope row is editable').toEqual({ edit: true, toggle: true })
    expect(rowControls(aliasOf('a-g', '_global', 'chat'), false), 'W05-13 gate: _global row, tenant-admin').toEqual({ edit: false, toggle: false })
    const forged = aliasOf('a-g2', '_global', 'chat', { writable: true })
    expect(rowControls(forged, false), 'W05-13 gate: the scope is a second lock next to writable').toEqual({ edit: false, toggle: false })
  })

  it('W05-13: a server-admin writes _global rows; a row the server marks read-only stays read-only', () => {
    expect(rowControls(aliasOf('a-g', '_global', 'chat', { writable: true }), true), 'server-admin, _global').toEqual({ edit: true, toggle: true })
    expect(rowControls(aliasOf('a-x', 'home', 'chat', { writable: false }), true), 'writable:false wins').toEqual({ edit: false, toggle: false })
  })
})

describe('W05-13 immutable name and kind', () => {
  it('W05-13 gate: name and kind are read-only on edit', () => {
    expect([...LOCKED_ON_EDIT].sort(), 'W05-13 gate: the locked edit fields').toEqual(['kind', 'name'])
  })

  it('W05-13 gate: the update patch never carries name, kind, scope or tenant_id — even when the form changed them', () => {
    const row = aliasOf('a-home', 'home', 'team-chat')
    const r = buildAliasUpdate(row, { ...FORM, name: 'renamed', kind: 'rerank', scope: 'shared', description: 'new' }, false)
    expect(r, 'W05-13 gate: only the mutable change rides').toEqual({ patch: { description: 'new' } })
  })

  it('W05-13: only changed mutable fields ride the patch; nothing changed is a client error', () => {
    const row = aliasOf('a-home', 'home', 'team-chat', { enabled: true })
    expect(buildAliasUpdate(row, { ...FORM, enabled: false }, false), 'disabling sends enabled:false').toEqual({ patch: { enabled: false } })
    expect(buildAliasUpdate(row, { ...FORM }, false), 'no change').toEqual({ error: 'nothing changed' })
  })
})

describe('W05-13 enabled explicit + pool suggestion drafts', () => {
  it('W05-13 gate: every create sends enabled explicitly — false included', () => {
    const on = buildAliasCreate({ ...FORM, enabled: true }, false)
    const off = buildAliasCreate({ ...FORM, enabled: false }, false)
    expect(on, 'positive control: enabled true').toEqual({ spec: { name: 'team-chat', kind: 'chat', role: 'chat', enabled: true, sensitivity: 'credentials' } })
    expect('spec' in off && Object.prototype.hasOwnProperty.call(off.spec, 'enabled') && off.spec.enabled, 'W05-13 gate: enabled:false is on the wire').toBe(false)
    expect('spec' in off && 'enabled' in off.spec, 'W05-13 gate: the key is present').toBe(true)
  })

  it('W05-13 gate: "Suggest from pool" drafts are disabled (enabled:false) and named after the primary model', () => {
    expect(POOL_ROLES, 'the three pool roles').toEqual([
      { role: 'chat', kind: 'chat' },
      { role: 'embed', kind: 'embedding' },
      { role: 'rerank', kind: 'rerank' },
    ])
    expect(suggestDraft('chat', 'chat', 'qwen38-27b'), 'W05-13 gate: a draft').toEqual({ name: 'qwen38-27b', kind: 'chat', role: 'chat', enabled: false })
    expect(suggestDraft('rerank', 'rerank', null), 'an empty pool role suggests nothing (fail-closed)').toBeNull()
  })
})

describe('W05-13 sensitivity downgrade confirm', () => {
  it('W05-13 gate: lowering needs a confirm; keeping or raising does not', () => {
    expect(needsDowngradeConfirm(null, 'credentials'), 'create at credentials: no confirm').toBe(false)
    expect(needsDowngradeConfirm(null, 'personal'), 'W05-13 gate: create below credentials').toBe(true)
    expect(needsDowngradeConfirm('internal', 'public'), 'W05-13 gate: update below the current value').toBe(true)
    expect(needsDowngradeConfirm('internal', 'personal'), 'raising needs no confirm').toBe(false)
    expect(needsDowngradeConfirm('public', 'public'), 'unchanged needs no confirm').toBe(false)
  })

  it('W05-13 gate: an unconfirmed downgrade builds no request; the confirmed one carries confirm_sensitivity_downgrade', () => {
    const low = { ...FORM, sensitivity: 'personal' as const }
    expect('error' in buildAliasCreate(low, false), 'W05-13 gate: unconfirmed ⇒ no spec').toBe(true)
    expect(buildAliasCreate(low, true), 'confirmed ⇒ flag on the wire').toEqual({
      spec: { name: 'team-chat', kind: 'chat', role: 'chat', enabled: true, sensitivity: 'personal', confirm_sensitivity_downgrade: true },
    })
    const row = aliasOf('a-home', 'home', 'team-chat', { sensitivity: 'internal' })
    expect('error' in buildAliasUpdate(row, { ...FORM, sensitivity: 'public' }, false), 'W05-13 gate: update unconfirmed').toBe(true)
    expect(buildAliasUpdate(row, { ...FORM, sensitivity: 'public' }, true)).toEqual({ patch: { sensitivity: 'public', confirm_sensitivity_downgrade: true } })
  })

  it('W05-13: a tenant-admin is offered only values ≥ the current one; a server-admin every value', () => {
    const t = aliasSensitivityOptions(false, null)
    expect(t.map((o) => [o.value, o.disabled]), 'tenant-admin create: credentials only').toEqual([
      ['credentials', false],
      ['personal', true],
      ['internal', true],
      ['public', true],
    ])
    expect(t.filter((o) => o.disabled).every((o) => o.note === 'server-admin only')).toBe(true)
    expect(aliasSensitivityOptions(true, null).every((o) => !o.disabled), 'server-admin: all values').toBe(true)
  })
})

describe('W05-13 policy form', () => {
  it('W05-13: the policy keys per kind (design/02 §3.2)', () => {
    const chat = policyKeys('chat')
    for (const k of ['max_output_tokens', 'default_max_tokens', 'max_n', 'timeout_s', 'reasoning', 'vision', 'context_length']) expect(chat, `chat: ${k}`).toContain(k)
    expect(policyKeys('embedding').some((k) => k.startsWith('embedding.')), 'embedding.* for embedding aliases').toBe(true)
    expect(policyKeys('rerank').some((k) => k.startsWith('rerank.')), 'rerank.* for rerank aliases').toBe(true)
    expect(chat.some((k) => k.startsWith('embedding.') || k.startsWith('rerank.')), 'no foreign keys on chat').toBe(false)
  })

  it('W05-13: a 422 fields array lands at the fields', () => {
    const err = new ApiError(422, 'validation', 'validation failed', null, {
      success: false,
      error: 'validation failed',
      fields: [
        { field: 'policy.max_n', message: 'must be ≥ 1' },
        { field: 'sensitivity', message: 'below the _global floor' },
      ],
    })
    expect(fieldErrors(err)).toEqual({ 'policy.max_n': 'must be ≥ 1', sensitivity: 'below the _global floor' })
    expect(fieldErrors(new ApiError(403, 'forbidden', 'admin key required')), 'no fields ⇒ nothing at a field').toEqual({})
  })
})
