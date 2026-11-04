// Tier-derivation table (design 06-role-nav.md §4 N1-Gate). Covers the three
// real tiers, unknown-role→member (R3 least privilege), the admin+member combo
// that proves admin overrides role (R4), and null→loading (R6). The flag
// containment server-admin ⊇ tenant-admin ⊇ member is asserted per row. The
// model-API caps (design/05 §4.6, wave W05-01) get their own table: tier ×
// whoami.capabilities.model_api.{enabled, self_service}, mirroring the server's
// tierSelfService gate (V-01-c) — the SPA never opens what the server refuses.

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
      viewWorkflow: false,
      viewModelApi: false,
      mintModelKeys: false,
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
      viewWorkflow: false, // feature flag off unless whoami carries it (dark-launch)
      // server-admin sees and mints model keys before the gateway is switched on
      // (design/05 §4.6: alias catalog and ctx-peer consumers are prepared ahead of the flip).
      viewModelApi: true,
      mintModelKeys: true,
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
      viewWorkflow: false,
      viewModelApi: false, // no model_api from the server ⇒ gateway closed for tenant-admins
      mintModelKeys: false,
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
      viewWorkflow: false,
      viewModelApi: false,
      mintModelKeys: false,
    })
  })

  it('viewWorkflow follows the whoami feature flag, not the tier (design 04 §4.1.3, U04)', () => {
    // Absent flag → false for every tier (dark-launch).
    expect(capabilitiesFor(whoami({ admin: false, role: 'member' })).viewWorkflow).toBe(false)
    expect(capabilitiesFor(whoami({ admin: true, role: 'owner' })).viewWorkflow).toBe(false)
    // Flag on → true even for a plain member (it is a per-tenant feature, not a tier).
    expect(
      capabilitiesFor(whoami({ admin: false, role: 'member', capabilities: { workflow: true } })).viewWorkflow,
    ).toBe(true)
    // Explicit false or an empty capabilities bag → false.
    expect(
      capabilitiesFor(whoami({ admin: false, role: 'member', capabilities: { workflow: false } })).viewWorkflow,
    ).toBe(false)
    expect(capabilitiesFor(whoami({ admin: false, role: 'member', capabilities: {} })).viewWorkflow).toBe(false)
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

// The server policy whoami carries (handler/whoami.go WhoamiModelAPI); the
// seconds are arbitrary here — the caps read only the two switches.
function modelApi(enabled: boolean, selfService: boolean) {
  return {
    model_api: {
      enabled,
      self_service: selfService,
      key_max_lifetime_s: 3600,
      rotation_grace_s: 60,
      expiry_warn_s: 600,
    },
  }
}

const TIERS = {
  'server-admin': { admin: true, role: 'owner' },
  // R4: the server-global flag wins over the role (auth.IsServerAdmin ignores
  // it), so the caps follow the tier, never the role alone (review F-2).
  'server-admin (role member)': { admin: true, role: 'member' },
  'tenant-admin (owner)': { admin: false, role: 'owner' },
  'tenant-admin (admin)': { admin: false, role: 'admin' },
  member: { admin: false, role: 'member' },
} as const

describe('capabilitiesFor — model API (design/05 §4.6, W05-01)', () => {
  // Full matrix tier × enabled × self_service. mintModelKeys mirrors the
  // server: model-consumer-create/model-key-rotate pass a server-admin always,
  // a tenant-admin only with gateway.enabled ∧ gateway.tenant_self_service
  // (tierSelfService, V-01-c), a member never. viewModelApi opens the area for
  // a tenant-admin once the gateway is on; the server-admin sees it before.
  it.each([
    ['server-admin', false, false, true, true],
    ['server-admin', false, true, true, true],
    ['server-admin', true, false, true, true],
    ['server-admin', true, true, true, true],
    ['server-admin (role member)', false, false, true, true],
    ['server-admin (role member)', false, true, true, true],
    ['server-admin (role member)', true, false, true, true],
    ['server-admin (role member)', true, true, true, true],
    ['tenant-admin (owner)', false, false, false, false],
    ['tenant-admin (owner)', false, true, false, false], // self-service alone opens nothing (V-01-c)
    ['tenant-admin (owner)', true, false, true, false],
    ['tenant-admin (owner)', true, true, true, true],
    ['tenant-admin (admin)', false, false, false, false],
    ['tenant-admin (admin)', false, true, false, false],
    ['tenant-admin (admin)', true, false, true, false],
    ['tenant-admin (admin)', true, true, true, true],
    ['member', false, false, false, false],
    ['member', false, true, false, false],
    ['member', true, false, false, false],
    ['member', true, true, false, false], // flags on, tier still member
  ] as const)('%s, enabled=%s, self_service=%s → viewModelApi=%s, mintModelKeys=%s', (tier, enabled, selfService, view, mint) => {
    const caps = capabilitiesFor(whoami({ ...TIERS[tier], capabilities: modelApi(enabled, selfService) }))
    expect(caps.viewModelApi).toBe(view)
    expect(caps.mintModelKeys).toBe(mint)
  })

  it('older server without model_api → both false below server-admin', () => {
    for (const capabilities of [undefined, {}, { workflow: true }]) {
      const ta = capabilitiesFor(whoami({ admin: false, role: 'owner', capabilities }))
      expect(ta.viewModelApi).toBe(false)
      expect(ta.mintModelKeys).toBe(false)
      const sa = capabilitiesFor(whoami({ admin: true, role: 'owner', capabilities }))
      expect(sa.viewModelApi).toBe(true)
      expect(sa.mintModelKeys).toBe(true)
    }
  })

  it('only a literal true opens the switches (a truthy non-boolean is closed)', () => {
    const caps = capabilitiesFor(
      whoami({
        admin: false,
        role: 'owner',
        capabilities: {
          model_api: {
            enabled: 'true' as unknown as boolean,
            self_service: 1 as unknown as boolean,
            key_max_lifetime_s: 0,
            rotation_grace_s: 0,
            expiry_warn_s: 0,
          },
        },
      }),
    )
    expect(caps.viewModelApi).toBe(false)
    expect(caps.mintModelKeys).toBe(false)
  })

  it('loading → both false', () => {
    const caps = capabilitiesFor(null)
    expect(caps.viewModelApi).toBe(false)
    expect(caps.mintModelKeys).toBe(false)
  })

  it('containment: mintModelKeys ⇒ viewModelApi, and server-admin ⊇ tenant-admin ⊇ member', () => {
    for (const enabled of [false, true]) {
      for (const selfService of [false, true]) {
        const at = (tier: keyof typeof TIERS) =>
          capabilitiesFor(whoami({ ...TIERS[tier], capabilities: modelApi(enabled, selfService) }))
        for (const tier of Object.keys(TIERS) as (keyof typeof TIERS)[]) {
          const caps = at(tier)
          if (caps.mintModelKeys) expect(caps.viewModelApi).toBe(true)
        }
        for (const f of ['viewModelApi', 'mintModelKeys'] as const) {
          if (at('member')[f]) expect(at('tenant-admin (owner)')[f]).toBe(true)
          if (at('tenant-admin (owner)')[f]) expect(at('server-admin')[f]).toBe(true)
        }
      }
    }
  })
})
