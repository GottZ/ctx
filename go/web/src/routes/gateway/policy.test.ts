// W05-07 — policy edit with field-tier UX (design/05 §4.5, §4.2 row "Policy
// ändern"; RC-D C5). The SPA mirrors the field tiers of
// internal/handler/model_consumers.go — red against a dialog that offers
// every option to every caller:
//   - a tenant-admin sees the sensitivities BELOW the current one disabled
//     ("server-admin only"), raising stays allowed; a server-admin picks all;
//   - without mintModelKeys (self-service off) widening is disabled: unchecked
//     models, "all offered", a new / changed scope, dropping the allowlist or
//     prefixes outside it — narrowing stays allowed;
//   - a ctx-peer row carries no edit control below server-admin;
//   - the patch carries ONLY changed fields, never kind / tenant_id /
//     expires_at, and a change the tiers forbid never reaches the wire.

import { describe, expect, it } from 'vitest'
import { buildUpdatePatch, canEditPolicy, cidrsWiden, editFormFrom, editTiers, type EditCaps, type EditFormInput } from './policy'
import { consumerOf } from './forms.fixtures'

const TENANT_SELF: EditCaps = { serverAdmin: false, mintModelKeys: true }
const TENANT_NO_SELF: EditCaps = { serverAdmin: false, mintModelKeys: false }
const SERVER: EditCaps = { serverAdmin: true, mintModelKeys: true }

const sensOf = (tiers: ReturnType<typeof editTiers>): Record<string, { disabled: boolean; note?: string }> =>
  Object.fromEntries(tiers.sensitivity.map((o) => [o.value, o]))

describe('W05-07 edit control per row', () => {
  it('W05-07 gate: a tenant-admin edits its own app consumer, never a ctx-peer row; a server-admin edits both', () => {
    const app = consumerOf('ci-chat')
    const peer = consumerOf('peer-b', { kind: 'ctx-peer' })
    expect(canEditPolicy(app, TENANT_SELF), 'positive control: own app consumer is editable').toBe(true)
    expect(canEditPolicy(app, TENANT_NO_SELF), 'narrowing needs no self-service: still editable').toBe(true)
    expect(canEditPolicy(peer, TENANT_SELF), 'W05-07 gate: no edit control on a ctx-peer row below server-admin').toBe(false)
    expect(canEditPolicy(peer, SERVER), 'server-admin edits a peer').toBe(true)
    expect(editTiers(peer, TENANT_SELF).locked, 'peer tiers are locked below server-admin').toBe(true)
    expect(editTiers(app, TENANT_SELF).locked, 'positive control: app tiers are not locked').toBe(false)
  })
})

describe('W05-07 sensitivity tier', () => {
  it('W05-07 gate: a tenant-admin sees the lower sensitivities disabled ("server-admin only")', () => {
    const s = sensOf(editTiers(consumerOf('a'), TENANT_SELF))
    expect(Object.keys(s), 'all four values are listed').toEqual(['credentials', 'personal', 'internal', 'public'])
    expect(s.credentials?.disabled, 'the current value stays selectable').toBe(false)
    for (const v of ['personal', 'internal', 'public']) {
      expect(s[v]?.disabled, `W05-07 gate: lowering credentials → ${v} is server-admin only`).toBe(true)
      expect(s[v]?.note).toBe('server-admin only')
    }
  })

  it('W05-07: a tenant-admin may raise from internal (credentials / personal / internal open, public disabled)', () => {
    const s = sensOf(editTiers(consumerOf('a', { prompt_sensitivity: 'internal' }), TENANT_NO_SELF))
    for (const v of ['credentials', 'personal', 'internal']) expect(s[v]?.disabled, `raise/keep ${v}`).toBe(false)
    expect(s.public?.disabled, 'lowering internal → public is server-admin only').toBe(true)
  })

  it('W05-07: a server-admin picks every sensitivity', () => {
    const s = sensOf(editTiers(consumerOf('a'), SERVER))
    expect(Object.keys(s)).toEqual(['credentials', 'personal', 'internal', 'public'])
    for (const v of Object.keys(s)) expect(s[v]?.disabled, `server-admin: ${v}`).toBe(false)
  })
})

describe('W05-07 widening tier (mintModelKeys)', () => {
  it('W05-07 gate: without mintModelKeys every widening control is disabled, with it they open', () => {
    const c = consumerOf('a', { allowed_cidrs: ['10.0.0.0/8'] })
    const with_ = editTiers(c, TENANT_SELF)
    expect([with_.mayWidenModels, with_.mayEnableAllModels, with_.mayChangeScope, with_.mayWidenCidrs], 'positive control: self-service widens').toEqual([true, true, true, true])
    const without = editTiers(c, TENANT_NO_SELF)
    expect(without.mayWidenModels, 'W05-07 gate: model widening disabled without mintModelKeys').toBe(false)
    expect(without.mayEnableAllModels, 'W05-07 gate: "all offered" disabled without mintModelKeys').toBe(false)
    expect(without.mayChangeScope, 'W05-07 gate: scope change disabled without mintModelKeys').toBe(false)
    expect(without.mayWidenCidrs, 'W05-07 gate: CIDR widening disabled without mintModelKeys').toBe(false)
    const sa = editTiers(c, SERVER)
    expect([sa.mayWidenModels, sa.mayEnableAllModels, sa.mayChangeScope, sa.mayWidenCidrs], 'server-admin widens').toEqual([true, true, true, true])
  })

  it('W05-07: cidrsWiden mirrors the handler (null over a list widens, a list over null narrows, outside prefixes widen)', () => {
    expect(cidrsWiden(['10.0.0.0/8'], ['10.1.0.0/16']), 'a sub-prefix narrows').toBe(false)
    expect(cidrsWiden(['10.0.0.0/8'], ['10.0.0.0/8']), 'the same list does not widen').toBe(false)
    expect(cidrsWiden(['10.0.0.0/8'], ['192.168.0.0/16']), 'a prefix outside the stored list widens').toBe(true)
    expect(cidrsWiden(['10.1.0.0/16'], ['10.0.0.0/8']), 'a super-prefix widens').toBe(true)
    expect(cidrsWiden(['10.0.0.0/8'], null), 'dropping the allowlist (any source) widens').toBe(true)
    expect(cidrsWiden(null, ['10.0.0.0/8']), 'a list over any-source narrows').toBe(false)
    expect(cidrsWiden(null, null), 'any-source over any-source does not widen').toBe(false)
  })
})

function form(c: ReturnType<typeof consumerOf>, p: Partial<EditFormInput> = {}): EditFormInput {
  return { ...editFormFrom(c), ...p }
}

describe('W05-07 update patch', () => {
  it('W05-07: the form opens with the consumer values', () => {
    const c = consumerOf('ci-chat', { description: 'd', models: ['m-b', 'm-a'], admission_lane: 'urgent', allowed_cidrs: ['10.0.0.0/8', '192.168.1.0/24'], prompt_sensitivity: 'personal' })
    expect(editFormFrom(c)).toEqual({
      label: 'ci-chat',
      description: 'd',
      scope: 'home',
      allModels: false,
      models: ['m-b', 'm-a'],
      sensitivity: 'personal',
      lane: 'urgent',
      cidrsText: '10.0.0.0/8\n192.168.1.0/24',
    })
  })

  it('W05-07: only changed fields ride the patch; models compare as sets; nothing changed is a client error', () => {
    const c = consumerOf('ci-chat', { models: ['m-a', 'm-b'] })
    const tiers = editTiers(c, TENANT_SELF)
    const label = buildUpdatePatch(c, form(c, { label: 'ci-chat-2' }), tiers)
    expect(label, 'a label change sends exactly {label}').toEqual({ patch: { label: 'ci-chat-2' } })
    expect(buildUpdatePatch(c, form(c, { models: ['m-b', 'm-a'] }), tiers), 'the same set in another order is no change').toEqual({ error: 'nothing changed' })
    const narrowed = buildUpdatePatch(c, form(c, { models: ['m-b'] }), editTiers(c, TENANT_NO_SELF))
    expect(narrowed, 'removing a model is narrowing — allowed without self-service, sorted ids').toEqual({ patch: { models: ['m-b'] } })
  })

  it('W05-07 gate: a forbidden change never reaches the wire (tenant-admin lowering, widening without self-service)', () => {
    const c = consumerOf('ci-chat', { allowed_cidrs: ['10.0.0.0/8'] })
    // positive control: an allowed change builds a patch
    expect('patch' in buildUpdatePatch(c, form(c, { sensitivity: 'credentials', description: 'x' }), editTiers(c, TENANT_SELF)), 'positive control').toBe(true)
    const lower = buildUpdatePatch(c, form(c, { sensitivity: 'public' }), editTiers(c, TENANT_SELF))
    expect('error' in lower && lower.error, 'W05-07 gate: lowering is refused client-side').toContain('server-admin only')
    const noSelf = editTiers(c, TENANT_NO_SELF)
    for (const [what, p] of [
      ['a new model', { models: ['m-chat', 'm-new'] }],
      ['all offered', { allModels: true }],
      ['a scope change', { scope: 'shared' }],
      ['dropping the allowlist', { cidrsText: '' }],
      ['an outside prefix', { cidrsText: '192.168.0.0/16' }],
    ] as const) {
      const r = buildUpdatePatch(c, form(c, p), noSelf)
      expect('error' in r, `W05-07 gate: ${what} without self-service is refused before the request`).toBe(true)
    }
  })

  it('W05-07: the patch never carries kind, tenant_id or expires_at', () => {
    const c = consumerOf('ci-chat')
    const r = buildUpdatePatch(c, form(c, { label: 'x', description: 'y', sensitivity: 'credentials' }), editTiers(c, SERVER))
    expect('patch' in r, 'positive control: a patch is built').toBe(true)
    if ('patch' in r) for (const k of ['kind', 'tenant_id', 'expires_at', 'id']) expect(Object.keys(r.patch), `no ${k}`).not.toContain(k)
  })

  it('W05-07: a server-admin may lower and raise the lane in one patch', () => {
    const c = consumerOf('ci-chat')
    const r = buildUpdatePatch(c, form(c, { sensitivity: 'public', lane: 'urgent' }), editTiers(c, SERVER))
    expect(r).toEqual({ patch: { prompt_sensitivity: 'public', admission_lane: 'urgent' } })
  })
})
