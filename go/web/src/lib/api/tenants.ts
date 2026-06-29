// Tenant-lifecycle + cross-tenant scope-grant manage client (design 04 §3, Wave
// A1). There is no REST resource for tenants — everything goes through the
// admin-gated `POST /api/manage` actions (tenant-* + tenant-grant-*). One named
// function per action, mirroring the backends.ts manage pattern; apiFetch raises
// the {success:false} envelope (incl. a 403 from enforceActionTier) as ApiError.

import { apiFetch } from '../api'
import type {
  ScopeOverviewListResponse,
  TenantDeleteResponse,
  TenantGrantDeleteResponse,
  TenantGrantListResponse,
  TenantGrantResponse,
  TenantListResponse,
  TenantResponse,
} from './types'

/** tenant-create payload (tenant_manage.go:32 tenantSpec). */
export interface TenantSpec {
  slug: string
  display_name: string
}

/** tenant-update patch: status is a TOP-LEVEL manage field (req.Status), the
 * optional display_name rides in `data` — exactly as handleTenantUpdate reads it
 * (tenant_manage.go:105-114). At least one of the two must be set (else 400). */
export interface TenantPatch {
  status?: string
  display_name?: string
}

export function createTenant(spec: TenantSpec): Promise<TenantResponse> {
  return apiFetch<TenantResponse>('/api/manage', {
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
