// Tenant-lifecycle + cross-tenant scope-grant manage client (design 04 §3, Wave
// A1). There is no REST resource for tenants — everything goes through the
// admin-gated `POST /api/manage` actions (tenant-* + tenant-grant-*). One named
// function per action, mirroring the backends.ts manage pattern; apiFetch raises
// the {success:false} envelope (incl. a 403 from enforceActionTier) as ApiError.

import { apiFetch } from '../api'
import type {
  ScopeOverviewListResponse,
  TenantCreateResult,
  TenantDeleteResponse,
  TenantGrantDeleteResponse,
  TenantGrantListResponse,
  TenantGrantResponse,
  TenantLimitSpec,
  TenantListResponse,
  TenantResponse,
  TenantUsageResponse,
  TenantUsageView,
} from './types'

/** The seeded default tenant (mirrors store.DefaultTenantID). The server hard-
 * guards it (tenant-delete → 400, tenant_manage.go:159-161); this FE constant
 * only drives the Delete-disabled visibility on TenantDetail (A3b). */
export const DEFAULT_TENANT_ID = '00000000-0000-0000-0000-0000000d3fa0'

/** tenant-create payload (tenant_manage.go:32 tenantSpec). max_scopes/max_keys
 * are the BE5 structural Tenant-Limits (design 02-be5-tenant-quota), seeded at
 * create; null OR omitted = unlimited for that dimension (063 NULL-unlimited). */
export interface TenantSpec {
  slug: string
  display_name: string
  max_scopes?: number | null
  max_keys?: number | null
}

/** tenant-update patch: status is a TOP-LEVEL manage field (req.Status), the
 * optional display_name rides in `data` — exactly as handleTenantUpdate reads it
 * (tenant_manage.go:105-114). At least one of the two must be set (else 400). */
export interface TenantPatch {
  status?: string
  display_name?: string
}

/** Compound create (atomic, design 03-be6-roles §6): the tenant row + an initial
 * auto-prefixed scope + a minted owner-key (role 'owner') — resolving the K10
 * inert-tenant gap. The FLAT result carries the owner-key PLAINTEXT in `owner_key`
 * (reveal-once, RevealOnceKey hygiene — never persisted on a model/storage/URL),
 * the full server-built `scope`, and `owner_key_id`; it is NOT nested. */
export function createTenant(spec: TenantSpec): Promise<TenantCreateResult> {
  return apiFetch<TenantCreateResult>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'tenant-create', data: spec }),
  })
}

export function listTenants(): Promise<TenantListResponse> {
  return apiFetch<TenantListResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'tenant-list' }),
  })
}

/** Server-admin scope landscape: one row per scope (block + key counts + owning
 * tenant) across the whole store. tierServerAdmin-gated server-side
 * (handler/scope_overview.go); the counts are deliberately unscoped (no block
 * content leaves the store). Feeds the ScopeMap panel (A0-FE), and later the
 * tenant→scope mapping the QuotaForm needs + the delete blast-radius count. */
export function scopeOverview(): Promise<ScopeOverviewListResponse> {
  return apiFetch<ScopeOverviewListResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'scope-overview' }),
  })
}

export function getTenant(id: string): Promise<TenantResponse> {
  return apiFetch<TenantResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'tenant-get', id }),
  })
}

export function updateTenant(id: string, patch: TenantPatch): Promise<TenantResponse> {
  const req: Record<string, unknown> = { action: 'tenant-update', id }
  if (patch.status !== undefined) req.status = patch.status
  if (patch.display_name !== undefined) req.data = { display_name: patch.display_name }
  return apiFetch<TenantResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify(req),
  })
}

/** Full-prune (irreversible): all scope-carried data + keys + the tenant row.
 * The default tenant is server-guarded (400). The Slug-typing confirm lives in
 * the UI (A3) — this binding is the raw call. */
export function deleteTenant(id: string): Promise<TenantDeleteResponse> {
  return apiFetch<TenantDeleteResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'tenant-delete', id }),
  })
}

/** Reads usage + structural limits for the /tenant self-service quota visibility
 * (S3, design 05 §1). A server-admin targets any tenant via the top-level `id`; a
 * tenant-admin OMITS it and is server-pinned to ar.TenantID (req.ID is read only
 * when IsServerAdmin — a foreign id is ignored/403, never leaking another
 * tenant's counts). Unwraps the envelope to the view; null fields = unlimited. */
export async function getTenantUsage(id?: string): Promise<TenantUsageView> {
  const req: Record<string, unknown> = { action: 'tenant-usage-get' }
  if (id !== undefined) req.id = id
  const res = await apiFetch<TenantUsageResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify(req),
  })
  return res.usage
}

/** Sets a tenant's structural limits (server-admin only, design 02-be5-tenant-
 * quota). REPLACE-semantics like QuotaForm: BOTH max_scopes and max_keys are
 * mandatory (server 400 otherwise), null = unlimited for that dimension, a number
 * = the cap. `id` is a TOP-LEVEL manage field; the spec rides in `data` — exactly
 * as handleTenantLimitSet reads it. */
export function setTenantLimits(id: string, spec: TenantLimitSpec): Promise<TenantResponse> {
  return apiFetch<TenantResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'tenant-limit-set', id, data: spec }),
  })
}

/** tenant-grant-create payload (tenant_manage.go:183 grantSpec). */
export interface TenantGrantSpec {
  grantee_tenant: string
  granted_scope: string
}

/** Cross-tenant READ grant: grantee_tenant gains read access to granted_scope
 * (owned by another tenant). Effective at the grantee's NEXT auth — the UI wraps
 * this in an exfiltration confirm (A5). */
export function createTenantGrant(spec: TenantGrantSpec): Promise<TenantGrantResponse> {
  return apiFetch<TenantGrantResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'tenant-grant-create', data: spec }),
  })
}

/** Lists tenant grants. The list IS global (the central "who reads whose scope"
 * view); an optional granteeTenant narrows to one grantee (req.ID filter,
 * tenant_manage.go:220-222). */
export function listTenantGrants(granteeTenant?: string): Promise<TenantGrantListResponse> {
  const req: Record<string, unknown> = { action: 'tenant-grant-list' }
  if (granteeTenant !== undefined) req.id = granteeTenant
  return apiFetch<TenantGrantListResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify(req),
  })
}

export function deleteTenantGrant(id: string): Promise<TenantGrantDeleteResponse> {
  return apiFetch<TenantGrantDeleteResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'tenant-grant-delete', id }),
  })
}
