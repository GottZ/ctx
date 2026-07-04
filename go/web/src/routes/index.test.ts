// Route-table gates (design 04-§2.3 + 01-shell-layout §4 S2): the base areas
// exist, every route stays lazy, none collides with paths chi registers on the
// server (a mistyped SPA route must not shadow /api, /mcp, OAuth or health
// endpoints), and every registered route resolves to a layout mode.
//
// The shape is pinned as a SUPERSET (arrayContaining), not exact equality:
// later axes (Admin/Tenant/Member) register their own routes in areaRoutes, so
// the table is additive/parallel-safe — adding a route never forces an edit to
// this test (design 01-shell-layout §5, §6).

import { describe, expect, it } from 'vitest'
import { RESERVED_SERVER_PREFIXES, areaMode, areaRoutes, entryRedirect } from './index'
import type { LayoutMode } from './index'
import { capabilitiesFor } from '../lib/auth/capabilities'
import type { WhoamiResponse } from '../lib/api/types'
import { contracts, pendingContracts } from '../../e2e/contract/registry'

/**
 * Base areas every build must register: the 5 corpus/ops areas + backends
 * deep-route + the member landing /home (N6) + the role-gated /admin and
 * /tenant areas (N4/N5) + the workflow surface (/issues, /issues/:id, /board —
 * design 04 §4.1.1, wave U04; dark-launched behind viewWorkflow but registered
 * so a deep link renders the EmptyState instead of NotFound) + catch-all.
 */
const BASE_AREAS = [
  '*',
  '/admin',
  '/admin/tenants/:id',
  '/admin/types',
  '/blocks',
  '/board',
  '/chat',
  '/graph',
  '/home',
  '/issues',
  '/issues/:id',
  '/settings',
  '/settings/backends',
  '/status',
  '/tenant',
] as const

// Reserved server prefixes the SPA must never claim (index.ts
// RESERVED_SERVER_PREFIXES). U04 adds '/webhooks' as a pin ahead of the W13
// forge-webhook route (design 04 §4.1.1 / Achse 03 §5.3): the route lives
// OUTSIDE /api and the SPA must never interpret it as a client route.
const REQUIRED_RESERVED_PREFIXES = [
  '/api',
  '/mcp',
  '/health',
  '/authorize',
  '/token',
  '/.well-known',
  '/webhooks',
] as const

function whoami(p: Partial<WhoamiResponse> = {}): WhoamiResponse {
  return {
    success: true,
    label: 'a key',
    home_scope: 'private',
    read_scopes: ['private'],
    admin: false,
    tenant_id: '0190000000007000800000000000abcd',
    role: 'member',
    api_key_id: '0190000000007000800000000000ke7',
    tenant_slug: 'default',
    tenant_display_name: 'Default Tenant',
    ...p,
  }
}

const memberCaps = capabilitiesFor(whoami({ admin: false, role: 'member' }))
const serverAdminCaps = capabilitiesFor(whoami({ admin: true, role: 'owner' }))
const loadingCaps = capabilitiesFor(null)

const LAYOUT_MODES: readonly LayoutMode[] = ['reading', 'canvas', 'split', 'thread', 'board']

describe('areaRoutes', () => {
  it('registers at least the base areas, backends sub-route and the catch-all', () => {
    expect(Object.keys(areaRoutes)).toEqual(expect.arrayContaining([...BASE_AREAS]))
  })

  it('keeps every area lazy (separate chunk per area)', () => {
    for (const loader of Object.values(areaRoutes)) {
      expect(typeof loader).toBe('function')
    }
  })

  it('reserves every required server prefix incl. /webhooks (W13 pin, design 04 §4.1.1)', () => {
    expect(RESERVED_SERVER_PREFIXES).toEqual(expect.arrayContaining([...REQUIRED_RESERVED_PREFIXES]))
  })

  it('claims no path inside the reserved server namespace', () => {
    const paths = Object.keys(areaRoutes).filter((p) => p.startsWith('/'))
    for (const path of paths) {
      for (const reserved of RESERVED_SERVER_PREFIXES) {
        expect(path === reserved || path.startsWith(`${reserved}/`)).toBe(false)
      }
    }
  })

  it('resolves every registered route to a layout mode (areaMode covers the table)', () => {
    for (const key of Object.keys(areaRoutes)) {
      expect(LAYOUT_MODES).toContain(areaMode(key))
    }
  })
})

// Matrix pin NEXT TO the table it guards (design 06 §4.2, wave PV4): the
// 4-point registration pattern (Route + LayoutMode + NavItem + Gate) is a
// 5-point pattern from PV4 on — + PageContract. This is the FAST-path twin of
// the Playwright matrix meta-test (e2e/contract/matrix.spec.ts): a new
// areaRoutes key without a registry entry turns `bun run test` red before the
// e2e suite ever boots.
describe('PageContract matrix pin (PV4)', () => {
  const contractRoutes = new Set(contracts.map((c) => c.route))
  const pendingRoutes = new Set(pendingContracts.map((p) => p.route))

  it('every areaRoutes key is contract-covered or a named pending entry', () => {
    for (const key of Object.keys(areaRoutes)) {
      expect(
        contractRoutes.has(key) || pendingRoutes.has(key),
        `route '${key}' has neither a PageContract nor a pending entry (e2e/contract/registry.ts)`,
      ).toBe(true)
    }
  })

  it('no pending entry shadows an existing contract (stale debt ⇒ red)', () => {
    for (const p of pendingContracts) {
      expect(contractRoutes.has(p.route), `pending '${p.route}' is stale — its contract exists`).toBe(false)
    }
  })
})

describe('entryRedirect', () => {
  it('canonicalizes / to the caps-adaptive landing: member → /home (N6)', () => {
    expect(entryRedirect('/', memberCaps)).toBe('/home')
  })

  it('canonicalizes / to /status for higher tiers and the loading floor', () => {
    expect(entryRedirect('/', serverAdminCaps)).toBe('/status')
    expect(entryRedirect('/', loadingCaps)).toBe('/status')
  })

  it('leaves every real route alone, regardless of tier', () => {
    for (const caps of [memberCaps, serverAdminCaps, loadingCaps]) {
      for (const path of ['/settings', '/status', '/graph', '/chat', '/admin', '/tenant', '/nope', '']) {
        expect(entryRedirect(path, caps)).toBeNull()
      }
    }
  })
})
