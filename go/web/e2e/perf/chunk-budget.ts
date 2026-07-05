// Deterministic on-disk byte gate (overnight plan C, wave C-W1 = PLc).
//
//   bun run test:budget        (run `bun run build` first — release build,
//                               NO VITE_E2E; in CI both happen inside the
//                               pinned toolchain container)
//
// Reads every dist/**/*.{js,css} artifact plus its precompressed .br sibling
// (the bytes go/web/web.go actually serves) and asserts against
// chunk-budget.json: a per-chunk budget for EVERY chunk (override or
// per-extension default), plus the transfer total. Zero runner variance —
// pure byte counts, no browser. The Lighthouse lab-metric layer is separate
// (nightly, warn-only); THIS gate is the PR authority for bundle bytes.
//
// Design decisions (masterplan C-W1):
//  - dist/ missing or empty is a hard FAIL, never a skip — a wiring bug that
//    runs the gate before the build must be visible (review C3-M2).
//  - A chunk at/above the compression threshold without a .br sibling is a
//    FAIL: precompression is broken, the Go handler would fall back to raw.
//  - Budgets/verdicts hold only for in-container builds (brotli output is
//    toolchain-version-sensitive) — see chunk-budget.json _doc.

import { readFileSync, readdirSync, statSync, existsSync } from 'node:fs'
import { join, relative } from 'node:path'

interface Budget {
  calibrated: boolean
  raw_threshold_bytes: number
  total_transfer_bytes: number
  defaults: { js_br_bytes: number; css_br_bytes: number }
  overrides: Record<string, number>
}

const webRoot = new URL('../..', import.meta.url).pathname
const distDir = join(webRoot, 'dist')
const budgetPath = new URL('./chunk-budget.json', import.meta.url).pathname
const budget = JSON.parse(readFileSync(budgetPath, 'utf8')) as Budget

/** Strip the 8-char vite content hash: `GraphPage-Cr0MwOqk.js` -> `GraphPage.js`. */
function stemOf(name: string): string {
  return name.replace(/-[A-Za-z0-9_-]{8}(?=\.(?:js|css)$)/, '')
}

function walk(dir: string): string[] {
  const out: string[] = []
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, entry.name)
    if (entry.isDirectory()) out.push(...walk(p))
    else out.push(p)
  }
  return out
}

const failures: string[] = []
const warnings: string[] = []

if (!existsSync(distDir)) {
  console.error('chunk-budget: dist/ missing — run `bun run build` first (release build, no VITE_E2E)')
  process.exit(1)
}

const artifacts = walk(distDir)
  .filter((p) => /\.(?:js|css)$/.test(p))
  .sort()

if (artifacts.length === 0) {
  console.error('chunk-budget: no js/css artifacts under dist/ — run `bun run build` first')
  process.exit(1)
}

let total = 0
const seenStems = new Set<string>()
const rows: string[] = []

for (const file of artifacts) {
  const rel = relative(distDir, file)
  const rawSize = statSync(file).size
  const brPath = `${file}.br`
  const hasBr = existsSync(brPath)
  const served = hasBr ? statSync(brPath).size : rawSize
  const stem = stemOf(rel.split('/').pop() as string)
  seenStems.add(stem)
  total += served

  if (!hasBr && rawSize >= budget.raw_threshold_bytes) {
    failures.push(
      `${rel}: ${rawSize} B raw with NO .br sibling (threshold ${budget.raw_threshold_bytes}) — precompression broken`,
    )
    continue
  }

  const limit =
    budget.overrides[stem] ?? (stem.endsWith('.css') ? budget.defaults.css_br_bytes : budget.defaults.js_br_bytes)
  const kind = hasBr ? 'br' : 'raw'
  rows.push(`  ${rel} (${stem}): ${served} B ${kind} / limit ${limit}`)
  if (served > limit) {
    failures.push(`${rel}: ${served} B ${kind} exceeds budget ${limit} B for '${stem}'`)
  }
}

for (const key of Object.keys(budget.overrides)) {
  if (!seenStems.has(key)) {
    warnings.push(`stale override '${key}': no matching chunk in dist/ (renamed or removed — clean up the budget)`)
  }
}

if (total > budget.total_transfer_bytes) {
  failures.push(`transfer total ${total} B exceeds total_transfer_bytes ${budget.total_transfer_bytes} B`)
}

console.log(`chunk-budget: ${artifacts.length} artifacts, transfer total ${total} B / limit ${budget.total_transfer_bytes} B`)
if (process.env.CTX_BUDGET_VERBOSE) for (const r of rows) console.log(r)
for (const w of warnings) console.warn(`chunk-budget WARN: ${w}`)

if (failures.length > 0) {
  console.error(`chunk-budget: ${failures.length} violation(s):`)
  for (const f of failures) console.error(`  FAIL ${f}`)
  process.exit(1)
}
console.log('chunk-budget: OK')
