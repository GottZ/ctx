// Graph filter model (design 05-§4-W4): one filter state drives BOTH the
// instant client-side sigma reducers (loaded elements) AND the query params
// of future expands. Pure module, pinned by filters.test.ts.

import type { EgoQuery } from './api'
import type { EdgeAttrs, NodeAttrs } from './graph-client'

export const ALL_LINK_CLASSES = ['topical', 'factual', 'causal', 'recurrent', 'supersedes'] as const

export interface GraphFilters {
  /** Empty = all categories pass. */
  categories: string[]
  /** Gate on the edge confidence (0 = off). */
  minConfidence: number
  /** Edge classes shown/traversed; default all five. */
  linkClasses: string[]
  /** ISO date bounds on node created_at ('' = open). */
  createdAfter: string
  createdBefore: string
}

export function defaultFilters(): GraphFilters {
  return {
    categories: [],
    minConfidence: 0,
    linkClasses: [...ALL_LINK_CLASSES],
    createdAfter: '',
    createdBefore: '',
  }
}

export function isDefault(f: GraphFilters): boolean {
  return (
    f.categories.length === 0 &&
    f.minConfidence === 0 &&
    f.linkClasses.length === ALL_LINK_CLASSES.length &&
    f.createdAfter === '' &&
    f.createdBefore === ''
  )
}

/** Client-side node predicate (sigma nodeReducer hides the rest). */
export function nodeVisible(attrs: Pick<NodeAttrs, 'category' | 'createdAt'>, f: GraphFilters): boolean {
  if (f.categories.length > 0 && !f.categories.includes(attrs.category)) return false
  if (f.createdAfter !== '' && attrs.createdAt < f.createdAfter) return false
  // createdBefore is a date (day) bound — pad to end-of-day for intuition.
  if (f.createdBefore !== '' && attrs.createdAt > `${f.createdBefore}T23:59:59Z`) return false
  return true
}

/** Client-side edge predicate; an edge with a hidden endpoint hides too. */
export function edgeVisible(attrs: Pick<EdgeAttrs, 'rel' | 'conf' | 'kind'>, f: GraphFilters): boolean {
  // INTERIM hard branch (GA2, design 03-§7 cut rule b): structural edges are
  // always visible — the dream allowlist below would hide every structural
  // class, breaking default-visibility without an extra click. GC2 replaces
  // this branch with the structClassesHidden blocklist model.
  if (attrs.kind === 'structural') return true
  if (!f.linkClasses.includes(attrs.rel)) return false
  if (f.minConfidence > 0 && attrs.conf < f.minConfidence) return false
  return true
}

/**
 * Server-side mirror for the NEXT expands (loaded elements filter
 * instantly, new fetches honor the same filters). Defaults are omitted —
 * the server's own defaults are authoritative.
 */
export function toEgoQuery(f: GraphFilters): EgoQuery {
  const q: EgoQuery = {}
  if (f.categories.length > 0) q.category = [...f.categories]
  if (f.minConfidence > 0) q.min_confidence = f.minConfidence
  if (f.linkClasses.length < ALL_LINK_CLASSES.length) q.link_class = [...f.linkClasses]
  if (f.createdAfter !== '') q.created_after = `${f.createdAfter}T00:00:00Z`
  if (f.createdBefore !== '') q.created_before = `${f.createdBefore}T23:59:59Z`
  return q
}
