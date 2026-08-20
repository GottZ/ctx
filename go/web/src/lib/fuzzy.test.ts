// Fuzzy matcher unit tests: the three tiers (substring, subsequence,
// Levenshtein-to-substring), the AND-across-tokens contract, field
// weighting, and the highlight-range merge.

import { describe, expect, it } from 'vitest'
import { editBudget, fuzzyMatch, highlightRanges, matchToken } from './fuzzy'

describe('matchToken — substring tier', () => {
  it('finds an exact substring with its indices', () => {
    const m = matchToken('backoff', 'dream.backoff_min')
    expect(m).not.toBeNull()
    expect(m?.indices).toEqual([6, 7, 8, 9, 10, 11, 12])
  })

  it('is case-insensitive against the original casing', () => {
    const m = matchToken('BACKOFF', 'dream.backoff_min')
    expect(m).not.toBeNull()
    expect(m?.indices[0]).toBe(6)
  })

  it('scores a word-boundary start above a mid-word hit at comparable position', () => {
    const boundary = matchToken('mode', 'dream.mode')
    const midword = matchToken('mode', 'dreammodel')
    expect(boundary).not.toBeNull()
    expect(midword).not.toBeNull()
    expect(boundary!.score).toBeGreaterThan(midword!.score)
  })
})

describe('matchToken — subsequence tier', () => {
  it('matches in-order scattered characters', () => {
    const m = matchToken('bomin', 'backoff_min')
    expect(m).not.toBeNull()
    expect(m?.indices).toHaveLength(5)
  })

  it('rejects out-of-order characters without a typo path', () => {
    expect(matchToken('nim_ffokcab', 'backoff_min')).toBeNull()
  })
})

describe('matchToken — Levenshtein tier', () => {
  it('tolerates one typo on a mid-length token', () => {
    // "backpff" is no substring and no subsequence ("p" breaks the chain).
    const m = matchToken('backpff', 'dream.backoff_min')
    expect(m).not.toBeNull()
  })

  it('tolerates a transposition on a long token', () => {
    const m = matchToken('treshhold', 'query.score_threshold')
    expect(m).not.toBeNull()
  })

  it('gives short tokens no edit budget', () => {
    expect(editBudget(2)).toBe(0)
    expect(editBudget(4)).toBe(1)
    expect(editBudget(8)).toBe(2)
    expect(matchToken('xy', 'ab')).toBeNull()
  })

  it('rejects beyond the edit budget', () => {
    expect(matchToken('zzzzz', 'backoff')).toBeNull()
  })
})

describe('fuzzyMatch — multi-token AND across fields', () => {
  const fields = [
    { id: 'key', text: 'dream.backoff_factor', weight: 1 },
    { id: 'description', text: 'growth base of the exponential curve', weight: 0.7 },
  ]

  it('requires every token to land somewhere', () => {
    expect(fuzzyMatch('backoff growth', fields)).not.toBeNull()
    expect(fuzzyMatch('backoff zebra', fields)).toBeNull()
  })

  it('reports per-token hits with their field', () => {
    const r = fuzzyMatch('backoff growth', fields)
    expect(r?.hits.map((h) => h.field).sort()).toEqual(['description', 'key'])
  })

  it('weights the key above the description on equal-quality hits', () => {
    const a = fuzzyMatch('dream', [
      { id: 'key', text: 'dream.think', weight: 1 },
      { id: 'description', text: 'dream toggle', weight: 0.7 },
    ])
    expect(a?.hits[0].field).toBe('key')
  })

  it('returns null for an empty query', () => {
    expect(fuzzyMatch('   ', fields)).toBeNull()
  })
})

describe('highlightRanges', () => {
  it('merges adjacent indices into ranges and keeps gaps apart', () => {
    const hits = [
      { field: 'key', indices: [0, 1, 2, 7, 8], score: 1 },
      { field: 'key', indices: [2, 3], score: 1 },
      { field: 'other', indices: [5], score: 1 },
    ]
    expect(highlightRanges(hits, 'key')).toEqual([
      [0, 4],
      [7, 9],
    ])
  })

  it('returns no ranges for a field without hits', () => {
    expect(highlightRanges([], 'key')).toEqual([])
  })
})
