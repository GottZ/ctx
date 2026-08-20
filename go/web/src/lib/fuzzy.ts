// Fuzzy matcher for the settings search (and any future palette-style
// filter): pure scoring, no DOM. Three tiers per token, best one wins —
// substring (position-weighted), in-order subsequence (gap-penalized), and
// Levenshtein-to-best-substring (typo tolerance, distance budget scaled by
// token length). Multi-token queries AND across tokens; every token must
// land somewhere in the candidate's fields for the candidate to match.
//
// Highlighting: substring and subsequence tiers report exact character
// indices; the Levenshtein tier reports the best-matching window (the edit
// region is approximate by nature). Indices refer to the ORIGINAL field
// string, so the renderer can wrap them without re-deriving offsets.

/** One searchable field of a candidate, tagged so weights can differ. */
export interface FuzzyField {
  /** Field id, e.g. 'key' | 'description' | 'value' | 'env' | 'type'. */
  id: string
  text: string
  /** Score multiplier — key hits should outrank value hits. */
  weight: number
}

/** A token's hit inside one field. */
export interface FuzzyHit {
  field: string
  /** Matched char indices in the field's original text (for highlighting). */
  indices: number[]
  score: number
}

export interface FuzzyResult {
  /** Sum of the per-token best-field scores (higher = better). */
  score: number
  /** One entry per (token, field) hit — a field can appear multiple times. */
  hits: FuzzyHit[]
}

// Tier base scores. A weaker tier in a heavy field can outrank a stronger
// tier in a light field only across a large weight gap — intentional: a typo
// in the key is usually the better answer than a clean hit in a value.
const SUBSTRING_BASE = 100
const SUBSEQUENCE_BASE = 55
const LEVENSHTEIN_BASE = 40

/** Levenshtein edit budget for a token: 1 for short tokens, 2 from length 5. */
export function editBudget(tokenLen: number): number {
  if (tokenLen <= 2) return 0 // too short — typo tolerance would match noise
  if (tokenLen <= 4) return 1
  return 2
}

/**
 * Score one token against one text. Returns null when no tier matches.
 * All comparisons are lowercase; `text` keeps original casing for indices.
 */
export function matchToken(token: string, text: string): { indices: number[]; score: number } | null {
  if (token.length === 0 || text.length === 0) return null
  const t = text.toLowerCase()
  const q = token.toLowerCase()

  // Tier 1 — substring. Earlier position and tighter fit score higher; a
  // word-boundary start (after . _ - / or space) gets a bonus so "mode"
  // prefers backoff_mode over "model".
  const at = t.indexOf(q)
  if (at !== -1) {
    const boundary = at === 0 || '._-/ '.includes(t[at - 1])
    const posFactor = 1 - Math.min(at, 40) / 80 // 1 .. 0.5
    const coverFactor = q.length / t.length // full-field match → 1
    const score = SUBSTRING_BASE * posFactor * (0.6 + 0.4 * coverFactor) + (boundary ? 15 : 0)
    const indices = Array.from({ length: q.length }, (_, i) => at + i)
    return { indices, score }
  }

  // Tier 2 — in-order subsequence ("bomin" → backoff_min). Consecutive runs
  // reward, gaps penalize; fail when the spread is absurd (>4x token).
  const sub = subsequence(q, t)
  if (sub !== null) {
    const spread = sub.indices[sub.indices.length - 1] - sub.indices[0] + 1
    if (spread <= q.length * 4) {
      const density = q.length / spread // 1 = consecutive
      const score = SUBSEQUENCE_BASE * (0.4 + 0.6 * density)
      return { indices: sub.indices, score }
    }
  }

  // Tier 3 — Levenshtein distance to the BEST substring of text (DP with a
  // zero-cost first row): "backof" ~ "backoff", "treshold" ~ "threshold".
  const budget = editBudget(q.length)
  if (budget === 0) return null
  const best = bestSubstringDistance(q, t)
  if (best.distance <= budget) {
    const score = LEVENSHTEIN_BASE * (1 - best.distance / (budget + 1))
    const indices: number[] = []
    for (let i = best.start; i < best.end; i++) indices.push(i)
    return { indices, score }
  }

  return null
}

/** Longest-prefix greedy subsequence match; null when not all chars land. */
function subsequence(q: string, t: string): { indices: number[] } | null {
  const indices: number[] = []
  let ti = 0
  for (const ch of q) {
    const found = t.indexOf(ch, ti)
    if (found === -1) return null
    indices.push(found)
    ti = found + 1
  }
  return { indices }
}

/**
 * Minimum edit distance between q and any substring of t, plus that
 * substring's window. Standard fuzzy-substring DP: first row zeroed so a
 * match may start anywhere; traceback via a parallel start-index row.
 */
function bestSubstringDistance(q: string, t: string): { distance: number; start: number; end: number } {
  const m = q.length
  const n = t.length
  let prev = new Array<number>(n + 1).fill(0)
  let prevStart = Array.from({ length: n + 1 }, (_, j) => j)
  let curr = new Array<number>(n + 1).fill(0)
  let currStart = new Array<number>(n + 1).fill(0)

  for (let i = 1; i <= m; i++) {
    curr[0] = i
    currStart[0] = 0
    for (let j = 1; j <= n; j++) {
      const subCost = prev[j - 1] + (q[i - 1] === t[j - 1] ? 0 : 1)
      const delCost = prev[j] + 1 // drop a q char
      const insCost = curr[j - 1] + 1 // skip a t char
      if (subCost <= delCost && subCost <= insCost) {
        curr[j] = subCost
        currStart[j] = prevStart[j - 1]
      } else if (delCost <= insCost) {
        curr[j] = delCost
        currStart[j] = prevStart[j]
      } else {
        curr[j] = insCost
        currStart[j] = currStart[j - 1]
      }
    }
    ;[prev, curr] = [curr, prev]
    ;[prevStart, currStart] = [currStart, prevStart]
  }

  let bestJ = 0
  for (let j = 1; j <= n; j++) {
    if (prev[j] < prev[bestJ]) bestJ = j
  }
  return { distance: prev[bestJ], start: prevStart[bestJ], end: bestJ }
}

/**
 * Match a whitespace-tokenized query against a candidate's fields. Every
 * token must hit at least one field (AND); a token's score is its best
 * weighted field hit. Null = candidate filtered out.
 */
export function fuzzyMatch(query: string, fields: FuzzyField[]): FuzzyResult | null {
  const tokens = query.trim().split(/\s+/).filter((s) => s.length > 0)
  if (tokens.length === 0) return null
  let total = 0
  const hits: FuzzyHit[] = []
  for (const token of tokens) {
    let best: FuzzyHit | null = null
    for (const field of fields) {
      const m = matchToken(token, field.text)
      if (m === null) continue
      const weighted = m.score * field.weight
      if (best === null || weighted > best.score) {
        best = { field: field.id, indices: m.indices, score: weighted }
      }
    }
    if (best === null) return null
    total += best.score
    hits.push(best)
  }
  return { score: total, hits }
}

/**
 * Merge the hit indices of one field into sorted [start, end) ranges for
 * the highlight renderer (adjacent/duplicate indices collapse).
 */
export function highlightRanges(hits: FuzzyHit[], field: string): Array<[number, number]> {
  const set = new Set<number>()
  for (const h of hits) {
    if (h.field === field) for (const i of h.indices) set.add(i)
  }
  const sorted = [...set].sort((a, b) => a - b)
  const ranges: Array<[number, number]> = []
  for (const i of sorted) {
    const last = ranges[ranges.length - 1]
    if (last !== undefined && last[1] === i) last[1] = i + 1
    else ranges.push([i, i + 1])
  }
  return ranges
}
