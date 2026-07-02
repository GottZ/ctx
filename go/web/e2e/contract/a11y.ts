// a11y baseline-debt ledger — PURE gate logic (design 06 §3.3, wave PV5).
//
// Existing violations are FROZEN as visible, shrinkable debt in
// e2e/a11y-baseline.json, with a frozen node count per entry so the debt
// cannot grow INSIDE an entry either. Gate rules (§3.3), per entry:
//
//   matched > nodes  ⇒ red  ("debt entry grew")    — closes the absorption
//                             path where a later tile with a new violation
//                             hides under an existing selector;
//   matched < nodes  ⇒ red  ("partial fix")        — the ratchet forces the
//                             ledger to shrink with every fix;
//   matched == nodes ⇒ tolerated, surfaced as a testInfo annotation;
//   matched == 0     ⇒ red  ("stale entry")        — a fixed entry must
//                             DISAPPEAR from the JSON (shrink-only ratchet).
//
// Every violation node NOT covered by a matching entry ⇒ red (unexpected).
//
// `nodes` (the count) is the gate; `nodePaths` (axe targets at freeze time)
// are triage documentation, never a matching criterion — DOM paths move under
// refactorings, a path gate would be false-red-prone (§3.3).
//
// Model deviation from the design-06 §3.3 example, named (not silently
// resolved): entries additionally carry `theme` + `viewport` as REQUIRED
// matching context. The doc example keys entries by (page, rule, target)
// only, but the axe gate runs 4 contexts per page (2 themes × 2 viewports —
// the doc's own §4.1 table, "Kontrast ist theme-abhängig") and a
// contrast violation that exists only in one theme would be reported
// "stale" by the other theme's run under a context-free key. The freeze is
// therefore per (page, theme, viewport, rule, targets).
//
// This module is PURE (no Playwright runtime): the DOM-dependent part —
// which generalized entry selector covers which violation node — is resolved
// in-page by axe.ts and handed in as `coveredBy`. That split makes the whole
// ratchet unit-testable (a11y.test.ts).

import type { Theme, ViewportName } from './contract'

export interface A11yBaselineEntry {
  /** Contract route the entry applies to ('/status', '*', 'login'). */
  page: string
  /** axe rule id ('color-contrast', 'target-size', …). */
  rule: string
  /** Run context — a violation frozen in dark is NOT tolerated in light. */
  theme: Theme
  viewport: ViewportName
  /** Generalized CSS selectors that DELIMIT the debt region (coverage domain). */
  targets: string[]
  /** Frozen node budget — THE gate (§3.3). */
  nodes: number
  /** Exact axe node targets at freeze time — triage documentation only. */
  nodePaths: string[]
  /** Freeze date (YYYY-MM-DD). */
  since: string
  /** Mandatory tracking reference — design-doc anchor until Achse-02 issues exist (§3.3 interim rule). */
  issue: string
}

export interface A11yLedger {
  $schema?: string
  entries: A11yBaselineEntry[]
}

/** One axe violation node, with its in-page coverage already resolved. */
export interface GateNode {
  rule: string
  /** axe node target (top-frame selector) — for messages/triage. */
  target: string
  /** Entry-target selectors (from the run-context entries) that cover this node. */
  coveredBy: string[]
}

export interface GateContext {
  page: string
  theme: Theme
  viewport: ViewportName
}

export interface GateResult {
  /** Red findings — the gate asserts this list is empty. */
  findings: string[]
  /** Tolerated debt entries (matched == nodes) — surfaced as annotations. */
  tolerated: string[]
}

/** The run-context slice of the ledger (page + theme + viewport must match). */
export function entriesFor(ledger: A11yLedger, ctx: GateContext): A11yBaselineEntry[] {
  return ledger.entries.filter(
    (e) => e.page === ctx.page && e.theme === ctx.theme && e.viewport === ctx.viewport,
  )
}

/**
 * Apply the §3.3 gate rules. `entries` MUST already be the run-context slice
 * (entriesFor) — staleness is judged per context, so a dark-only entry cannot
 * be washed green by the light run. Each node counts against AT MOST one
 * entry (first matching entry in ledger order wins — overlapping entries for
 * the same rule are a ledger smell, kept deterministic here).
 */
export function evaluateGate(entries: A11yBaselineEntry[], ctx: GateContext, nodes: GateNode[]): GateResult {
  const findings: string[] = []
  const tolerated: string[] = []
  const label = (e: A11yBaselineEntry) => `${ctx.page} [${ctx.theme}/${ctx.viewport}] ${e.rule} @ ${e.targets.join(', ')}`

  const matched = new Map<A11yBaselineEntry, number>()
  for (const e of entries) matched.set(e, 0)

  for (const n of nodes) {
    const owner = entries.find((e) => e.rule === n.rule && e.targets.some((t) => n.coveredBy.includes(t)))
    if (owner === undefined) {
      findings.push(
        `unexpected a11y violation: rule '${n.rule}' at ${n.target} on ${ctx.page} ` +
          `[${ctx.theme}/${ctx.viewport}] — not covered by any a11y-baseline.json entry ` +
          `(new violations are fixed, never absorbed; design 06 §3.3)`,
      )
      continue
    }
    matched.set(owner, (matched.get(owner) ?? 0) + 1)
  }

  for (const e of entries) {
    const m = matched.get(e) ?? 0
    if (m === 0) {
      findings.push(
        `baseline entry stale — remove it: ${label(e)} matched 0 nodes ` +
          `(a fixed entry must disappear from the JSON; shrink-only ratchet, design 06 §3.3)`,
      )
    } else if (m > e.nodes) {
      findings.push(
        `debt entry grew — new violation under existing selector: ${label(e)} ` +
          `matched ${m} nodes, frozen budget is ${e.nodes} (design 06 §3.3)`,
      )
    } else if (m < e.nodes) {
      findings.push(
        `partial fix — shrink the entry: ${label(e)} matched ${m} nodes, ` +
          `frozen budget is ${e.nodes}; update nodes/nodePaths to the fixed state (design 06 §3.3)`,
      )
    } else {
      tolerated.push(`${label(e)}: ${m} node(s) tolerated (frozen ${e.since}, ref ${e.issue})`)
    }
  }

  return { findings, tolerated }
}

/** Structural ledger validation — consumed by the vitest meta-test (a11y.test.ts). */
export function validateLedger(ledger: A11yLedger, knownPages: readonly string[]): string[] {
  const errs: string[] = []
  const seen = new Set<string>()
  for (const [i, e] of ledger.entries.entries()) {
    const at = `entries[${i}]`
    if (!knownPages.includes(e.page)) errs.push(`${at}: page '${e.page}' is not a contracted route`)
    if (e.rule.trim() === '') errs.push(`${at}: rule must be non-empty`)
    if (e.theme !== 'dark' && e.theme !== 'light') errs.push(`${at}: theme '${e.theme}' invalid`)
    if (e.viewport !== 'desktop' && e.viewport !== 'mobile') errs.push(`${at}: viewport '${e.viewport}' invalid`)
    if (e.targets.length === 0 || e.targets.some((t) => t.trim() === ''))
      errs.push(`${at}: targets must be non-empty selectors`)
    if (!Number.isInteger(e.nodes) || e.nodes < 1) errs.push(`${at}: nodes must be an integer ≥ 1`)
    if (e.nodePaths.length !== e.nodes)
      errs.push(`${at}: nodePaths (${e.nodePaths.length}) must document exactly nodes (${e.nodes}) targets`)
    if (!/^\d{4}-\d{2}-\d{2}$/.test(e.since)) errs.push(`${at}: since must be YYYY-MM-DD`)
    if (e.issue.trim() === '') errs.push(`${at}: issue reference is mandatory (§3.3 Issue-Pflicht)`)
    const key = `${e.page}|${e.theme}|${e.viewport}|${e.rule}|${e.targets.join(',')}`
    if (seen.has(key)) errs.push(`${at}: duplicate entry key (${key})`)
    seen.add(key)
  }
  return errs
}
