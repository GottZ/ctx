// Tier-guard + landing table (design 06-role-nav.md §4 N4/N5-Gate). landingFor
// per tier; guardArea over the {area × tier} matrix including the two
// invariants that protect the restore race and the ungated corpus: loading →
// null (no redirect before whoami resolves, R6) and free areas → null. The
// redirect target equals landingFor(caps), so a forbidden area sends each tier
// to its own landing. Caps are derived through the real capabilitiesFor (N1
// dep) so the policy is tested end-to-end, not against hand-built flag combos
// that could drift.

import { describe, expect, it } from 'vitest'
import { capabilitiesFor } from '../auth/capabilities'
import type { WhoamiResponse } from '../api/types'
import { guardArea, landingFor } from './guard'

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

const loading = capabilitiesFor(null)
const member = capabilitiesFor(whoami({ admin: false, role: 'member' }))
const tenantAdmin = capabilitiesFor(whoami({ admin: false, role: 'owner' }))
const serverAdmin = capabilitiesFor(whoami({ admin: true, role: 'owner' }))

describe('landingFor', () => {
  it('member → /blocks (corpus entry that exists today)', () => {
    expect(landingFor(member)).toBe('/blocks')
  })
  it('tenant-admin → /status (ops focus)', () => {
    expect(landingFor(tenantAdmin)).toBe('/status')
  })
  it('server-admin → /status (ops focus)', () => {
    expect(landingFor(serverAdmin)).toBe('/status')
  })
  it('loading → /status (safe default before the session resolves)', () => {
    expect(landingFor(loading)).toBe('/status')
  })
})

describe('guardArea', () => {
  it('loading → null for every area (no redirect during the restore race, R6)', () => {
    for (const path of ['/admin', '/admin/tenants', '/tenant', '/tenant/backends', '/blocks']) {
      expect(guardArea(path, loading)).toBeNull()
    }
  })

  it('free (ungated) areas → null for every resolved tier', () => {
    for (const caps of [member, tenantAdmin, serverAdmin]) {
      for (const path of ['/blocks', '/graph', '/chat', '/status', '/settings', '/']) {
        expect(guardArea(path, caps)).toBeNull()
      }
    }
  })

  it('/admin requires server-admin; lower tiers redirect to their landing', () => {
    expect(guardArea('/admin', member)).toBe('/blocks')
    expect(guardArea('/admin', tenantAdmin)).toBe('/status')
    expect(guardArea('/admin', serverAdmin)).toBeNull()
    // deep sub-route inherits the gate
    expect(guardArea('/admin/tenants', member)).toBe('/blocks')
    expect(guardArea('/admin/tenants', serverAdmin)).toBeNull()
  })

  it('/tenant requires tenant-admin or higher; member redirects to its landing', () => {
    expect(guardArea('/tenant', member)).toBe('/blocks')
    expect(guardArea('/tenant', tenantAdmin)).toBeNull()
    expect(guardArea('/tenant', serverAdmin)).toBeNull()
    // deep sub-route inherits the gate
    expect(guardArea('/tenant/backends', member)).toBe('/blocks')
    expect(guardArea('/tenant/backends', tenantAdmin)).toBeNull()
  })
})
