// Write-scope derivation pins (design 04 §5.3 / N3, wave U06). Fail-closed
// matrix — mirrors writableBlockScopes (home ∪ shared-if-granted). DOM-free.

import { describe, expect, it } from 'vitest'
import { canWriteScope } from './writable'

const HOME = { homeScope: 'acme:main', readScopes: ['acme:main', 'shared'] }

describe('canWriteScope — the write-scope politik (N3)', () => {
  it('allows the caller home_scope', () => {
    expect(canWriteScope('acme:main', HOME)).toBe(true)
  })

  it("allows 'shared' when the key is granted it", () => {
    expect(canWriteScope('shared', HOME)).toBe(true)
  })

  it("denies 'shared' when the key is NOT granted it", () => {
    expect(canWriteScope('shared', { homeScope: 'home', readScopes: ['home'] })).toBe(false)
  })

  it('denies a foreign read-only scope (deep link into a scope one cannot write)', () => {
    // The §5.5 deep-link-read-only case: reachable via read_scopes, still read-only.
    expect(canWriteScope('globex:main', HOME)).toBe(false)
  })
})

describe('canWriteScope — fail-closed edges', () => {
  it('denies when home_scope is not yet resolved (whoami pending)', () => {
    expect(canWriteScope('acme:main', { homeScope: null, readScopes: [] })).toBe(false)
  })

  it('denies an empty / undefined scope', () => {
    expect(canWriteScope('', HOME)).toBe(false)
    expect(canWriteScope(undefined, HOME)).toBe(false)
    expect(canWriteScope(null, HOME)).toBe(false)
  })
})
