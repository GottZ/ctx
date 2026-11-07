// limits-form.ts gates (RC-D C6 S1, W05-12 — design/05 §7: "tenant-admin:
// Eingabe-Maximum = server(d), kein „unbegrenzt"-Schalter — rot gegen
// Formular mit leer = unbegrenzt; 422 mit Feldnamen erscheint am Feld";
// design/06 §4.3 server(d) = server row ?? tenant default ?? registry,
// V-06-e "eine fehlende Server-Zeile ist nie unbegrenzt"). Red against the
// S1 stubs (null / {}).

import { describe, expect, it } from 'vitest'
import { ApiError } from '../../lib/api'
import type { LimitValues, LimitsOpView } from '../../lib/api/types'
import { LIMIT_DIMENSIONS, limitFieldErrors, limitFieldSpec, serverCeiling, tenantInputError } from './limits-form'

function values(over: Partial<LimitValues> = {}): LimitValues {
  return {
    rpm: null,
    tpm: null,
    max_concurrency: null,
    daily_tokens: null,
    monthly_tokens: null,
    daily_requests: null,
    monthly_cost_usd: null,
    allow_external: null,
    allow_unmetered_external: null,
    ...over,
  }
}

// chat: the consumer has NO server row for rpm (inherits the registry
// default 30) but a server row for max_concurrency (4); the tenant row
// narrows tpm. monthly_tokens: registry 0 = unbounded ⇒ defaults null.
const CHAT: LimitsOpView = {
  op: 'chat',
  server: values({ max_concurrency: 4 }),
  tenant: values({ tpm: 20_000 }),
  defaults: values({ rpm: 30, tpm: 60_000, max_concurrency: 2, daily_tokens: 2_000_000, monthly_cost_usd: 1 }),
  effective: values({ rpm: 30, tpm: 20_000, max_concurrency: 4, daily_tokens: 2_000_000, monthly_cost_usd: 1 }),
  binding: 'consumer',
  tenant_envelope: null,
  instance_envelope: null,
}

describe('serverCeiling = server(d) (W05-12 / V-06-e)', () => {
  it('a missing server row falls back to the defaults — never unbounded', () => {
    expect(serverCeiling(CHAT, 'rpm'), 'gate W05-12: fehlende Server-Zeile = Registry-Default').toBe(30)
  })
  it('a server row wins over the defaults', () => {
    expect(serverCeiling(CHAT, 'max_concurrency'), 'gate W05-12: server row').toBe(4)
  })
  it('only a registry 0 (defaults null for monthly_tokens/daily_requests) is unbounded', () => {
    expect(serverCeiling(CHAT, 'monthly_tokens')).toBeNull()
  })
})

describe('limitFieldSpec — two tiers (W05-12)', () => {
  it('tenant-admin: input maximum = server(d), no "unlimited" option', () => {
    const rpm = limitFieldSpec(CHAT, 'rpm', 'tenant')
    expect(rpm, 'gate W05-12: tenant-admin form spec exists').not.toBeNull()
    expect(rpm?.max, 'gate W05-12: Eingabe-Maximum = server(d)').toBe(30)
    expect(rpm?.unlimitedOption, 'gate W05-12: kein „unbegrenzt"-Schalter').toBe(false)
    expect(limitFieldSpec(CHAT, 'max_concurrency', 'tenant')?.max).toBe(4)
  })

  it('tenant-admin: an empty field means "inherits server(d)", never unlimited (rot gegen leer = unbegrenzt)', () => {
    const rpm = limitFieldSpec(CHAT, 'rpm', 'tenant')
    expect(rpm?.value, 'the tenant row has no rpm').toBeNull()
    expect(rpm?.emptyMeans, 'gate W05-12: empty = inherits').toMatch(/^inherits 30\b/)
    expect(rpm?.emptyMeans ?? '', 'gate W05-12: empty is never "unlimited"').not.toMatch(/unlimited|unbegrenzt|∞/i)
  })

  it('tenant-admin: the current tenant value is the field value', () => {
    expect(limitFieldSpec(CHAT, 'tpm', 'tenant')).toMatchObject({ tier: 'tenant', value: 20_000, max: 60_000 })
  })

  it('every dimension of a tenant-admin form has no unlimited option', () => {
    for (const d of LIMIT_DIMENSIONS) expect(limitFieldSpec(CHAT, d, 'tenant')?.unlimitedOption, d).toBe(false)
  })

  it('server-admin: edits the server row without a client maximum', () => {
    expect(limitFieldSpec(CHAT, 'max_concurrency', 'server'), 'gate W05-12: server tier').toMatchObject({
      tier: 'server',
      value: 4,
      max: null,
    })
  })
})

describe('tenantInputError (UX mirror of the server check)', () => {
  it('a value above server(d) is refused at the field; a value within passes', () => {
    expect(tenantInputError(CHAT, 'rpm', '31'), 'gate W05-12: above the ceiling').toEqual(expect.stringMatching(/30/))
    expect(tenantInputError(CHAT, 'rpm', '30')).toBeNull()
    expect(tenantInputError(CHAT, 'rpm', '')).toBeNull()
  })
  it('zero and negatives are refused (there is no 0 sentinel)', () => {
    expect(tenantInputError(CHAT, 'rpm', '0'), 'gate: kein 0-Sentinel').not.toBeNull()
    expect(tenantInputError(CHAT, 'rpm', '-1')).not.toBeNull()
  })
})

describe('limitFieldErrors (W05-12: 422 with field name lands at the field)', () => {
  it('maps fields[] of a 422 onto "<op>.<field>"', () => {
    const err = new ApiError(422, 'unprocessable', 'limit exceeds the server limit', null, {
      success: false,
      fields: [
        { field: 'max_concurrency', message: 'at most 4 (server limit)' },
        { field: 'rpm', message: 'at most 30 (server limit)' },
      ],
    })
    expect(limitFieldErrors(err, 'chat'), 'gate W05-12: 422 am Feld').toEqual({
      'chat.max_concurrency': 'at most 4 (server limit)',
      'chat.rpm': 'at most 30 (server limit)',
    })
  })
  it('any other refusal maps to no field', () => {
    expect(limitFieldErrors(new ApiError(403, 'forbidden', 'admin key required'), 'chat')).toEqual({})
  })
})
