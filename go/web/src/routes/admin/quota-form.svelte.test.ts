// QuotaForm logic gates (Wave A4): the null↔unlimited mapping is the contract —
// a blank limit field is `null` (unlimited), a number is itself; the on_exceed
// domain is exactly {external_off, block} with external_off the default; scope is
// a hard precondition of the set payload. Pure node, no DOM.

import { describe, expect, it } from 'vitest'
import type { TenantQuotaView } from '../../lib/api/types'
import {
  ON_EXCEED_OPTIONS,
  fieldsFromView,
  isOnExceed,
  limitToField,
  parseLimit,
  toQuotaSpec,
  type QuotaFormFields,
} from './quota-form.svelte'

function fields(p: Partial<QuotaFormFields> = {}): QuotaFormFields {
  return {
    enabled: true,
    dailyCost: '',
    monthlyCost: '',
    dailyCalls: '',
    onExceed: 'external_off',
    ...p,
  }
}

describe('parseLimit (null ↔ unlimited)', () => {
  it('maps a blank or whitespace field to null (unlimited dimension)', () => {
    expect(parseLimit('')).toBeNull()
    expect(parseLimit('   ')).toBeNull()
  })

  it('maps a numeric field to that number, never to 0 for blank', () => {
    expect(parseLimit('5')).toBe(5)
    expect(parseLimit('5.5')).toBe(5.5)
    expect(parseLimit('0')).toBe(0) // an explicit 0 is a real 0, distinct from blank→null
  })

  it('rejects a non-numeric or negative entry', () => {
    expect(() => parseLimit('abc')).toThrow()
    expect(() => parseLimit('-1')).toThrow()
  })

  it('rejects a fractional value when integer is required (daily_calls)', () => {
    expect(parseLimit('10', { integer: true })).toBe(10)
    expect(() => parseLimit('10.5', { integer: true })).toThrow()
  })
})

describe('limitToField (inverse)', () => {
  it('renders null/undefined as the empty string and a number as its decimal', () => {
    expect(limitToField(null)).toBe('')
    expect(limitToField(undefined)).toBe('')
    expect(limitToField(5)).toBe('5')
    expect(limitToField(0)).toBe('0')
  })
})

describe('on_exceed domain', () => {
  it('is exactly {external_off, block} with external_off first (the default)', () => {
    expect(ON_EXCEED_OPTIONS).toEqual(['external_off', 'block'])
    expect(ON_EXCEED_OPTIONS[0]).toBe('external_off')
  })

  it('isOnExceed accepts only the two members', () => {
    expect(isOnExceed('external_off')).toBe(true)
    expect(isOnExceed('block')).toBe(true)
    expect(isOnExceed('nonsense')).toBe(false)
    expect(isOnExceed(undefined)).toBe(false)
  })
})

describe('fieldsFromView (seed)', () => {
  it('seeds a nil-policy view as all-blank, enabled off', () => {
    const v: TenantQuotaView = { scope: 'work', enabled: false, unlimited: true }
    expect(fieldsFromView(v)).toEqual(fields({ enabled: false }))
  })

  it('seeds a real policy, mapping null dimensions back to blank', () => {
    const v: TenantQuotaView = {
      scope: 'work',
      enabled: true,
      daily_cost_usd: 5,
      monthly_cost_usd: null,
      daily_calls: 100,
      on_exceed: 'block',
    }
    expect(fieldsFromView(v)).toEqual(
      fields({ dailyCost: '5', monthlyCost: '', dailyCalls: '100', onExceed: 'block' }),
    )
  })

  it('defaults an out-of-domain/absent on_exceed to external_off', () => {
    const v = { scope: 'work', enabled: true, on_exceed: 'weird' } as unknown as TenantQuotaView
    expect(fieldsFromView(v).onExceed).toBe('external_off')
  })
})

describe('toQuotaSpec', () => {
  it('requires a non-empty scope', () => {
    expect(() => toQuotaSpec('', fields())).toThrow(/scope is required/)
    expect(() => toQuotaSpec('   ', fields())).toThrow(/scope is required/)
  })

  it('collapses blank limit fields to null (unlimited) and carries enabled + on_exceed', () => {
    const spec = toQuotaSpec('work', fields({ enabled: false, onExceed: 'block' }))
    expect(spec).toEqual({
      scope: 'work',
      enabled: false,
      daily_cost_usd: null,
      monthly_cost_usd: null,
      daily_calls: null,
      on_exceed: 'block',
    })
  })

  it('passes numeric limits through, parsing daily_calls as an integer', () => {
    const spec = toQuotaSpec('work', fields({ dailyCost: '12.5', monthlyCost: '300', dailyCalls: '1000' }))
    expect(spec.daily_cost_usd).toBe(12.5)
    expect(spec.monthly_cost_usd).toBe(300)
    expect(spec.daily_calls).toBe(1000)
    expect(spec.scope).toBe('work')
  })

  it('rejects a fractional daily_calls', () => {
    expect(() => toQuotaSpec('work', fields({ dailyCalls: '5.5' }))).toThrow()
  })
})
