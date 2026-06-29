// Tier-derivation table (design 06-role-nav.md §4 N1-Gate). Covers the three
// real tiers, unknown-role→member (R3 least privilege), the admin+member combo
// that proves admin overrides role (R4), and null→loading (R6). The flag
// containment server-admin ⊇ tenant-admin ⊇ member is asserted per row.

import { describe, expect, it } from 'vitest'
import type { WhoamiResponse } from '../api/types'
import { capabilitiesFor } from './capabilities'

function whoami(p: Partial<WhoamiResponse> = {}): WhoamiResponse {
  return {
    success: true,
    label: 'a key',
    home_scope: 'private',
    read_scopes: ['private', 'shared'],
    admin: false,
    tenant_id: '0190000000007000800000000000abcd',
    role: 'member',
    api_key_id: '0190000000007000800000000000ke7',
    tenant_slug: 'default',
    tenant_display_name: 'Default Tenant',
    ...p,
  }
}

describe('capabilitiesFor', () => {
  it('null whoami → loading, every flag false (restore race, R6)', () => {
    expect(capabilitiesFor(null)).toEqual({
      tier: 'loading',
      viewCorpus: false,
      viewTenants: false,
      manageTenantKeys: false,
      manageMembers: false,
      viewTenantBackends: false,
      viewOpsSurfaces: false,
    })
  })

  it('admin===true → server-admin with all flags (incl. viewTenants)', () => {
    expect(capabilitiesFor(whoami({ admin: true, role: 'owner' }))).toEqual({
      tier: 'server-admin',
      viewCorpus: true,
      viewTenants: true,
      manageTenantKeys: true,
      manageMembers: true,
      viewTenantBackends: true,
      viewOpsSurfaces: true,
    })
  })

  it('admin overrides role: admin===true with role=member is still server-admin (R4)', () => {
    const caps = capabilitiesFor(whoami({ admin: true, role: 'member' }))
    expect(caps.tier).toBe('server-admin')
    expect(caps.viewTenants).toBe(true)
  })

  it('role=owner → tenant-admin (manage tenant, ops surfaces; NOT viewTenants)', () => {
    expect(capabilitiesFor(whoami({ admin: false, role: 'owner' }))).toEqual({
      tier: 'tenant-admin',
      viewCorpus: true,
      viewTenants: false,
      manageTenantKeys: true,
      manageMembers: true,
      viewTenantBackends: true,
      viewOpsSurfaces: true,
    })
  })

  it('role=admin → tenant-admin (same surface as owner)', () => {
    const caps = capabilitiesFor(whoami({ admin: false, role: 'admin' }))
    expect(caps.tier).toBe('tenant-admin')
    expect(caps.viewOpsSurfaces).toBe(true)
    expect(caps.viewTenants).toBe(false)
  })

  it('role=member → member: corpus only, no tenant/ops flags', () => {
    expect(capabilitiesFor(whoami({ admin: false, role: 'member' }))).toEqual({
      tier: 'member',
      viewCorpus: true,
      viewTenants: false,
      manageTenantKeys: false,
      manageMembers: false,
      viewTenantBackends: false,
      viewOpsSurfaces: false,
    })
  })

  it('unknown role → member (least privilege, R3 forward-compat)', () => {
    expect(capabilitiesFor(whoami({ admin: false, role: 'superintendent' })).tier).toBe('member')
  })

  it('empty role → member (least privilege)', () => {
    expect(capabilitiesFor(whoami({ admin: false, role: '' })).tier).toBe('member')
  })

  it('flag containment: server-admin ⊇ tenant-admin ⊇ member', () => {
    const member = capabilitiesFor(whoami({ admin: false, role: 'member' }))
    const tenantAdmin = capabilitiesFor(whoami({ admin: false, role: 'owner' }))
    const serverAdmin = capabilitiesFor(whoami({ admin: true, role: 'owner' }))
    const flags = [
      'viewCorpus',
      'viewTenants',
      'manageTenantKeys',
      'manageMembers',
      'viewTenantBackends',
      'viewOpsSurfaces',
    ] as const
    for (const f of flags) {
      if (member[f]) expect(tenantAdmin[f]).toBe(true)
      if (tenantAdmin[f]) expect(serverAdmin[f]).toBe(true)
    }
  })
})
