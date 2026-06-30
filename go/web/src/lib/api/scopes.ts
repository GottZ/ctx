// Self-service scope manage client (masterplan §K1/K4 BE5-3/BE5-4, Wave FE-2).
// A tenant-admin provisions its OWN scopes through the tier-gated
// (tierTenantAdmin) `POST /api/manage` actions; there is no REST resource. The
// client sends ONLY the bare `name` — the server builds the full
// `<tenant_slug>:<name>` from ar.TenantID (S1: prefix injection is structurally
// impossible client-side) and echoes the full scope back. One named function per
// action, mirroring the keys.ts/tenants.ts manage pattern; apiFetch raises every
// {success:false} envelope (incl. a 400 name-charset reject, a 429 quota cap and
// a 403 from enforceActionTier) as ApiError.

import { apiFetch } from '../api'
import type { ScopeCreateResult, ScopeCreateSpec, ScopeOverviewListResponse } from './types'

/** Provisions ONE new scope for the caller's tenant. The client sends only the
 * bare `name`; the server prepends `<tenant_slug>:` from ar.TenantID (S1) and
 * returns the FULL name in ScopeCreateResult.scope — render that, never a
 * client-built prefix. tenant_id is a server-admin-only override (A3c: provision
 * for a foreign tenant); a non-server-admin's tenant_id is ignored/rejected
 * server-side. A name carrying `:`/whitespace/Unicode is a server 400; a scope
 * over the tenant's max_scopes is a 429 — both surface as ApiError. */
export function createScope(spec: ScopeCreateSpec): Promise<ScopeCreateResult> {
  return apiFetch<ScopeCreateResult>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'scope-create', data: spec }),
  })
}

/** Lists the caller's OWN tenant scopes (tierTenantAdmin, server-filtered on
 * ar.TenantID — cross-tenant scopes are not enumerable). Distinct from the
 * server-admin `scope-overview` (deliberately unscoped, whole-store): this is the
 * tenant-scoped read that feeds the /tenant scope list + the re-list after a
 * createScope. Rows are ScopeOverview-shaped (reused envelope). No payload. */
export function listScopes(): Promise<ScopeOverviewListResponse> {
  return apiFetch<ScopeOverviewListResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'scope-list' }),
  })
}
