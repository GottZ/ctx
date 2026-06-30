// Pure helpers of TenantCreateDialog (FE-A3a) — the slug pre-validation that
// mirrors the server slugPattern (tenant_manage.go:46) and the optional cap-field
// parser. Node-only, no DOM (design 04 §5.5), like ConfirmDialog's slug-match.

import { describe, expect, it } from 'vitest'
import { parseLimit, validSlug } from './TenantCreateDialog.svelte'

describe('validSlug', () => {
  it('accepts lowercase alnum with internal hyphen', () => {
    expect(validSlug('acme')).toBe(true)
    expect(validSlug('acme-research')).toBe(true)
    expect(validSlug('a1')).toBe(true)
    expect(validSlug('x')).toBe(true)
  })

  it('rejects empty, uppercase, and the prefix-corrupting chars (: / space / _)', () => {
    expect(validSlug('')).toBe(false)
    expect(validSlug('Acme')).toBe(false)
    expect(validSlug('acme:research')).toBe(false)
    expect(validSlug('acme research')).toBe(false)
    expect(validSlug('acme_research')).toBe(false)
  })

  it('rejects leading/trailing hyphen and over-length (>24)', () => {
    expect(validSlug('-acme')).toBe(false)
    expect(validSlug('acme-')).toBe(false)
    expect(validSlug('a'.repeat(24))).toBe(true)
    expect(validSlug('a'.repeat(25))).toBe(false)
  })
})

describe('parseLimit', () => {
  it('empty → undefined (omit → server default)', () => {
    expect(parseLimit('')).toBeUndefined()
    expect(parseLimit('   ')).toBeUndefined()
  })

  it('a positive whole number parses', () => {
    expect(parseLimit('25')).toBe(25)
    expect(parseLimit(' 50 ')).toBe(50)
    expect(parseLimit('1')).toBe(1)
  })

  it('non-integer / zero / negative → invalid sentinel', () => {
    expect(parseLimit('0')).toBe('invalid')
    expect(parseLimit('-3')).toBe('invalid')
    expect(parseLimit('2.5')).toBe('invalid')
    expect(parseLimit('abc')).toBe('invalid')
  })
})
