// Board column classification (design 04 §4.2/§6.2, wave U07). PURE, DOM-free —
// the open/closed/unmapped verdict is POLICY DATA (the type registry's workflow
// config), NEVER a hardcoded status list (the doctrine: mechanism = code,
// policy = data, ctx 019e83df-3787).
//
// The board wire (BoardResponse.columns[]) carries only status/count/issues/
// cursor — NO category, NO terminal flag, NO order field: the column ORDER is
// the wire array order (server-derived from the type-config States order,
// project_issues.go HandleBoard → blocktype WorkflowStates). The closed-collapse
// + unmapped signal is NOT on the board wire; it comes from GET /api/types
// (config.workflow.{states,terminal}). This module joins the two and touches the
// order of NEITHER.

import type { BlockTypeView, BoardColumn, IssueCursor, IssueRow } from '../../lib/api/types'

/** A board column's derived category. 'unmapped' = a wire status the registry
 * does not know (registry staleness / drift) — rendered read-only, never
 * collapsed, so a verwaiste Karten-Spalte stays visible instead of vanishing. */
export type ColumnCategory = 'open' | 'closed' | 'unmapped'

/** The registry-derived status vocabulary. Unioned across ALL workflow-bearing
 * types so the board never hardcodes a single type name (§4): a status is
 * 'known' if any type declares it, 'terminal' if any type marks it closed. */
export interface StatusVocab {
  known: Set<string>
  terminal: Set<string>
}

/** A board column enriched with its registry-derived category. The wire order is
 * preserved verbatim — this shape carries no order field because the array
 * position IS the order (§5.5). */
export interface ClassifiedColumn {
  status: string
  count: number
  issues: IssueRow[]
  cursor: IssueCursor
  category: ColumnCategory
}

/** Build the status vocabulary from the effective type registry (GET /api/types).
 * Reads config.workflow.{states,terminal}; a type without a workflow section
 * contributes nothing. terminal ⊆ states by construction (policy.go validate),
 * but we add terminal into `known` too so a config that only lists a status in
 * `terminal` still classifies it as closed, never unmapped. */
export function vocabFromTypes(types: BlockTypeView[]): StatusVocab {
  const known = new Set<string>()
  const terminal = new Set<string>()
  for (const t of types) {
    const wf = t.config.workflow
    if (wf === undefined) continue
    for (const s of wf.states ?? []) known.add(s)
    for (const s of wf.terminal ?? []) {
      terminal.add(s)
      known.add(s)
    }
  }
  return { known, terminal }
}

/** Classify each wire column against the vocabulary, PRESERVING wire order (a
 * plain map — no sort, no reorder). terminal → 'closed', otherwise-known →
 * 'open', unknown → 'unmapped'. A hardcoded status→category table would LOSE (or
 * misclassify) any status outside it — that is exactly the U07 negative gate;
 * keying off the registry data instead makes an unknown status a first-class
 * 'unmapped' column rather than a dropped one. */
export function classifyColumns(columns: BoardColumn[], vocab: StatusVocab): ClassifiedColumn[] {
  return columns.map((c) => ({
    status: c.status,
    count: c.count,
    issues: c.issues,
    cursor: c.cursor,
    category: vocab.terminal.has(c.status) ? 'closed' : vocab.known.has(c.status) ? 'open' : 'unmapped',
  }))
}

/** The status ids that start collapsed (§6.2): the closed/terminal columns. At
 * 10k+ the mass lives in done/closed, so the first render stays on the open
 * windows; unmapped columns start OPEN (they may hold verwaiste Karten worth
 * seeing). */
export function initialCollapsed(columns: ClassifiedColumn[]): string[] {
  return columns.filter((c) => c.category === 'closed').map((c) => c.status)
}
