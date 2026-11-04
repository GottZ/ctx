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

// Gateway policy as whoami carries it (W05-01, design/05 §4.6). Only `enabled`
// decides the /gateway gate; the other fields ride along unchanged.
function modelApi(enabled: boolean): WhoamiResponse['capabilities'] {
  return {
    model_api: {
      enabled,
      self_service: false,
      key_max_lifetime_s: 31_536_000,
      rotation_grace_s: 86_400,
      expiry_warn_s: 1_209_600,
    },
  }
}
const memberGatewayOn = capabilitiesFor(whoami({ admin: false, role: 'member', capabilities: modelApi(true) }))
const tenantAdminGatewayOff = capabilitiesFor(whoami({ admin: false, role: 'owner', capabilities: modelApi(false) }))
const tenantAdminGatewayOn = capabilitiesFor(whoami({ admin: false, role: 'admin', capabilities: modelApi(true) }))
const serverAdminGatewayOff = capabilitiesFor(whoami({ admin: true, role: 'owner', capabilities: modelApi(false) }))

describe('landingFor', () => {
  it('member → /home (the dedicated capability screen, N6)', () => {
    expect(landingFor(member)).toBe('/home')
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
    for (const path of ['/admin', '/admin/tenants', '/tenant', '/tenant/backends', '/gateway', '/blocks']) {
      expect(guardArea(path, loading)).toBeNull()
    }
  })

  it('free (ungated) areas → null for every resolved tier', () => {
    for (const caps of [member, tenantAdmin, serverAdmin]) {
      for (const path of ['/home', '/blocks', '/graph', '/chat', '/status', '/settings', '/']) {
        expect(guardArea(path, caps)).toBeNull()
      }
    }
  })

  it('/admin requires server-admin; lower tiers redirect to their landing', () => {
    expect(guardArea('/admin', member)).toBe('/home')
    expect(guardArea('/admin', tenantAdmin)).toBe('/status')
    expect(guardArea('/admin', serverAdmin)).toBeNull()
    // deep sub-route inherits the gate
    expect(guardArea('/admin/tenants', member)).toBe('/home')
    expect(guardArea('/admin/tenants', serverAdmin)).toBeNull()
  })

  it('/tenant requires tenant-admin or higher; member redirects to its landing', () => {
    expect(guardArea('/tenant', member)).toBe('/home')
    expect(guardArea('/tenant', tenantAdmin)).toBeNull()
    expect(guardArea('/tenant', serverAdmin)).toBeNull()
    // deep sub-route inherits the gate
    expect(guardArea('/tenant/backends', member)).toBe('/home')
    expect(guardArea('/tenant/backends', tenantAdmin)).toBeNull()
  })

  it('/admin/types inherits the /admin server-admin gate — no new entry (U04 pin, U10 vorgriff)', () => {
    // design 04 §4.1.4: /admin/types matches the ['/admin', viewTenants] prefix
    // via startsWith, so it is server-admin-only WITHOUT a dedicated TIER_GATED
    // row. Pinned now so U10 can land the route knowing the guard already covers
    // it (else a member/tenant-admin deep link would reach the type-registry admin).
    expect(guardArea('/admin/types', member)).toBe('/home')
    expect(guardArea('/admin/types', tenantAdmin)).toBe('/status')
    expect(guardArea('/admin/types', serverAdmin)).toBeNull()
  })

  it('/gateway requires viewModelApi: member never, tenant-admin only with gateway.enabled, server-admin always (W05-05)', () => {
    // member ⇒ its landing, with the gateway on or off (tier first, design/05 §4.6)
    expect(guardArea('/gateway', member)).toBe('/home')
    expect(guardArea('/gateway', memberGatewayOn)).toBe('/home')
    // tenant-admin before the switch (enabled:false) and against an older
    // server that sends no model_api at all ⇒ its landing
    expect(guardArea('/gateway', tenantAdminGatewayOff)).toBe('/status')
    expect(guardArea('/gateway', tenantAdmin)).toBe('/status')
    // tenant-admin once the gateway is on ⇒ allowed
    expect(guardArea('/gateway', tenantAdminGatewayOn)).toBeNull()
    // server-admin prepares the area before the switch ⇒ allowed either way
    expect(guardArea('/gateway', serverAdminGatewayOff)).toBeNull()
    expect(guardArea('/gateway', serverAdmin)).toBeNull()
    // the restore race never redirects (R6)
    expect(guardArea('/gateway', loading)).toBeNull()
  })

  it('/gateway sub-routes inherit the gate; the prefix match stops at a path boundary (W05-05)', () => {
    for (const path of ['/gateway/x', '/gateway/consumers/550e8400-e29b-41d4-a716-446655440001', '/gateway/models']) {
      expect(guardArea(path, member)).toBe('/home')
      expect(guardArea(path, tenantAdminGatewayOff)).toBe('/status')
      expect(guardArea(path, tenantAdminGatewayOn)).toBeNull()
      expect(guardArea(path, serverAdminGatewayOff)).toBeNull()
    }
    // '/gatewayx' is not under the prefix — no gate claims it
    expect(guardArea('/gatewayx', member)).toBeNull()
  })

  it('/gateway is disjoint from /admin and /tenant: each prefix keeps its own capability (W05-05)', () => {
    // A tenant-admin with the gateway on enters /gateway and /tenant, never /admin —
    // the peer page stays /admin/peers (design/05 §4.6), it does not move under /gateway.
    expect(guardArea('/gateway', tenantAdminGatewayOn)).toBeNull()
    expect(guardArea('/tenant', tenantAdminGatewayOn)).toBeNull()
    expect(guardArea('/admin', tenantAdminGatewayOn)).toBe('/status')
    expect(guardArea('/admin/peers', tenantAdminGatewayOn)).toBe('/status')
    // The gateway switch opens nothing else for a member.
    expect(guardArea('/tenant', memberGatewayOn)).toBe('/home')
    expect(guardArea('/admin', memberGatewayOn)).toBe('/home')
    // A tenant-admin before the switch keeps /tenant.
    expect(guardArea('/tenant', tenantAdminGatewayOff)).toBeNull()
  })

  it('the workflow surface stays ungated for every tier (deep-link renders EmptyState, U04)', () => {
    // design 04 §4.1.4: /issues, /issues/:id, /board are member surfaces — the
    // guard never redirects them; visibility is the viewWorkflow flag's job, the
    // real authorization stays server-side. A dark-launched deep link must reach
    // the page (EmptyState), not bounce to a landing.
    for (const caps of [member, tenantAdmin, serverAdmin]) {
      for (const path of ['/issues', '/issues/550e8400-e29b-41d4-a716-446655440001', '/board']) {
        expect(guardArea(path, caps)).toBeNull()
      }
    }
  })
})
