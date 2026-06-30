// tenant-limit pure logic (FE-M3): the null↔unlimited mapping + validation
// (blank→null, 0 distinct from blank, negative/non-numeric/fractional/over-range
// throw) and the field↔spec round-trip stability.

import { describe, expect, it } from 'vitest'
import {
  fieldsFromLimits,
  MAX_TENANT_LIMIT,
  parseTenantLimit,
  tenantLimitToField,
  toTenantLimitSpec,
} from './tenant-limit.svelte'

describe('parseTenantLimit', () => {
  it('blank / whitespace → null (unlimited)', () => {
    expect(parseTenantLimit('')).toBeNull()
    expect(parseTenantLimit('   ')).toBeNull()
  })

  it('"0" → 0 (distinct from blank), a positive int → that number', () => {
    expect(parseTenantLimit('0')).toBe(0)
    expect(parseTenantLimit('25')).toBe(25)
    expect(parseTenantLimit(' 50 ')).toBe(50)
  })

  it('exactly MAX_TENANT_LIMIT is accepted, one over throws', () => {
    expect(parseTenantLimit(String(MAX_TENANT_LIMIT))).toBe(MAX_TENANT_LIMIT)
    expect(() => parseTenantLimit(String(MAX_TENANT_LIMIT + 1))).toThrow(/at most/)
  })

  it('negative / non-numeric / fractional throw', () => {
    expect(() => parseTenantLimit('-1')).toThrow(/non-negative/)
    expect(() => parseTenantLimit('abc')).toThrow(/non-negative/)
    expect(() => parseTenantLimit('2.5')).toThrow(/whole number/)
  })
})

describe('tenantLimitToField', () => {
  it('null/undefined (unlimited) → empty string; a number → its decimal string', () => {
    expect(tenantLimitToField(null)).toBe('')
    expect(tenantLimitToField(undefined)).toBe('')
    expect(tenantLimitToField(0)).toBe('0')
    expect(tenantLimitToField(25)).toBe('25')
  })
})

describe('round-trip + spec', () => {
  it('fieldsFromLimits ∘ toTenantLimitSpec is stable (unlimited + capped)', () => {
    const stored = { max_scopes: null, max_keys: 50 }
    const fields = fieldsFromLimits(stored)
    expect(fields).toEqual({ maxScopes: '', maxKeys: '50' })
    expect(toTenantLimitSpec(fields)).toEqual({ max_scopes: null, max_keys: 50 })
  })

  it('toTenantLimitSpec collapses blanks to null and keeps both dimensions', () => {
    expect(toTenantLimitSpec({ maxScopes: '10', maxKeys: '' })).toEqual({ max_scopes: 10, max_keys: null })
  })

  it('an invalid field throws before producing a spec', () => {
    expect(() => toTenantLimitSpec({ maxScopes: '-5', maxKeys: '10' })).toThrow()
  })
})
