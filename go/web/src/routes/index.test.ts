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

/**
 * Base areas every build must register: the 5 corpus/ops areas + backends
 * deep-route + the member landing /home (N6) + the role-gated /admin and
 * /tenant areas (N4/N5) + catch-all.
 */
const BASE_AREAS = [
  '*',
  '/admin',
  '/blocks',
  '/chat',
  '/graph',
  '/home',
  '/settings',
  '/settings/backends',
  '/status',
  '/tenant',
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

const LAYOUT_MODES: readonly LayoutMode[] = ['reading', 'canvas', 'split', 'thread']

describe('areaRoutes', () => {
  it('registers at least the base areas, backends sub-route and the catch-all', () => {
    expect(Object.keys(areaRoutes)).toEqual(expect.arrayContaining([...BASE_AREAS]))
  })

  it('keeps every area lazy (separate chunk per area)', () => {
    for (const loader of Object.values(areaRoutes)) {
      expect(typeof loader).toBe('function')
    }
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
