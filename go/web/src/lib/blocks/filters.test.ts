// Pins the block-workbench filter model (plan §W2): defaultFilters is empty,
// toSearchParams mirrors active facets (category single, tags array) and OMITS
// defaults, and NEVER emits a scope/sensitivity request field (the /api/search
// contract has neither — scope is a client-side facet only). Mirrors
// lib/graph/filters.test.ts.

import { describe, expect, it } from 'vitest'
import { defaultFilters, isDefault, scopeVisible, toSearchParams } from './filters'

describe('defaultFilters / isDefault', () => {
  it('default is empty across every field', () => {
    const f = defaultFilters()
    expect(f).toEqual({ query: '', category: '', tags: [], scope: '' })
    expect(isDefault(f)).toBe(true)
  })

  it('any active facet makes it non-default', () => {
    expect(isDefault({ ...defaultFilters(), query: 'go' })).toBe(false)
    expect(isDefault({ ...defaultFilters(), category: 'learnings' })).toBe(false)
    expect(isDefault({ ...defaultFilters(), tags: ['a'] })).toBe(false)
    expect(isDefault({ ...defaultFilters(), scope: 'private' })).toBe(false)
  })
})

describe('toSearchParams', () => {
  it('on defaults sends only an empty query (server defaults stay authoritative)', () => {
    expect(toSearchParams(defaultFilters())).toEqual({ query: '' })
  })

  it('mirrors active query + category + tags', () => {
    const body = toSearchParams({ query: 'graph', category: 'learnings', tags: ['a', 'b'], scope: '' })
    expect(body).toEqual({ query: 'graph', category: 'learnings', tags: ['a', 'b'] })
  })

  it('omits an empty category and empty tags', () => {
    expect(toSearchParams({ ...defaultFilters(), query: 'x' })).toEqual({ query: 'x' })
    // empty category string and empty tag array must NOT appear as keys
    const body = toSearchParams({ query: '', category: '', tags: [], scope: '' })
    expect('category' in body).toBe(false)
    expect('tags' in body).toBe(false)
  })

  it('NEVER emits a scope or sensitivity request field (client-side facet only)', () => {
    const body = toSearchParams({ query: 'x', category: 'learnings', tags: ['a'], scope: 'private' })
    expect('scope' in body).toBe(false)
    expect('sensitivity' in body).toBe(false)
  })

  it('clones the tags array (no shared reference into the body)', () => {
    const f = { ...defaultFilters(), tags: ['a'] }
    const body = toSearchParams(f)
    expect(body.tags).toEqual(['a'])
    expect(body.tags).not.toBe(f.tags)
  })
})

describe('scopeVisible (client-side scope facet)', () => {
  it('passes every result when scope is empty', () => {
    expect(scopeVisible({ scope: 'private' }, defaultFilters())).toBe(true)
    expect(scopeVisible({ scope: 'shared' }, defaultFilters())).toBe(true)
  })

  it('narrows to the selected scope', () => {
    const f = { ...defaultFilters(), scope: 'private' }
    expect(scopeVisible({ scope: 'private' }, f)).toBe(true)
    expect(scopeVisible({ scope: 'shared' }, f)).toBe(false)
  })
})
