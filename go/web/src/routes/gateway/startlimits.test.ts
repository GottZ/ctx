// FOLGE-Z Z4 gates (briefings/FOLGE-Z-INDEX.md §1 C-SPA, §3; dispatch
// dispatch-folge-z-spa.md): no silent value when a model consumer is created.
//   Z4-lane     the lane is preselected visibly per tier: server-admin `urgent`
//               (app), tenant-admin `bulk` with the reason "urgent is
//               server-admin only"
//   Z4-ops      start limits for chat + embed always, rerank only when the
//               catalog offers a rerank alias
//   Z4-source   prefill = gateway-limits-get of the target TENANT (server-admin
//               without a tenant id: the instance; a tenant-admin without one:
//               nothing — fail-closed)
//   Z4-prefill  effective defaults per op/dim; null = unbounded; anything else
//               = not reported (never a guessed number)
//   Z4-request  only CHANGED fields go out as `limits`; a tenant-admin can only
//               narrow (≤ the inherited default)
//   Z4-template "App · Chat" 8/120, "App · Embedding" 4/240 — clamped visibly
//               for a tenant-admin
//   Z4-reveal   effective_limits + lane of the answer; fail-closed "not
//               reported" when the server omits them

import { describe, expect, it } from 'vitest'
import type { ModelAliasView } from '../../lib/api/types'
import { aliasOf } from './forms.fixtures'
import {
  EFFECTIVE_FIELDS,
  START_DIMS,
  TEMPLATE_START,
  laneShareNote,
  tenantCaps,
  applyTemplateStart,
  buildStartLimits,
  createLaneDefault,
  createLaneHint,
  fieldKey,
  limitSourceLabel,
  limitsSubject,
  prefillInputs,
  revealLane,
  revealLimits,
  startDefaults,
  startFieldSpec,
  startOps,
  type StartDefaults,
} from './startlimits'

/** A gateway-limits-get answer (C1 shape) with the given effective values per op. */
function limitsAnswer(eff: Record<string, Record<string, unknown>>): unknown {
  const ops: Record<string, unknown> = {}
  for (const [op, e] of Object.entries(eff)) {
    ops[op] = { server: null, tenant: null, effective: { allow_external: false, allow_unmetered_external: false, ...e }, sources: {} }
  }
  return { success: true, limits: { subject_kind: 'tenant', tenant_id: 't-a', ops } }
}

const SETTINGS_DEFAULTS: StartDefaults = {
  chat: { rpm: 30, tpm: 60_000, max_concurrency: 2, daily_tokens: 2_000_000 },
  embed: { rpm: 120, tpm: 500_000, max_concurrency: 2, daily_tokens: null },
}

describe('FOLGE-Z Z4-lane: the lane of a new app consumer is preselected visibly per tier', () => {
  it('Z4-lane gate: server-admin ⇒ urgent, tenant-admin ⇒ bulk', () => {
    expect(createLaneDefault(true), 'Z4-lane: a server-admin creates app consumers on urgent').toBe('urgent')
    expect(createLaneDefault(false), 'Z4-lane: a tenant-admin may not raise — bulk').toBe('bulk')
  })

  it('Z4-lane gate: a tenant-admin is told why (urgent is server-admin only); a server-admin needs no note', () => {
    expect(createLaneHint(false), 'Z4-lane: the reason is visible to a tenant-admin').toMatch(/urgent is server-admin only/)
    expect(createLaneHint(true)).toBeNull()
  })
})

describe('FOLGE-Z Z4-ops: the ops of the start-limits block', () => {
  const chat = aliasOf('a1', '_global', 'chat')
  const embed = aliasOf('a2', '_global', 'embed', { kind: 'embedding', role: 'embedding' })
  const rerank = aliasOf('a3', '_global', 'rr', { kind: 'rerank', role: 'rerank' })

  it('Z4-ops gate: chat and embed always, rerank only with an offered rerank alias', () => {
    expect(startOps([]), 'Z4-ops: chat + embed without any alias').toEqual(['chat', 'embed'])
    expect(startOps([chat, embed]), 'Z4-ops: no rerank alias ⇒ no rerank block').toEqual(['chat', 'embed'])
    expect(startOps([chat, rerank]), 'Z4-ops: a rerank alias brings the rerank block').toEqual(['chat', 'embed', 'rerank'])
  })

  it('Z4-ops: a disabled rerank alias does not count (offeredAliases already drops it; the kind decides)', () => {
    const off: ModelAliasView = { ...rerank, enabled: false }
    expect(startOps([off])).toEqual(['chat', 'embed'])
  })
})

describe('FOLGE-Z Z4-source: which gateway-limits-get the prefill reads', () => {
  it('Z4-source gate: the target tenant for both tiers', () => {
    expect(limitsSubject(true, 't-b'), 'Z4-source: server-admin reads the chosen tenant').toEqual({ subject_kind: 'tenant', tenant_id: 't-b' })
    expect(limitsSubject(false, 't-a'), 'Z4-source: tenant-admin reads its own tenant').toEqual({ subject_kind: 'tenant', tenant_id: 't-a' })
  })

  it('Z4-source gate: without a tenant id the server-admin reads the instance, a tenant-admin nothing', () => {
    expect(limitsSubject(true, null)).toEqual({ subject_kind: 'instance' })
    expect(limitsSubject(true, '')).toEqual({ subject_kind: 'instance' })
    expect(limitsSubject(false, null), 'Z4-source: no tenant id ⇒ no read (instance is server-admin only)').toBeNull()
  })
})

describe('FOLGE-Z Z4-prefill: effective defaults from the limits answer', () => {
  it('Z4-prefill gate: numbers per op and dimension, null = unbounded', () => {
    const d = startDefaults(
      limitsAnswer({
        chat: { rpm: 30, tpm: 60_000, max_concurrency: 2, daily_tokens: 2_000_000 },
        embed: { rpm: 120, tpm: 500_000, max_concurrency: 2, daily_tokens: null },
      }),
    )
    expect(d.chat, 'Z4-prefill: chat effective values').toEqual({ rpm: 30, tpm: 60_000, max_concurrency: 2, daily_tokens: 2_000_000 })
    expect(d.embed?.daily_tokens, 'Z4-prefill: a null budget is unbounded, kept as null').toBeNull()
  })

  it('Z4-prefill gate: never a guessed number — garbage, 0, a null rate or a missing op is "not reported"', () => {
    const d = startDefaults(limitsAnswer({ chat: { rpm: 0, tpm: '60000', max_concurrency: null, daily_tokens: -1 } }))
    expect(d.chat?.rpm, 'a 0 is no limit (no 0 sentinel)').toBeUndefined()
    expect(d.chat?.tpm, 'a string is not a number').toBeUndefined()
    expect(d.chat?.max_concurrency, 'a rate is never unbounded ⇒ null is not reported').toBeUndefined()
    expect(d.chat?.daily_tokens).toBeUndefined()
    expect(d.embed, 'a missing op').toBeUndefined()
    expect(startDefaults(null)).toEqual({})
    expect(startDefaults({ success: true })).toEqual({})
    expect(startDefaults({ success: true, limits: { ops: [] } })).toEqual({})
  })

  it('Z4-prefill: the inputs carry the defaults as text; unbounded and unknown stay empty', () => {
    const inputs = prefillInputs({ ...SETTINGS_DEFAULTS, rerank: {} }, ['chat', 'embed', 'rerank'])
    expect(inputs[fieldKey('chat', 'rpm')]).toBe('30')
    expect(inputs[fieldKey('chat', 'max_concurrency')]).toBe('2')
    expect(inputs[fieldKey('embed', 'daily_tokens')]).toBe('')
    expect(inputs[fieldKey('rerank', 'rpm')]).toBe('')
    expect(Object.keys(inputs)).toHaveLength(12)
  })

  it('Z4-prefill: field spec — a tenant-admin may only narrow, without a known default the field is locked', () => {
    const t = startFieldSpec(SETTINGS_DEFAULTS, 'chat', 'rpm', false)
    expect(t).toMatchObject({ value: 30, max: 30, disabled: false })
    expect(startFieldSpec(SETTINGS_DEFAULTS, 'chat', 'rpm', true), 'a server-admin has no client bound').toMatchObject({ value: 30, max: null, disabled: false })
    expect(startFieldSpec(SETTINGS_DEFAULTS, 'embed', 'daily_tokens', false), 'unbounded: no client max').toMatchObject({ value: null, max: null, disabled: false })
    expect(startFieldSpec({}, 'chat', 'rpm', false), 'Z4-prefill gate: no known default ⇒ locked for a tenant-admin').toMatchObject({ value: undefined, disabled: true })
    expect(startFieldSpec({}, 'chat', 'rpm', true).disabled, 'a server-admin may still set it').toBe(false)
    expect(startFieldSpec(SETTINGS_DEFAULTS, 'embed', 'daily_tokens', false).placeholder).toMatch(/unbounded/)
    expect(startFieldSpec({}, 'chat', 'rpm', true).placeholder).toMatch(/not reported/)
  })
})

describe('FOLGE-Z Z4-request: only changed fields go out as `limits`', () => {
  const ops = ['chat', 'embed'] as const

  it('Z4-request gate: untouched prefill ⇒ no limits at all', () => {
    const inputs = prefillInputs(SETTINGS_DEFAULTS, ops)
    expect(buildStartLimits({ ops, defaults: SETTINGS_DEFAULTS, inputs, serverAdmin: true })).toEqual({ limits: null })
  })

  it('Z4-request gate: a changed field goes out under its op; equal-to-default and empty fields do not', () => {
    const inputs = { ...prefillInputs(SETTINGS_DEFAULTS, ops), [fieldKey('chat', 'max_concurrency')]: '8', [fieldKey('chat', 'rpm')]: ' 120 ', [fieldKey('embed', 'tpm')]: '' }
    expect(buildStartLimits({ ops, defaults: SETTINGS_DEFAULTS, inputs, serverAdmin: true })).toEqual({ limits: { chat: { max_concurrency: 8, rpm: 120 } } })
  })

  it('Z4-request gate: a tenant-admin may narrow, never widen past the inherited default', () => {
    const narrow = { ...prefillInputs(SETTINGS_DEFAULTS, ops), [fieldKey('chat', 'rpm')]: '10' }
    expect(buildStartLimits({ ops, defaults: SETTINGS_DEFAULTS, inputs: narrow, serverAdmin: false })).toEqual({ limits: { chat: { rpm: 10 } } })
    const wide = { ...prefillInputs(SETTINGS_DEFAULTS, ops), [fieldKey('chat', 'rpm')]: '31' }
    const r = buildStartLimits({ ops, defaults: SETTINGS_DEFAULTS, inputs: wide, serverAdmin: false })
    expect(r, 'Z4-request gate: widening is refused client-side for a tenant-admin').toMatchObject({ field: 'chat.rpm' })
    expect('error' in r && r.error).toMatch(/at most 30/)
    expect(buildStartLimits({ ops, defaults: SETTINGS_DEFAULTS, inputs: wide, serverAdmin: true }), 'a server-admin is free').toEqual({ limits: { chat: { rpm: 31 } } })
  })

  it('Z4-request: an unbounded default bounds nothing; an unknown default refuses a tenant-admin value', () => {
    const inputs = { ...prefillInputs(SETTINGS_DEFAULTS, ops), [fieldKey('embed', 'daily_tokens')]: '9000000' }
    expect(buildStartLimits({ ops, defaults: SETTINGS_DEFAULTS, inputs, serverAdmin: false })).toEqual({ limits: { embed: { daily_tokens: 9_000_000 } } })
    const unknown = buildStartLimits({ ops: ['chat'], defaults: {}, inputs: { [fieldKey('chat', 'rpm')]: '5' }, serverAdmin: false })
    expect(unknown).toMatchObject({ field: 'chat.rpm' })
    expect(buildStartLimits({ ops: ['chat'], defaults: {}, inputs: { [fieldKey('chat', 'rpm')]: '5' }, serverAdmin: true })).toEqual({ limits: { chat: { rpm: 5 } } })
  })

  it('Z4-request gate: > 0 and whole numbers only (no 0 sentinel, nothing JSON turns into null)', () => {
    for (const raw of ['0', '-3', '1.5', 'abc', '1e400']) {
      const r = buildStartLimits({ ops: ['chat'], defaults: SETTINGS_DEFAULTS, inputs: { [fieldKey('chat', 'tpm')]: raw }, serverAdmin: true })
      expect(r, `"${raw}" is refused`).toMatchObject({ field: 'chat.tpm' })
    }
  })

  it('Z4-request: only the ops of the block — a stale rerank input never rides along', () => {
    const inputs = { [fieldKey('rerank', 'rpm')]: '7' }
    expect(buildStartLimits({ ops: ['chat', 'embed'], defaults: SETTINGS_DEFAULTS, inputs, serverAdmin: true })).toEqual({ limits: null })
  })
})

describe('FOLGE-Z Z4-template: templates set concurrency and RPM', () => {
  it('Z4-template gate: App · Chat = 8 / 120, App · Embedding = 4 / 240', () => {
    expect(TEMPLATE_START['app-chat']).toEqual({ op: 'chat', values: { max_concurrency: 8, rpm: 120 } })
    expect(TEMPLATE_START['app-embedding']).toEqual({ op: 'embed', values: { max_concurrency: 4, rpm: 240 } })
    const r = applyTemplateStart('app-chat', prefillInputs(SETTINGS_DEFAULTS, ['chat', 'embed']), SETTINGS_DEFAULTS, true)
    expect(r.inputs[fieldKey('chat', 'max_concurrency')]).toBe('8')
    expect(r.inputs[fieldKey('chat', 'rpm')]).toBe('120')
    expect(r.inputs[fieldKey('embed', 'rpm')], 'the other op stays').toBe('120')
    expect(r.notes).toEqual({})
  })

  it('Z4-template gate: a tenant-admin gets the template clamped to its limit — with a visible note', () => {
    const r = applyTemplateStart('app-chat', prefillInputs(SETTINGS_DEFAULTS, ['chat', 'embed']), SETTINGS_DEFAULTS, false)
    expect(r.inputs[fieldKey('chat', 'max_concurrency')]).toBe('2')
    expect(r.inputs[fieldKey('chat', 'rpm')]).toBe('30')
    expect(r.notes[fieldKey('chat', 'max_concurrency')], 'Z4-template: the clamp is said, not silent').toMatch(/template asks 8.*allows 2/)
    expect(r.notes[fieldKey('chat', 'rpm')]).toMatch(/template asks 120.*allows 30/)
  })

  it('Z4-template: within the limit a tenant-admin gets the template value; an unknown template changes nothing', () => {
    const roomy: StartDefaults = { embed: { rpm: 600, max_concurrency: 6 } }
    const r = applyTemplateStart('app-embedding', {}, roomy, false)
    expect(r.inputs[fieldKey('embed', 'max_concurrency')]).toBe('4')
    expect(r.inputs[fieldKey('embed', 'rpm')]).toBe('240')
    expect(r.notes).toEqual({})
    const same = { a: 'b' }
    expect(applyTemplateStart('nope', same, roomy, true)).toEqual({ inputs: same, notes: {} })
  })
})

describe('FOLGE-Z Z4-reveal: effective limits and lane after the create', () => {
  const eff = {
    chat: {
      rpm: 120, tpm: 60_000, max_concurrency: 8, daily_tokens: null, monthly_tokens: null, daily_requests: null, monthly_cost_usd: 1,
      allow_external: false, allow_unmetered_external: false,
      sources: {
        rpm: 'consumer_server', tpm: 'settings', max_concurrency: 'consumer_server', daily_tokens: 'settings', monthly_tokens: 'settings',
        daily_requests: 'settings', monthly_cost_usd: 'settings', allow_external: 'settings', allow_unmetered_external: 'default',
      },
    },
    embed: {
      rpm: 120, tpm: 500_000, max_concurrency: 2, daily_tokens: 20_000_000, monthly_tokens: null, daily_requests: null, monthly_cost_usd: 1,
      allow_external: false, allow_unmetered_external: false,
      sources: { rpm: 'tenant_server', tpm: 'instance', max_concurrency: 'settings', daily_tokens: 'settings' },
    },
  }

  it('Z4-reveal gate: every start dimension of every block op with its source', () => {
    const r = revealLimits(eff, ['chat', 'embed'])
    expect(r.reported, 'Z4-reveal: the answer carries effective_limits').toBe(true)
    if (!r.reported) return
    const chat = r.ops.find((o) => o.op === 'chat')
    expect(chat?.reported).toBe(true)
    if (!chat?.reported) return
    expect(chat.dims.map((d) => d.dim), 'H2 (review 2): every §3 E field, not only the four start dimensions').toEqual([...EFFECTIVE_FIELDS])
    expect(chat.dims.find((d) => d.dim === 'max_concurrency')).toEqual({ dim: 'max_concurrency', text: '8', source: 'consumer row (server-admin)' })
    expect(chat.dims.find((d) => d.dim === 'tpm')).toEqual({ dim: 'tpm', text: '60,000', source: 'from settings' })
    expect(chat.dims.find((d) => d.dim === 'daily_tokens'), 'null budget = unbounded').toEqual({ dim: 'daily_tokens', text: 'unbounded', source: 'from settings' })
    expect(chat.dims.find((d) => d.dim === 'monthly_cost_usd')?.text, 'H2: the cost budget').toBe('$1')
    expect(chat.dims.find((d) => d.dim === 'allow_external'), 'H2: a new consumer may not route externally — said').toEqual({ dim: 'allow_external', text: 'no', source: 'from settings' })
    expect(chat.dims.find((d) => d.dim === 'allow_unmetered_external')?.source).toBe('server default')
  })

  it('Z4-reveal gate: fail-closed — no effective_limits ⇒ not reported, never a number', () => {
    expect(revealLimits(undefined, ['chat'])).toEqual({ reported: false })
    expect(revealLimits(null, ['chat'])).toEqual({ reported: false })
    expect(revealLimits('x', ['chat'])).toEqual({ reported: false })
  })

  it('Z4-reveal gate: a missing op or a broken value is "not reported" at that place', () => {
    const r = revealLimits({ chat: { ...eff.chat, rpm: 'lots', tpm: null } }, ['chat', 'rerank'])
    if (!r.reported) throw new Error('Z4-reveal: the object is reported')
    expect(r.ops.find((o) => o.op === 'rerank'), 'Z4-reveal: missing op').toEqual({ op: 'rerank', reported: false })
    const chat = r.ops.find((o) => o.op === 'chat')
    if (!chat?.reported) throw new Error('chat reported')
    expect(chat.dims.find((d) => d.dim === 'rpm')?.text).toBe('not reported')
    expect(chat.dims.find((d) => d.dim === 'tpm')?.text, 'a rate is never unbounded').toBe('not reported')
    const odd = revealLimits({ chat: { ...eff.chat, allow_external: 'yes', monthly_cost_usd: 0 } }, ['chat'])
    if (!odd.reported || !odd.ops[0].reported) throw new Error('reported')
    expect(odd.ops[0].dims.find((d) => d.dim === 'allow_external')?.text, 'H2: a non-boolean permission').toBe('not reported')
    expect(odd.ops[0].dims.find((d) => d.dim === 'monthly_cost_usd')?.text, 'H2: a 0 cost is no budget value').toBe('not reported')
  })

  it('Z4-reveal: source labels — the §3 vocabulary, raw text for anything newer, "not reported" for nothing', () => {
    expect(limitSourceLabel('settings')).toBe('from settings')
    expect(limitSourceLabel('default')).toBe('server default')
    expect(limitSourceLabel('instance')).toBe('instance row')
    expect(limitSourceLabel('tenant_server')).toBe('tenant row (server-admin)')
    expect(limitSourceLabel('tenant_tenant')).toBe('tenant row (tenant-admin)')
    expect(limitSourceLabel('consumer_server')).toBe('consumer row (server-admin)')
    expect(limitSourceLabel('consumer_tenant')).toBe('consumer row (tenant-admin)')
    expect(limitSourceLabel('tenant_envelope')).toBe('source: tenant_envelope')
    expect(limitSourceLabel(undefined)).toBe('source not reported')
    expect(limitSourceLabel(3)).toBe('source not reported')
  })

  it('Z4-reveal gate: the lane of the answer — only the closed vocabulary', () => {
    expect(revealLane('urgent')).toBe('urgent')
    expect(revealLane('bulk')).toBe('bulk')
    expect(revealLane(undefined)).toBe('not reported')
    expect(revealLane('interactive'), 'a queue name is no lane').toBe('not reported')
  })
})

describe('FOLGE-Z hardening after the external review', () => {
  it('H4 (review 6): only plain whole numbers within the safe range — no hex, no exponent, no silent rounding', () => {
    for (const raw of ['0x10', '1e3', '9007199254740993', '+5', '5 5']) {
      const r = buildStartLimits({ ops: ['chat'], defaults: SETTINGS_DEFAULTS, inputs: { [fieldKey('chat', 'tpm')]: raw }, serverAdmin: true })
      expect(r, `"${raw}" is refused`).toMatchObject({ field: 'chat.tpm' })
    }
    expect(buildStartLimits({ ops: ['chat'], defaults: SETTINGS_DEFAULTS, inputs: { [fieldKey('chat', 'tpm')]: ' 70000 ' }, serverAdmin: true })).toEqual({ limits: { chat: { tpm: 70_000 } } })
  })

  it('H3 (review 3): the tenant\'s own row caps a dimension — read from the sources of the limits answer', () => {
    const answer = {
      success: true,
      limits: {
        subject_kind: 'tenant',
        ops: {
          chat: { effective: { rpm: 30, tpm: 60_000, max_concurrency: 2, daily_tokens: null }, sources: { rpm: 'settings', tpm: 'settings' } },
          embed: { effective: { rpm: 60, tpm: 500_000, max_concurrency: 2, daily_tokens: null }, sources: { rpm: 'tenant_tenant', tpm: 'tenant_server' } },
        },
      },
    }
    expect(tenantCaps(answer), 'H3: only tenant_tenant narrows a server-admin consumer value').toEqual({ embed: { rpm: 60 } })
    expect(tenantCaps(null)).toEqual({})
  })

  it('H9 (review 1): bulk is admitted only up to 1 − gateway.xu_reserve_ratio of each limit — said, never computed', () => {
    expect(laneShareNote('bulk')).toMatch(/xu_reserve_ratio/)
    expect(laneShareNote('bulk')).toMatch(/at least 1/)
    expect(laneShareNote('urgent')).toBeNull()
  })

  it('H6 (review 9): the field spec IS the client rule — a tenant-admin value above spec.max is refused, a locked field too', () => {
    const spec = startFieldSpec(SETTINGS_DEFAULTS, 'chat', 'max_concurrency', false)
    const over = buildStartLimits({ ops: ['chat'], defaults: SETTINGS_DEFAULTS, inputs: { [fieldKey('chat', 'max_concurrency')]: String((spec.max ?? 0) + 1) }, serverAdmin: false })
    expect(over).toMatchObject({ field: 'chat.max_concurrency' })
  })
})
