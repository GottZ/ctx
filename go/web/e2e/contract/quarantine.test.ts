// Meta-test (vitest) for the flake-quarantine ledger (design 06 §3.5/§5.4,
// wave PV11). Two layers, same shape as the a11y ratchet meta-test (a11y.test.ts):
//
//   1. The PURE gate rules — the three §5.4 negative probes proven red at unit
//      level: cap > 5 ⇒ red, `@quarantine` tag without entry ⇒ "untracked
//      quarantine", entry without tag ⇒ "stale". The nightly grepInvert wiring
//      enforces the same rules against the live suite.
//   2. The COMMITTED ledger (e2e/quarantine.json) — structural validity + the
//      STRUCTURAL bijection: the `@quarantine` tag set resolved from Playwright's
//      own `--list --reporter=json` collection must equal the ledger titles.
//      Currently both are empty (the healthy default) — the gate stays live.

import { execSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'
import {
  QUARANTINE_CAP,
  collectQuarantineGrep,
  collectQuarantineTagged,
  evaluateBijection,
  evaluateCap,
  validateQuarantine,
  type QuarantineEntry,
  type QuarantineLedger,
} from './quarantine'

function entry(over: Partial<QuarantineEntry> = {}): QuarantineEntry {
  return { title: 'flaky spec title', issue: 'design 06 §5.4 (interim ref)', since: '2026-07-05', ...over }
}

describe('evaluateCap — §5.4 hard deckel (> 5 ⇒ red)', () => {
  it('tolerates entries up to the cap', () => {
    const at = Array.from({ length: QUARANTINE_CAP }, (_, i) => entry({ title: `t${i}` }))
    expect(evaluateCap(at)).toEqual([])
  })

  it('reds one over the cap — a quarantined test must be fixed before another is added', () => {
    const over = Array.from({ length: QUARANTINE_CAP + 1 }, (_, i) => entry({ title: `t${i}` }))
    const f = evaluateCap(over)
    expect(f).toHaveLength(1)
    expect(f[0]).toContain('quarantine cap exceeded')
    expect(f[0]).toContain(`${QUARANTINE_CAP + 1} entries > cap ${QUARANTINE_CAP}`)
  })
})

describe('evaluateBijection — §5.4 tag<->ledger 1:1', () => {
  it('is green when the tag set equals the ledger set', () => {
    expect(evaluateBijection(['a', 'b'], ['b', 'a'])).toEqual([])
  })

  it('reds a tag WITHOUT a ledger entry — "untracked quarantine"', () => {
    const f = evaluateBijection(['a', 'b'], ['a'])
    expect(f).toHaveLength(1)
    expect(f[0]).toContain('untracked quarantine')
    expect(f[0]).toContain('"b"')
  })

  it('reds a ledger entry WITHOUT a tag — "stale"', () => {
    const f = evaluateBijection(['a'], ['a', 'ghost'])
    expect(f).toHaveLength(1)
    expect(f[0]).toContain('stale quarantine entry')
    expect(f[0]).toContain('"ghost"')
  })
})

describe('validateQuarantine — structural rules provable red', () => {
  it('flags empty title, empty issue, bad since and duplicate titles', () => {
    const bad: QuarantineLedger = {
      entries: [
        entry({ title: ' ' }),
        entry({ issue: ' ' }),
        entry({ since: '07-2026' }),
        entry({ title: 'dup' }),
        entry({ title: 'dup' }),
      ],
    }
    const errs = validateQuarantine(bad)
    expect(errs.join('\n')).toContain('title must be non-empty')
    expect(errs.join('\n')).toContain('issue reference is mandatory')
    expect(errs.join('\n')).toContain('since must be YYYY-MM-DD')
    expect(errs.join('\n')).toContain('duplicate entry title')
  })
})

describe('committed ledger — e2e/quarantine.json', () => {
  const ledger = JSON.parse(
    readFileSync(new URL('../quarantine.json', import.meta.url), 'utf8'),
  ) as QuarantineLedger

  it('declares its schema', () => {
    expect(ledger.$schema).toBe('./quarantine.schema.json')
  })

  it('is structurally valid and within the cap', () => {
    expect(validateQuarantine(ledger)).toEqual([])
    expect(evaluateCap(ledger.entries)).toEqual([])
  })

  // The live gate: resolve the `@quarantine` tag set STRUCTURALLY from
  // Playwright's own collection and assert the 1:1 bijection with the ledger.
  // Collection (`playwright test --list`) needs no browser but loads every spec
  // — hence the generous timeout. grep is the presence cross-check only.
  it('tag<->ledger bijection holds against the live suite (structural)', () => {
    const cwd = process.cwd() // go/web (vitest root)
    const tagged = collectQuarantineTagged(cwd)
    const ledgerTitles = ledger.entries.map((e) => e.title)
    expect(evaluateBijection(tagged, ledgerTitles)).toEqual([])

    // Second signal: grep source presence must agree with the structural set.
    const grepHits = collectQuarantineGrep(join(cwd, 'e2e'))
    expect(grepHits > 0).toBe(tagged.length > 0)
  }, 120_000)
})

// Guard the grepInvert wiring itself is present in the config: the PR gate MUST
// exclude @quarantine while a nightly (CTX_E2E_QUARANTINE=1) includes it — the
// runtime behaviour is proven in the wave gate via --list counts, this pins the
// config knob so a refactor cannot silently drop the exclusion.
describe('config wiring — @quarantine PR-gate exclusion', () => {
  it('playwright.config.ts inverts @quarantine unless CTX_E2E_QUARANTINE', () => {
    const cfg = execSync('cat playwright.config.ts', { cwd: process.cwd(), encoding: 'utf8' })
    expect(cfg).toContain('@quarantine')
    expect(cfg).toContain('CTX_E2E_QUARANTINE')
  })
})
