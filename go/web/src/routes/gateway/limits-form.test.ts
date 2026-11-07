// limits-form.ts gates (RC-D C6 S1, W05-12 — design/05 §7: "tenant-admin:
// Eingabe-Maximum = server(d), kein „unbegrenzt"-Schalter — rot gegen
// Formular mit leer = unbegrenzt; 422 mit Feldnamen erscheint am Feld";
// design/06 §4.3 server(d) = server row ?? tenant default ?? registry,
// V-06-e "eine fehlende Server-Zeile ist nie unbegrenzt"). Red against the
// S1 stubs (null / {}).
// S2 (Go wins, C1 W06-15): server(d) arrives resolved as the op's `ceiling`
// (C1 Snapshot.Ceiling — consumer server row ?? tenant server row ?? setting);
// rows are nullable objects; a field error is 400 {error, param} (one field),
// not 422 fields[].

import { describe, expect, it } from 'vitest'
import { ApiError } from '../../lib/api'
import type { LimitEffective, LimitValues, LimitsOpView } from '../../lib/api/types'
import { LIMIT_DIMENSIONS, limitFieldErrors, limitFieldSpec, serverCeiling, tenantInputError, unsendableInput } from './limits-form'

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

// chat: the consumer's server row says nothing about rpm (the ceiling still
// carries the setting's 30 — a missing server value is never unbounded) but
// sets max_concurrency 4; the tenant row narrows tpm. monthly_tokens and
// daily_requests: settings floor 0 ⇒ ceiling null = unbounded.
const EFFECTIVE: LimitEffective = {
  rpm: 30,
  tpm: 20_000,
  max_concurrency: 4,
  daily_tokens: 2_000_000,
  monthly_tokens: null,
  daily_requests: null,
  monthly_cost_usd: 1,
  allow_external: false,
  allow_unmetered_external: false,
}
const CHAT: LimitsOpView = {
  server: values({ max_concurrency: 4 }),
  tenant: values({ tpm: 20_000 }),
  ceiling: values({ rpm: 30, tpm: 60_000, max_concurrency: 4, daily_tokens: 2_000_000, monthly_cost_usd: 1, allow_external: false }),
  effective: EFFECTIVE,
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

describe('limitFieldErrors (W05-12: a 400 with the field name lands at the field)', () => {
  it('maps the param of a 400 onto "<op>.<param>"', () => {
    const err = new ApiError(400, 'bad_request', 'limit widens: max_concurrency', null, {
      success: false,
      error: 'limit widens: max_concurrency',
      param: 'max_concurrency',
    })
    expect(limitFieldErrors(err, 'chat'), 'gate W05-12: Fehler mit Feldnamen am Feld').toEqual({
      'chat.max_concurrency': 'limit widens: max_concurrency',
    })
  })
  it('any other refusal maps to no field', () => {
    expect(limitFieldErrors(new ApiError(403, 'forbidden', 'admin key required'), 'chat')).toEqual({})
    expect(limitFieldErrors(new ApiError(400, 'bad_request', 'invalid data', null, { success: false }), 'chat')).toEqual({})
  })
})

describe('no ceiling known (fail-closed, S2)', () => {
  it('a tenant-admin field without a ceiling is disabled, never unbounded', () => {
    const spec = limitFieldSpec({ server: null, tenant: null }, 'rpm', 'tenant')
    expect(spec.disabled).toBe(true)
    expect(spec.unlimitedOption).toBe(false)
    expect(tenantInputError({ server: null, tenant: null }, 'rpm', '5')).not.toBeNull()
  })
})

describe('unsendable input (review I4)', () => {
  it('a non-finite number is refused before the wire (JSON would turn it into null and clear the row)', () => {
    expect(unsendableInput('1e400'), 'Infinity never goes out as null').toBe(true)
    expect(unsendableInput('abc')).toBe(true)
    expect(unsendableInput('')).toBe(false)
    expect(unsendableInput(' 5 ')).toBe(false)
  })
})
