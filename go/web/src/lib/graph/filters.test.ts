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
    expect(edgeVisible({ rel: 'causal', conf: 0.9, kind: 'dream' }, f)).toBe(true)
    expect(edgeVisible({ rel: 'causal', conf: 0.7, kind: 'dream' }, f)).toBe(false)
    expect(edgeVisible({ rel: 'topical', conf: 0.9, kind: 'dream' }, f)).toBe(false)
  })

  // GC2 blocklist model (design 03-§4.3): [] = everything visible; hiding is
  // EXACT (only the deselected class); unknown classes stay visible in EVERY
  // filter state — a materialized allowlist variant fails both pins below
  // (extra-click regression made structurally impossible).
  it('default ([]) shows UNKNOWN structural classes; dream gates never apply', () => {
    const f = { ...defaultFilters(), linkClasses: ['causal'], minConfidence: 0.9 }
    // never-deselected class from a future registry extension → visible
    expect(edgeVisible({ rel: 'brand-new-class', conf: 1, kind: 'structural' }, f)).toBe(true)
    expect(edgeVisible({ rel: 'references', conf: 1, kind: 'structural' }, f)).toBe(true)
    expect(edgeVisible({ rel: 'causal', conf: 0.5, kind: 'dream' }, f)).toBe(false)
  })

  it('hides EXACTLY the deselected class; a class arriving AFTER a deselection stays visible', () => {
    const f = { ...defaultFilters(), structClassesHidden: ['references'] }
    expect(edgeVisible({ rel: 'references', conf: 1, kind: 'structural' }, f)).toBe(false)
    // the after-deselection case: duplicate-of loads later — never deselected → visible
    expect(edgeVisible({ rel: 'duplicate-of', conf: 1, kind: 'structural' }, f)).toBe(true)
    expect(edgeVisible({ rel: 'brand-new-class', conf: 1, kind: 'structural' }, f)).toBe(true)
  })

  it('minConfidence exempts structural facts (no confidence dimension, M076)', () => {
    const f = { ...defaultFilters(), minConfidence: 0.9 }
    expect(edgeVisible({ rel: 'references', conf: 0, kind: 'structural' }, f)).toBe(true)
    expect(edgeVisible({ rel: 'topical', conf: 0.5, kind: 'dream' }, f)).toBe(false)
  })
})

describe('toEgoQuery', () => {
  it('omits defaults entirely (server defaults stay authoritative)', () => {
    expect(toEgoQuery(defaultFilters())).toEqual({})
    expect(isDefault(defaultFilters())).toBe(true)
  })

  it('mirrors active filters as ego params (unified link_class carries both sides)', () => {
    const q = toEgoQuery(
      {
        categories: ['learnings'],
        minConfidence: 0.5,
        linkClasses: ['topical', 'causal'],
        createdAfter: '2026-01-01',
        createdBefore: '2026-06-01',
        structClassesHidden: [],
      },
      ['references'],
    )
    expect(q).toEqual({
      category: ['learnings'],
      min_confidence: 0.5,
      // GB5 contract: a set link_class partitions BOTH sides, an empty side
      // matches nothing — the known structural classes ride along.
      link_class: ['topical', 'causal', 'references'],
      created_after: '2026-01-01T00:00:00Z',
      created_before: '2026-06-01T23:59:59Z',
    })
  })

  // GC2 server mirror, amended to the GB5 unified-channel contract
  // ("absent = everything; set = both sides partitioned, empty side matches
  // NOTHING"). The mirror never sends a one-sided CSV: whenever either side
  // filters, active dream classes + (known minus hidden) go together.
  describe('unified link_class derivation (GB5 contract)', () => {
    const known = ['references', 'duplicate-of']

    it('full default → no param (server delivers everything, unknown classes included)', () => {
      expect(toEgoQuery(defaultFilters(), known)).toEqual({})
      expect(toEgoQuery(defaultFilters(), [])).toEqual({})
    })

    it('structural hidden → dream side rides along untouched', () => {
      const f = { ...defaultFilters(), structClassesHidden: ['duplicate-of'] }
      expect(toEgoQuery(f, known)).toEqual({
        link_class: ['topical', 'factual', 'causal', 'recurrent', 'supersedes', 'references'],
      })
    })

    it('dream deselection NEVER drops structural silently — known classes ride along', () => {
      // The regression the GB5 semantics would cause with a dream-only CSV:
      // deselecting 'causal' (a dream concern) must not empty the structural
      // side ("leere Seite matcht nichts").
      const f = { ...defaultFilters(), linkClasses: ['topical', 'factual', 'recurrent', 'supersedes'] }
      expect(toEgoQuery(f, known)).toEqual({
        link_class: ['topical', 'factual', 'recurrent', 'supersedes', 'references', 'duplicate-of'],
      })
    })

    it('dream filtered but NO structural class known yet → param suppressed (visibility beats mirror)', () => {
      // A dream-only CSV before the first structural load would lock ALL
      // structural classes out of every subsequent fetch.
      const f = { ...defaultFilters(), linkClasses: ['topical'] }
      expect(toEgoQuery(f, [])).toEqual({})
    })

    it('ALL known structural hidden → dream-only CSV is the user intent', () => {
      const f = { ...defaultFilters(), structClassesHidden: known }
      expect(toEgoQuery(f, known)).toEqual({
        link_class: ['topical', 'factual', 'causal', 'recurrent', 'supersedes'],
      })
    })

    it('everything deselected on both sides → no param (client reducers hide locally)', () => {
      const f = { ...defaultFilters(), linkClasses: [], structClassesHidden: known }
      expect(toEgoQuery(f, known)).toEqual({})
    })

    it('all dream deselected, structural default → structural-only CSV', () => {
      const f = { ...defaultFilters(), linkClasses: [] }
      expect(toEgoQuery(f, known)).toEqual({ link_class: known })
    })
  })

  it('isDefault requires an empty blocklist', () => {
    expect(isDefault({ ...defaultFilters(), structClassesHidden: ['references'] })).toBe(false)
  })
})
