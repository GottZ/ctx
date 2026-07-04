// URL filter round-trip pins (design 04 §3.3/§4/§5.5, wave U05). The reload gate
// requires parse ∘ serialize to be LOSSLESS: a reload must re-derive the exact
// filter state (scope INCLUDED) from the URL. These tests pin that, and the
// NEGATIVE half proves the round-trip check actually catches a dropped param —
// a parser that ignores `status` breaks the round-trip and this suite goes red
// (the "erst rot gegen einen absichtlich ignorierten Query-Param" gate).

import { describe, expect, it } from 'vitest'
import {
  emptyFilters,
  isSearchMode,
  issueFiltersToQuery,
  parseIssueFilters,
  type IssueFilters,
} from './issue-filters'

describe('parseIssueFilters', () => {
  it('parses the full filter set incl. scope', () => {
    const f = parseIssueFilters('?scope=acme%3Amain&status=open&q=bug&type=issue&label=p1&label=ui')
    expect(f).toEqual({ scope: 'acme:main', status: 'open', q: 'bug', type: 'issue', labels: ['p1', 'ui'] })
  })

  it('accepts comma-listed labels as well as repeated keys', () => {
    expect(parseIssueFilters('?label=p1,ui').labels).toEqual(['p1', 'ui'])
    expect(parseIssueFilters('?label=p1&label=ui').labels).toEqual(['p1', 'ui'])
  })

  it('treats blank/absent params as null and never throws on a malformed query', () => {
    expect(parseIssueFilters('')).toEqual(emptyFilters())
    expect(parseIssueFilters('?status=%20%20&q=').status).toBeNull()
    expect(parseIssueFilters('?=&&scope')).toEqual(emptyFilters())
  })
})

describe('issueFiltersToQuery', () => {
  it('drops empty values and expands labels to repeated keys', () => {
    expect(issueFiltersToQuery(emptyFilters())).toBe('')
    expect(issueFiltersToQuery({ ...emptyFilters(), scope: 'acme:main' })).toBe('?scope=acme%3Amain')
    expect(issueFiltersToQuery({ ...emptyFilters(), labels: ['p1', 'ui'] })).toBe('?label=p1&label=ui')
  })

  it('emits a FIXED, stable key order (scope first)', () => {
    const f: IssueFilters = { scope: 'acme:main', status: 'open', q: 'bug', type: 'issue', labels: ['p1'] }
    expect(issueFiltersToQuery(f)).toBe('?scope=acme%3Amain&status=open&q=bug&type=issue&label=p1')
  })
})

describe('round-trip (the reload gate property)', () => {
  // The exhaustive round-trip: parse(serialize(f)) === f for every field. A
  // parser that DROPS any single param (scope/status/q/type/label) fails HERE —
  // this is the mechanism the §5.5 reload gate depends on (negative-probe: seed
  // parseIssueFilters to skip `status` and this case turns red).
  const cases: IssueFilters[] = [
    emptyFilters(),
    { scope: 'acme:main', status: null, q: null, type: null, labels: [] },
    { scope: 'team/alpha', status: 'in_progress', q: null, type: null, labels: [] },
    { scope: 'acme:main', status: 'open', q: 'flaky test', type: 'issue', labels: ['p1', 'ui', 'a11y'] },
    { scope: null, status: null, q: 'orphan search', type: null, labels: [] },
  ]
  for (const f of cases) {
    it(`round-trips ${JSON.stringify(f)}`, () => {
      expect(parseIssueFilters(issueFiltersToQuery(f))).toEqual(f)
    })
  }
})

describe('isSearchMode', () => {
  it('is true exactly when q is a non-empty string', () => {
    expect(isSearchMode(emptyFilters())).toBe(false)
    expect(isSearchMode({ ...emptyFilters(), q: '' })).toBe(false)
    expect(isSearchMode({ ...emptyFilters(), q: 'bug' })).toBe(true)
  })
})
