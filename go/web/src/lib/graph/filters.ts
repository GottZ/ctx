// Graph filter model (design 05-§4-W4): one filter state drives BOTH the
// instant client-side sigma reducers (loaded elements) AND the query params
// of future expands. Pure module, pinned by filters.test.ts.

import type { EgoQuery } from './api'
import type { EdgeAttrs, NodeAttrs } from './graph-client'

export const ALL_LINK_CLASSES = ['topical', 'factual', 'causal', 'recurrent', 'supersedes'] as const

export interface GraphFilters {
  /** Empty = all categories pass. */
  categories: string[]
  /** Gate on the edge confidence (0 = off) — dream only, facts are exempt. */
  minConfidence: number
  /** Edge classes shown/traversed; default all five. */
  linkClasses: string[]
  /** ISO date bounds on node created_at ('' = open). */
  createdAfter: string
  createdBefore: string
  /** structural class BLOCKLIST (GC2, design 03-§4.3): [] = all visible
   *  (DEFAULT). Deselecting hides exactly the deselected class; UNKNOWN
   *  classes (registry growth à la M103) stay visible in EVERY filter state —
   *  deliberately NOT a materialized allowlist (dream-allowlist trap N8). */
  structClassesHidden: string[]
}

export function defaultFilters(): GraphFilters {
  return {
    categories: [],
    minConfidence: 0,
    linkClasses: [...ALL_LINK_CLASSES],
    createdAfter: '',
    createdBefore: '',
    structClassesHidden: [],
  }
}

/** Dropdown presentation of the category allowlist: the DEFAULT (empty
 *  selection = all pass) renders as "every option checked". */
export function categoryChecked(selected: string[], cat: string): boolean {
  return selected.length === 0 || selected.includes(cat)
}

/**
 * Toggle a category under the all-checked presentation while keeping the
 * model's allowlist semantics: unchecking from the default materializes the
 * allowlist as options-minus-that; a selection that covers every option
 * normalizes back to [] — so isDefault() holds and categories loaded LATER
 * stay visible (no permanently materialized allowlist).
 */
export function toggleCategory(selected: string[], options: string[], cat: string): string[] {
  let next: string[]
  if (selected.length === 0) next = options.filter((c) => c !== cat)
  else if (selected.includes(cat)) next = selected.filter((c) => c !== cat)
  else next = [...selected, cat]
  return options.every((c) => next.includes(c)) ? [] : next
}

export function isDefault(f: GraphFilters): boolean {
  return (
    f.categories.length === 0 &&
    f.minConfidence === 0 &&
    f.linkClasses.length === ALL_LINK_CLASSES.length &&
    f.createdAfter === '' &&
    f.createdBefore === '' &&
    f.structClassesHidden.length === 0
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
  if (attrs.kind === 'structural') {
    // Blocklist (GC2) — deliberate asymmetry to the dream allowlist below:
    // unknown classes are VISIBLE here, hidden there (N8). NO minConfidence
    // gate: facts have no confidence dimension (M076). A materialized
    // allowlist variant fails the unknown-class pins (mutation-probed).
    return !f.structClassesHidden.includes(attrs.rel)
  }
  if (!f.linkClasses.includes(attrs.rel)) return false
  if (f.minConfidence > 0 && attrs.conf < f.minConfidence) return false
  return true
}

/**
 * Server-side mirror for the NEXT expands (loaded elements filter
 * instantly, new fetches honor the same filters). Defaults are omitted —
 * the server's own defaults are authoritative.
 *
 * GC2 (amended to the GB5 contract): link_class is ONE unified channel over
 * dream ∪ structural vocabulary — server semantics "absent = everything; set
 * = both sides partitioned, an empty side matches NOTHING". The mirror
 * therefore NEVER sends a one-sided param: whenever either side filters, it
 * derives BOTH sides (active dream classes + known-minus-hidden structural).
 * `knownStructClasses` = classes loaded in the client (GraphPage settle sweep).
 *
 * Visibility precedence (FIX user directive beats cap-slot efficiency): if
 * the structural side is NOT representable — nothing loaded yet AND nothing
 * explicitly hidden — the param is suppressed entirely (a dream-only CSV
 * would silently drop ALL structural server-side); both sides then filter
 * client-only until classes are known. Documented approximation: while the
 * param is set, an expand only loads structural classes KNOWN at send time —
 * heals itself in the full default (no param). Mutation-probed: the rejected
 * one-sided dream-CSV variant fails 7 pins in filters.test.ts.
 */
export function toEgoQuery(f: GraphFilters, knownStructClasses: string[] = []): EgoQuery {
  const q: EgoQuery = {}
  if (f.categories.length > 0) q.category = [...f.categories]
  if (f.minConfidence > 0) q.min_confidence = f.minConfidence
  const dreamFiltered = f.linkClasses.length < ALL_LINK_CLASSES.length
  const structFiltered = f.structClassesHidden.length > 0
  if (dreamFiltered || structFiltered) {
    const structAllow = knownStructClasses.filter((c) => !f.structClassesHidden.includes(c))
    // Representable: at least one structural class survives, OR the user has
    // explicitly hidden classes (then an empty structural side is intent).
    if (structAllow.length > 0 || structFiltered) {
      const combined = [...f.linkClasses, ...structAllow]
      if (combined.length > 0) q.link_class = combined
    }
  }
  if (f.createdAfter !== '') q.created_after = `${f.createdAfter}T00:00:00Z`
  if (f.createdBefore !== '') q.created_before = `${f.createdBefore}T23:59:59Z`
  return q
}
