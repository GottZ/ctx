// Capability model for role-adaptive navigation (design 06-role-nav.md §3/§4,
// Welle N1). `capabilitiesFor(whoami)` is the SINGLE SOURCE OF TRUTH for the
// derived tier + surface flags that Nav-Rail, landing-redirect and route-guards
// consume — sichtbarkeit ist UX, die echte Autorisierung bleibt server-seitig
// (RequireAdmin/RequireAdminOrTenantAdmin), s. §1/R2.
//
// Tier precedence (R4): the server-global `admin` flag is orthogonal to the
// per-tenant `role` (a server-admin is role='owner' in the default tenant), so
// `admin===true` wins FIRST, then role. Unknown/empty role → 'member' (least
// privilege, forward-compat — same degradation as the SettingView unknown-type
// rule, types.ts:24-26 / R3). A null whoami (boot-time restore race, R6) →
// 'loading' with every flag false: no flickering rail, no redirect before the
// session resolves.
//
// Flag containment: server-admin ⊇ tenant-admin ⊇ member. The ops surfaces
// (Settings/Secrets/Status) are RequireAdminOrTenantAdmin per backend (§2a), so
// viewOpsSurfaces is true ab tenant-admin; viewTenants (cross-tenant admin) is
// server-admin only.

import type { WhoamiResponse } from '../api/types'

export type Tier = 'server-admin' | 'tenant-admin' | 'member' | 'loading'

export interface Capabilities {
  /** The single derived tier; everything below is a function of it. */
  tier: Tier
  /** Base corpus surface (search/ask, graph, chat, own blocks). False only while loading. */
  viewCorpus: boolean
  /** Cross-tenant admin (tenants/global-backends/grants). Server-admin only. */
  viewTenants: boolean
  /** Manage the own-tenant API keys. Tier ≥ tenant-admin. */
  manageTenantKeys: boolean
  /** Manage the own-tenant members. Tier ≥ tenant-admin. */
  manageMembers: boolean
  /** View the own-tenant backends. Tier ≥ tenant-admin. */
  viewTenantBackends: boolean
  /** Ops surfaces (Settings/Secrets/Status) — RequireAdminOrTenantAdmin (§2a). Tier ≥ tenant-admin. */
  viewOpsSurfaces: boolean
  /**
   * Issue/board workflow surface (design 04 §4.1.3, wave U04). NOT tier-derived
   * — a per-tenant feature flag from whoami.capabilities.workflow (B9). False
   * whenever the flag is absent (dark-launch) OR loading, so the nav section +
   * MemberHome tile stay hidden until the backend enables the surface. The
   * routes themselves are ungated (deep-link renders the EmptyState); this flag
   * is visibility only, like every other cap (R2 — real auth is server-side).
   */
  viewWorkflow: boolean
}

/** Per-tenant roles that map to the tenant-admin tier (owner manages the tenant, admin co-manages). */
const TENANT_ADMIN_ROLES = new Set(['owner', 'admin'])

/** All flags false — the loading floor, reused so the shape stays in one place. */
const NO_CAPS = {
  viewCorpus: false,
  viewTenants: false,
  manageTenantKeys: false,
  manageMembers: false,
  viewTenantBackends: false,
  viewOpsSurfaces: false,
  viewWorkflow: false,
} as const

/**
 * Derive the tier + surface flags from a whoami response (or null while the
 * session is restoring). Single source of truth — see file header for the
 * precedence (admin→role→least-privilege) and the loading floor.
 */
export function capabilitiesFor(whoami: WhoamiResponse | null): Capabilities {
  if (whoami === null) {
    return { tier: 'loading', ...NO_CAPS }
  }

  const tier: Tier = whoami.admin
    ? 'server-admin'
    : TENANT_ADMIN_ROLES.has(whoami.role)
      ? 'tenant-admin'
      : 'member' // role==='member' OR unknown/empty (R3 least privilege)

  const tenantAdminOrUp = tier === 'tenant-admin' || tier === 'server-admin'
  const serverAdmin = tier === 'server-admin'

  return {
    tier,
    viewCorpus: true,
    viewTenants: serverAdmin,
    manageTenantKeys: tenantAdminOrUp,
    manageMembers: tenantAdminOrUp,
    viewTenantBackends: tenantAdminOrUp,
    viewOpsSurfaces: tenantAdminOrUp,
    // Feature flag, not tier-derived: enabled for ANY resolved tier once the
    // backend sends it, absent (⇒ false) until then (dark-launch, §4.1.3).
    viewWorkflow: whoami.capabilities?.workflow === true,
  }
}
