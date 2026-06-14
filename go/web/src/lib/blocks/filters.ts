// Block-workbench filter model (W2, plan §W2): category + tags + free-text
// query as ONE reducer state -> POST /api/search params. Pure module, pinned
// by filters.test.ts (mirrors lib/graph/filters.ts).
//
// SCOPE DECISION: POST /api/search has NO scope param — the server gates
// visibility through the auth key's readScopes (context_search.go:25). A scope
// facet is therefore CLIENT-SIDE only: `scope` lives in the filter state and
// the page narrows the already-loaded results to it (scopeVisible), but
// toSearchParams MUST NEVER emit a scope (or sensitivity) request field — those
// are not part of the /api/search contract.
//
// `category` is single-select (server WHERE category = $n); `tags` is a
// free-text array (server WHERE tags && $n, overlap match); `query` is the
// stemmed FTS text (empty -> newest-first default list).

import type { SearchRequest } from '../api/blocks'
import type { SearchResult } from '../graph/api'

export interface BlockFilters {
  /** Stemmed FTS text; '' = newest-first default list. */
  query: string
  /** Single category (server matches `category = $n`); '' = all. */
  category: string
  /** Free-text tag array (server overlap-matches `tags && $n`); [] = all. */
  tags: string[]
  /** CLIENT-SIDE only scope chip over the visible results; '' = all scopes. */
  scope: string
}

export function defaultFilters(): BlockFilters {
  return { query: '', category: '', tags: [], scope: '' }
}

export function isDefault(f: BlockFilters): boolean {
  return f.query === '' && f.category === '' && f.tags.length === 0 && f.scope === ''
}

/**
 * Server-side search params for POST /api/search. The query is ALWAYS sent;
 * category/tags only when active (empty category string and empty tag array
 * are omitted so the server applies its own defaults). Never emits scope or
 * sensitivity — neither is a /api/search field (scope is a client-side facet,
 * sensitivity is not exposed at all). Mirrors the toEgoQuery omit pattern.
 */
export function toSearchParams(f: BlockFilters): SearchRequest {
  const body: SearchRequest = { query: f.query }
  if (f.category !== '') body.category = f.category
  if (f.tags.length > 0) body.tags = [...f.tags]
  return body
}

/**
 * CLIENT-SIDE scope predicate over an already-loaded result: an empty scope
 * filter passes all, otherwise the result's scope must match exactly. The
 * server gates true visibility through the auth key's readScopes — this only
 * narrows what is already visible.
 */
export function scopeVisible(r: Pick<SearchResult, 'scope'>, f: BlockFilters): boolean {
  return f.scope === '' || r.scope === f.scope
}
