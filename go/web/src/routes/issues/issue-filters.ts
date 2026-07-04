// URL-carried filter state for /issues (design 04 §3.3 / §4 URL-state, wave
// U05). The workflow surface is the ONE deliberate exception to the "kein
// URL-State"-Konvention (web-frontend.md §5): every filter — scope FIRST — lives
// in the query string so (a) each filter combination has a deep-linkable,
// Playwright-addressable URL and (b) issue lists are shareable (§3.3). Only
// filters travel in the URL — never content, never keys.
//
// parse ∘ serialize is a LOSSLESS round-trip: that is the property the reload
// gate (§5.5) leans on — a reload re-derives the exact same filter state from
// the URL. The round-trip vitest (issue-filters.test.ts) pins it, and its
// negative half proves a parser that DROPS a param is caught (a dropped param
// breaks the round-trip). Kept a pure module (no DOM) so it is asserted in a
// plain node environment.

import { parseScopeParam } from './scope-param'

/** The full deep-linkable filter set of the issue list. */
export interface IssueFilters {
  /** Repo-scope of the active project (§4.1.5 picker writes it) — null = none. */
  scope: string | null
  /** Workflow status (board column) server-filter (`?status=` → wire `state`). */
  status: string | null
  /** Free-text query — non-empty flips the list into SEARCH mode (§6.1, no append). */
  q: string | null
  /** Registry type key narrowing (`?type=`) — carried for deep-link fidelity. */
  type: string | null
  /** Label AND-filter (`?label=` repeated / comma), server tag-overlap filter. */
  labels: string[]
}

/** The empty filter state (nothing selected). */
export function emptyFilters(): IssueFilters {
  return { scope: null, status: null, q: null, type: null, labels: [] }
}

/** A single non-empty query value, trimmed, or null when absent/blank. */
function oneParam(sp: URLSearchParams, key: string): string | null {
  const raw = sp.get(key)
  const v = raw?.trim()
  return v ? v : null
}

/**
 * Parse the FULL filter state from a `location.search` string. `scope` reuses
 * parseScopeParam (single source for the scope rule). `label` accepts repeated
 * keys AND comma lists (the client serialises repeated keys; a hand-written
 * deep link may use either). Never throws on a malformed query.
 */
export function parseIssueFilters(search: string): IssueFilters {
  const sp = new URLSearchParams(search)
  const labels = sp
    .getAll('label')
    .flatMap((v) => v.split(','))
    .map((v) => v.trim())
    .filter((v) => v !== '')
  return {
    scope: parseScopeParam(search),
    status: oneParam(sp, 'status'),
    q: oneParam(sp, 'q'),
    type: oneParam(sp, 'type'),
    labels,
  }
}

/**
 * Serialise the filter state back to a `location.search` string (leading '?',
 * or '' when nothing is set). Key order is FIXED (scope, status, q, type,
 * label…) so the URL is stable across writes — a stable URL is what makes the
 * replaceState round-trip idempotent and the Playwright baselines deterministic.
 * Empty/null values are dropped; labels expand to repeated `label=` keys.
 */
export function issueFiltersToQuery(f: IssueFilters): string {
  const sp = new URLSearchParams()
  if (f.scope) sp.set('scope', f.scope)
  if (f.status) sp.set('status', f.status)
  if (f.q) sp.set('q', f.q)
  if (f.type) sp.set('type', f.type)
  for (const l of f.labels) if (l.trim() !== '') sp.append('label', l)
  const s = sp.toString()
  return s ? `?${s}` : ''
}

/** True when the query text puts the list in SEARCH mode (Top-N, no append). */
export function isSearchMode(f: IssueFilters): boolean {
  return f.q !== null && f.q !== ''
}
