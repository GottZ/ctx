// Flake-quarantine ledger — PURE gate logic + structural tag resolution
// (design 06 §3.5/§5.4, wave PV11).
//
// A `@quarantine`-tagged spec is excluded from the per-PR mock-tier gate
// (playwright.config.ts grepInvert) but STILL run nightly (CTX_E2E_QUARANTINE=1)
// — quarantine means observed, not forgotten. The ledger (e2e/quarantine.json)
// is what makes that observation accountable, with two gates:
//
//   1. CAP        — > 5 entries ⇒ red (evaluateCap). The quarantine cannot grow
//                   into an invisible heap; crossing the cap forces a fix.
//   2. BIJECTION  — tag <-> ledger is 1:1 (evaluateBijection):
//                     tag WITHOUT entry  ⇒ red ("untracked quarantine") — a spec
//                       excluded from the PR gate that neither counts against the
//                       cap nor carries an issue ref would be the exact drift the
//                       cap is meant to stop;
//                     entry WITHOUT tag  ⇒ red ("stale") — a fixed/renamed test
//                       must leave the JSON (same shrink-only ratchet as a11y §3.3).
//
// The tag set is resolved STRUCTURALLY from Playwright's own collection
// (`playwright test --list --reporter=json`), NOT from a naive grep over spec
// source: a tag applied via the `{ tag: '@quarantine' }` option lives in the
// parsed test object, and only Playwright's collector knows the true, deduped
// leaf-title set (describe nesting, generated tests, etc.). grep is kept as a
// cheap SECOND signal (collectQuarantineGrep) — it must agree on presence, but
// the bijection is judged against the structural set.
//
// This module is PURE except collectQuarantineTagged/collectQuarantineGrep,
// which are explicitly impure (child_process + fs) and only invoked by the
// vitest meta-test (quarantine.test.ts). The split keeps the cap + bijection
// rules unit-testable against synthetic inputs.

import { execFileSync } from 'node:child_process'
import { readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'

/** The hard cap on simultaneously quarantined tests (design 06 §5.4). */
export const QUARANTINE_CAP = 5

/** The Playwright tag that excludes a spec from the PR gate (with the @). */
export const QUARANTINE_TAG = '@quarantine'

export interface QuarantineEntry {
  /** Playwright leaf test title of the `@quarantine`-tagged test. */
  title: string
  /** Mandatory tracking reference (GitHub-issue interim form, §3.3/§9.4). */
  issue: string
  /** Date the test entered quarantine (YYYY-MM-DD). */
  since: string
}

export interface QuarantineLedger {
  $schema?: string
  entries: QuarantineEntry[]
}

/** CAP gate (design 06 §5.4): more than QUARANTINE_CAP entries ⇒ one finding. */
export function evaluateCap(entries: QuarantineEntry[]): string[] {
  if (entries.length <= QUARANTINE_CAP) return []
  return [
    `quarantine cap exceeded: ${entries.length} entries > cap ${QUARANTINE_CAP} ` +
      `— fix a quarantined test before adding another (design 06 §5.4)`,
  ]
}

/**
 * BIJECTION gate (design 06 §5.4). `taggedTitles` is the structural set of
 * `@quarantine`-tagged leaf titles from the live suite; `ledgerTitles` is the
 * committed quarantine.json. Every element of one set must appear in the other.
 */
export function evaluateBijection(taggedTitles: readonly string[], ledgerTitles: readonly string[]): string[] {
  const findings: string[] = []
  const tagged = new Set(taggedTitles)
  const ledger = new Set(ledgerTitles)

  for (const t of tagged) {
    if (!ledger.has(t)) {
      findings.push(
        `untracked quarantine: test "${t}" carries ${QUARANTINE_TAG} but has no ` +
          `e2e/quarantine.json entry — it is excluded from the PR gate yet counts ` +
          `against nothing and carries no issue ref (design 06 §5.4)`,
      )
    }
  }
  for (const t of ledger) {
    if (!tagged.has(t)) {
      findings.push(
        `stale quarantine entry: "${t}" is in e2e/quarantine.json but no test ` +
          `carries the ${QUARANTINE_TAG} tag — a fixed/renamed test must leave the ` +
          `ledger (shrink-only ratchet, design 06 §5.4)`,
      )
    }
  }
  return findings
}

/** Structural ledger validation — consumed by the vitest meta-test. */
export function validateQuarantine(ledger: QuarantineLedger): string[] {
  const errs: string[] = []
  const seen = new Set<string>()
  for (const [i, e] of ledger.entries.entries()) {
    const at = `entries[${i}]`
    if (typeof e.title !== 'string' || e.title.trim() === '') errs.push(`${at}: title must be non-empty`)
    if (typeof e.issue !== 'string' || e.issue.trim() === '')
      errs.push(`${at}: issue reference is mandatory (design 06 §5.4 Issue-Pflicht)`)
    if (typeof e.since !== 'string' || !/^\d{4}-\d{2}-\d{2}$/.test(e.since))
      errs.push(`${at}: since must be YYYY-MM-DD`)
    if (e.title !== undefined) {
      if (seen.has(e.title)) errs.push(`${at}: duplicate entry title (${e.title})`)
      seen.add(e.title)
    }
  }
  return errs
}

// ── impure: structural tag resolution (only the meta-test calls these) ───────

interface PwListSpec {
  title?: string
  tags?: string[]
  specs?: PwListSpec[]
  suites?: PwListSpec[]
}

/** Walk the Playwright JSON-list tree, collecting titles of `tag`-tagged specs. */
function walkTagged(node: PwListSpec, tag: string, out: Set<string>): void {
  if (Array.isArray(node.tags) && node.tags.includes(tag) && typeof node.title === 'string') out.add(node.title)
  for (const s of node.specs ?? []) walkTagged(s, tag, out)
  for (const s of node.suites ?? []) walkTagged(s, tag, out)
}

/**
 * PRIMARY tag source: `playwright test --list --reporter=json` (collection
 * only — no browser, no webServer). Playwright's `tags` array drops the `@`,
 * so we match on the bare tag name. Runs from the go/web dir (cwd of vitest).
 */
export function collectQuarantineTagged(cwd?: string): string[] {
  const bare = QUARANTINE_TAG.replace(/^@/, '')
  // CTX_E2E_QUARANTINE=1 so grepInvert does not hide the tagged specs from the
  // very collection that must see them (config excludes @quarantine otherwise).
  const raw = execFileSync('bun', ['x', 'playwright', 'test', '--list', '--reporter=json'], {
    cwd,
    encoding: 'utf8',
    maxBuffer: 64 * 1024 * 1024,
    env: { ...process.env, CTX_E2E_QUARANTINE: '1', CTX_E2E_CONTAINER: '1' },
  })
  const report = JSON.parse(raw) as PwListSpec
  const out = new Set<string>()
  for (const s of report.suites ?? []) walkTagged(s, bare, out)
  for (const s of report.specs ?? []) walkTagged(s, bare, out)
  return [...out].sort()
}

/**
 * SECOND signal: a cheap grep over spec source for the literal tag. Not the
 * bijection authority (it cannot resolve generated/deduped titles), only a
 * presence cross-check — it must agree with the structural set on whether ANY
 * quarantine tag exists at all.
 */
export function collectQuarantineGrep(e2eDir: string): number {
  let hits = 0
  const walk = (dir: string): void => {
    for (const ent of readdirSync(dir, { withFileTypes: true })) {
      if (ent.name === 'node_modules' || ent.name === '.results') continue
      const p = join(dir, ent.name)
      if (ent.isDirectory()) walk(p)
      else if (ent.name.endsWith('.spec.ts')) {
        for (const line of readFileSync(p, 'utf8').split('\n')) if (line.includes(QUARANTINE_TAG)) hits++
      }
    }
  }
  walk(e2eDir)
  return hits
}
