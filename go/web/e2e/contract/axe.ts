// axe gate fixture (design 06 §4.5, wave PV5) — projektweit EINE Definition.
//
// WCAG 2.2 AA as far as automatable: the four stable WCAG tags PLUS the
// explicitly enabled `target-size` rule (SC 2.5.8, 24×24 px) — the ONLY
// wcag22aa rule in axe-core, default-disabled and NOT activated by
// withTags/runOnly (research §3.2). Without that enable, "WCAG 2.2 AA"
// would be an empty label.
//
// API trap, verified against @axe-core/playwright 4.12.1 source (named
// doc↔Ist deviation): the design-06 §4.5 snippet chains
// `.withTags([...]).options({ rules: … })` — but `AxeBuilder#options()`
// REPLACES the whole internal option object (dist/index.js: `options(o) {
// this.option = o }`), silently DROPPING the runOnly tag scoping: ALL axe
// rules would run, not the WCAG set. This fixture therefore passes runOnly
// and the target-size enable in ONE options() call.
//
// `incomplete` results are ALWAYS attached (§4.5): the documented contrast
// blind spots (gradients, overlapping elements — research §3.3) must not be
// invisibly green. Triage pointer lives in COVERAGE.md.

import { AxeBuilder } from '@axe-core/playwright'
import { expect, type Page, type TestInfo } from '@playwright/test'
// Static JSON import (resolveJsonModule) instead of node:fs: svelte-check
// walks this file with the app tsconfig (types: svelte + vite/client, no
// node) and rejects fs imports here — the JSON module is typed, fs-free and
// carried identically by playwright, vitest and bun.
import committedLedger from '../a11y-baseline.json' with { type: 'json' }
import { entriesFor, evaluateGate, type A11yLedger, type GateNode } from './a11y'
import type { PageContract, Theme, ViewportName } from './contract'

export const AXE_TAGS = ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'] as const

/** The committed debt ledger (design 06 §3.3) — one load per process. */
export function loadLedger(): A11yLedger {
  return committedLedger as A11yLedger
}

/** Top-frame selector of an axe node target (no iframes/shadow roots in this SPA). */
function topSelector(target: unknown): string {
  if (Array.isArray(target)) return topSelector(target[target.length - 1])
  return String(target ?? '')
}

/**
 * Run the axe scan + baseline-debt gate for one contract in one run context.
 * Fails on: any violation not covered by the ledger, grown/partially-fixed/
 * stale ledger entries (§3.3 node-count freeze + shrink-only ratchet).
 */
export async function runAxeGate(
  page: Page,
  c: PageContract,
  theme: Theme,
  viewport: ViewportName,
  testInfo: TestInfo,
): Promise<void> {
  let builder = new AxeBuilder({ page }).options({
    // ONE call: runOnly + rules together — see the API trap in the header.
    runOnly: { type: 'tag', values: [...AXE_TAGS] },
    rules: { 'target-size': { enabled: true } },
  })
  for (const ex of c.axe?.exclude ?? []) builder = builder.exclude(ex.selector)

  const res = await builder.analyze()

  // incomplete is ALWAYS attached — never invisibly green (§4.5).
  await testInfo.attach('axe-incomplete', {
    body: JSON.stringify(
      res.incomplete.map((v) => ({ rule: v.id, impact: v.impact, nodes: v.nodes.map((n) => topSelector(n.target)) })),
      null,
      2,
    ),
    contentType: 'application/json',
  })

  const ctx = { page: c.route, theme, viewport }
  const entries = entriesFor(loadLedger(), ctx)

  // Resolve WHICH generalized entry selector covers WHICH violation node —
  // CSS matching needs the live DOM; the gate rules themselves stay pure
  // (a11y.ts, unit-tested). A node is covered when it matches the selector
  // or sits inside a matching region (closest) — that inside-path is exactly
  // the absorption path the node-count freeze then bounds.
  const rawNodes = res.violations.flatMap((v) => v.nodes.map((n) => ({ rule: v.id, target: topSelector(n.target) })))
  const entryTargets = [...new Set(entries.flatMap((e) => e.targets))]
  const coverage: string[][] = await page.evaluate(
    ({ nodes, targets }) =>
      nodes.map((n) => {
        let el: Element | null = null
        try {
          el = document.querySelector(n.target)
        } catch {
          el = null
        }
        if (!el) return []
        return targets.filter((t) => {
          try {
            return el.matches(t) || el.closest(t) !== null
          } catch {
            return false
          }
        })
      }),
    { nodes: rawNodes, targets: entryTargets },
  )
  const nodes: GateNode[] = rawNodes.map((n, i) => ({ ...n, coveredBy: coverage[i] ?? [] }))

  const { findings, tolerated } = evaluateGate(entries, ctx, nodes)
  for (const t of tolerated) testInfo.annotations.push({ type: 'a11y-debt', description: t })

  expect(findings, `axe gate on ${c.route} [${theme}/${viewport}]:\n${findings.join('\n')}`).toEqual([])
}
