// Pins the filter model (design 05-§4-W4): client predicates match the
// server param mirror, defaults are omitted from the ego query.

import { describe, expect, it } from 'vitest'
import { defaultFilters, edgeVisible, isDefault, nodeVisible, toEgoQuery } from './filters'

describe('nodeVisible', () => {
  const attrs = { category: 'learnings', createdAt: '2026-06-01T12:00:00Z' }

  it('passes everything on defaults', () => {
    expect(nodeVisible(attrs, defaultFilters())).toBe(true)
  })

  it('filters by category allow-list', () => {
    expect(nodeVisible(attrs, { ...defaultFilters(), categories: ['decisions'] })).toBe(false)
    expect(nodeVisible(attrs, { ...defaultFilters(), categories: ['decisions', 'learnings'] })).toBe(true)
  })

  it('applies the created_at window with end-of-day padding', () => {
    expect(nodeVisible(attrs, { ...defaultFilters(), createdAfter: '2026-06-02' })).toBe(false)
    expect(nodeVisible(attrs, { ...defaultFilters(), createdBefore: '2026-06-01' })).toBe(true)
    expect(nodeVisible(attrs, { ...defaultFilters(), createdBefore: '2026-05-31' })).toBe(false)
  })
})

describe('edgeVisible', () => {
  it('gates on link class and confidence', () => {
    const f = { ...defaultFilters(), linkClasses: ['causal'], minConfidence: 0.8 }
    expect(edgeVisible({ rel: 'causal', conf: 0.9 }, f)).toBe(true)
    expect(edgeVisible({ rel: 'causal', conf: 0.7 }, f)).toBe(false)
    expect(edgeVisible({ rel: 'topical', conf: 0.9 }, f)).toBe(false)
  })
})

describe('toEgoQuery', () => {
  it('omits defaults entirely (server defaults stay authoritative)', () => {
    expect(toEgoQuery(defaultFilters())).toEqual({})
    expect(isDefault(defaultFilters())).toBe(true)
  })

  it('mirrors active filters as ego params', () => {
    const q = toEgoQuery({
      categories: ['learnings'],
      minConfidence: 0.5,
      linkClasses: ['topical', 'causal'],
      createdAfter: '2026-01-01',
      createdBefore: '2026-06-01',
    })
    expect(q).toEqual({
      category: ['learnings'],
      min_confidence: 0.5,
      link_class: ['topical', 'causal'],
      created_after: '2026-01-01T00:00:00Z',
      created_before: '2026-06-01T23:59:59Z',
    })
  })
})
