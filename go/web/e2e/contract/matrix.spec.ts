import { readFileSync } from 'node:fs'
import { expect, test } from '@playwright/test'
import { areaRoutes } from '../../src/routes/index'
import { validateContract } from './contract'
import { renderCoverage } from './coverage'
import { contracts, pendingContracts } from './registry'

// Matrix meta-test (design 06 §4.2, wave PV4): »jede Seite« as a mechanical
// red/green gate, never a claim. Imports areaRoutes from src/routes/index.ts —
// the SOURCE, not a copy — so a route registered in Achse 04 without a
// PageContract turns this suite red the moment it lands. The 4-point
// registration pattern (Route + LayoutMode + NavItem + Gate) is a 5-point
// pattern from PV4 on: + PageContract.
//
// 'login' is a mandatory entry OUTSIDE the route table (the gate mounts before
// the router, src/App.svelte) — pinned here so the pre-router page cannot fall
// through the »every route« net.

const ROUTE_KEYS = [...Object.keys(areaRoutes), 'login']

test.describe('contract matrix (meta)', () => {
  test('every route key carries exactly one contract XOR one named pending entry', () => {
    const contractRoutes = new Set(contracts.map((c) => c.route))
    const pendingRoutes = new Set(pendingContracts.map((p) => p.route))

    const uncovered = ROUTE_KEYS.filter((r) => !contractRoutes.has(r) && !pendingRoutes.has(r))
    expect(
      uncovered,
      `route(s) without PageContract AND without pending entry — register a contract in e2e/contract/registry.ts (design 06 §4.2)`,
    ).toEqual([])

    // A pending entry whose contract exists is stale debt bookkeeping ⇒ red
    // (same ratchet direction as the a11y baseline: fixed ⇒ shrink the ledger).
    const stale = pendingContracts.filter((p) => contractRoutes.has(p.route))
    expect(stale.map((p) => p.route), 'stale pending entries — the contract exists, remove the debt row').toEqual([])
  })

  test('no orphan registry entries: every contract/pending route is a real route key', () => {
    const known = new Set(ROUTE_KEYS)
    expect(
      contracts.filter((c) => !known.has(c.route)).map((c) => c.route),
      'contract routes must exist in areaRoutes (or be the login gate) — a contract for a dead route proves nothing',
    ).toEqual([])
    expect(
      pendingContracts.filter((p) => !known.has(p.route)).map((p) => p.route),
      'pending routes must exist in areaRoutes (or be the login gate)',
    ).toEqual([])
  })

  test('contract routes and names are unique (they key baselines and matrix rows)', () => {
    const routes = contracts.map((c) => c.route)
    const names = contracts.map((c) => c.name)
    expect(new Set(routes).size, `duplicate contract routes in ${routes.join(', ')}`).toBe(routes.length)
    expect(new Set(names).size, `duplicate contract names in ${names.join(', ')}`).toBe(names.length)
  })

  test('every contract passes structural validation (scale/mobile/flowDoc discipline)', () => {
    for (const c of contracts) {
      // Scale enforcement (W8, PV4 gate b): an empty exempt reason is a
      // violation — the 10k+ dimension cannot be opted out of silently.
      expect(validateContract(c), `contract '${c.name}' violates the §4.1 model`).toEqual([])
    }
  })

  test('pending entries carry non-empty reasons (visible debt, not a silent gap)', () => {
    for (const p of pendingContracts) {
      expect(p.reason.trim(), `pending route '${p.route}' needs a reason + delivering wave`).not.toBe('')
    }
  })

  test('COVERAGE.md matches the registry (regenerate: bun run e2e:matrix)', () => {
    const committed = readFileSync(new URL('../COVERAGE.md', import.meta.url), 'utf8')
    expect(
      committed,
      'e2e/COVERAGE.md drifted from the registry — it is generated, never hand-edited (design 06 §3.4; PV4 gate c)',
    ).toBe(renderCoverage())
  })
})
