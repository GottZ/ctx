// Ratchet meta-test (vitest) for the a11y baseline-debt ledger (design 06
// §3.3, wave PV5). Two layers:
//
//   1. The PURE gate rules (evaluateGate) — all four §3.3 outcomes proven
//      shrink-only at unit level: grew ⇒ red, partial ⇒ red, stale ⇒ red,
//      exact match ⇒ tolerated. The generated axe tests enforce the same
//      rules at runtime against the live DOM.
//   2. The COMMITTED ledger (e2e/a11y-baseline.json) — structural validity:
//      every entry references a contracted page, carries the mandatory issue
//      ref (§3.3 Issue-Pflicht), a frozen budget ≥ 1 and nodePaths that
//      document exactly that budget.

import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import { entriesFor, evaluateGate, validateLedger, type A11yBaselineEntry, type A11yLedger } from './a11y'
import { contracts, pendingContracts } from './registry'

const CTX = { page: '/status', theme: 'dark', viewport: 'desktop' } as const

function entry(over: Partial<A11yBaselineEntry> = {}): A11yBaselineEntry {
  return {
    page: '/status',
    rule: 'color-contrast',
    theme: 'dark',
    viewport: 'desktop',
    targets: ['.tile .value'],
    nodes: 2,
    nodePaths: ['div.tile:nth-child(1) > .value', 'div.tile:nth-child(2) > .value'],
    since: '2026-07-02',
    issue: 'design 06 §3.3 (interim ref)',
    ...over,
  }
}

const node = (rule: string, target: string, coveredBy: string[]) => ({ rule, target, coveredBy })

describe('evaluateGate — §3.3 node-count freeze, shrink-only ratchet', () => {
  it('tolerates matched == nodes and surfaces it as debt annotation', () => {
    const r = evaluateGate([entry()], CTX, [
      node('color-contrast', 'div.tile:nth-child(1) > .value', ['.tile .value']),
      node('color-contrast', 'div.tile:nth-child(2) > .value', ['.tile .value']),
    ])
    expect(r.findings).toEqual([])
    expect(r.tolerated).toHaveLength(1)
    expect(r.tolerated[0]).toContain('2 node(s) tolerated')
  })

  it('reds a GROWN entry (matched > nodes) — absorption path closed', () => {
    const r = evaluateGate([entry()], CTX, [
      node('color-contrast', 'a', ['.tile .value']),
      node('color-contrast', 'b', ['.tile .value']),
      node('color-contrast', 'c', ['.tile .value']),
    ])
    expect(r.findings).toHaveLength(1)
    expect(r.findings[0]).toContain('debt entry grew')
  })

  it('reds a PARTIAL fix (0 < matched < nodes) — the ledger must shrink', () => {
    const r = evaluateGate([entry()], CTX, [node('color-contrast', 'a', ['.tile .value'])])
    expect(r.findings).toHaveLength(1)
    expect(r.findings[0]).toContain('partial fix')
  })

  it('reds a STALE entry (matched == 0) — a fixed entry must leave the JSON', () => {
    const r = evaluateGate([entry()], CTX, [])
    expect(r.findings).toHaveLength(1)
    expect(r.findings[0]).toContain('baseline entry stale')
  })

  it('reds any violation not covered by an entry (unexpected)', () => {
    const r = evaluateGate([], CTX, [node('button-name', 'button.icon', [])])
    expect(r.findings).toHaveLength(1)
    expect(r.findings[0]).toContain('unexpected a11y violation')
    expect(r.findings[0]).toContain('button-name')
  })

  it('matches rule AND selector — same selector under another rule stays unexpected', () => {
    const r = evaluateGate([entry()], CTX, [node('target-size', 'x', ['.tile .value'])])
    // The target-size node is unexpected AND the contrast entry is stale.
    expect(r.findings).toHaveLength(2)
    expect(r.findings.join('\n')).toContain('unexpected')
    expect(r.findings.join('\n')).toContain('stale')
  })

  it('entriesFor slices strictly per run context (theme/viewport/page)', () => {
    const ledger: A11yLedger = {
      entries: [entry(), entry({ theme: 'light' }), entry({ viewport: 'mobile' }), entry({ page: '/chat' })],
    }
    expect(entriesFor(ledger, CTX)).toHaveLength(1)
  })
})

describe('committed ledger — structural validity (§3.3 Issue-Pflicht)', () => {
  const ledger = JSON.parse(
    readFileSync(new URL('../a11y-baseline.json', import.meta.url), 'utf8'),
  ) as A11yLedger
  const knownPages = [...contracts.map((c) => c.route), ...pendingContracts.map((p) => p.route)]

  it('every entry is structurally valid and references a contracted page', () => {
    expect(validateLedger(ledger, knownPages)).toEqual([])
  })

  it('declares its schema (a11y-baseline.schema.json)', () => {
    expect(ledger.$schema).toBe('./a11y-baseline.schema.json')
  })
})

describe('validateLedger — rules provable red', () => {
  it('flags unknown page, empty issue, budget/paths mismatch and duplicates', () => {
    const bad: A11yLedger = {
      entries: [
        entry({ page: '/nope' }),
        entry({ issue: ' ' }),
        entry({ page: '/chat', nodes: 3 }), // nodePaths has 2
        entry({ page: '/graph' }),
        entry({ page: '/graph' }), // duplicate key
      ],
    }
    const errs = validateLedger(bad, ['/status', '/chat', '/graph'])
    expect(errs.join('\n')).toContain("page '/nope'")
    expect(errs.join('\n')).toContain('issue reference is mandatory')
    expect(errs.join('\n')).toContain('must document exactly nodes')
    expect(errs.join('\n')).toContain('duplicate entry key')
  })
})
