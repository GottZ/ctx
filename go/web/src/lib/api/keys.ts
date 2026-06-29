// Tenant-admin API-key manage client (design 05 §3, Wave TK1) — the API keys ARE
// the tenant's member-proxys. Everything goes through the tier-gated
// (tierTenantAdmin) `POST /api/manage` actions (api-key-create/list/delete);
// there is no REST resource. One named function per action, mirroring the
// backends.ts manage pattern; apiFetch raises every {success:false} envelope as
// ApiError — including the api-key-delete 404-no-oracle "key not found" served
// inside an HTTP 200 (api.ts:103) and a 403 from enforceActionTier. getTenantQuota
// (read-only quota transparency, §1) is the A1 quota.ts binding, re-exported so
// this module is the single import surface for the whole tenant-admin area — it is
// NOT redeclared (same anti-duplication rule A1 set for the shared types).

import { apiFetch } from '../api'
import type { ApiKeyCreateResult, ApiKeyListResponse } from './types'

export { getTenantQuota } from './quota'

/** api-key-create payload (context_manage.go:1257 apiKeyCreateRequest, read from
 * req.Data). home_scope is REQUIRED (v2.0.0 — an empty value is a 400);
 * allowed_scopes is optional (a nil set takes the tenant-aware {shared} default
 * server-side, R-LEAK5). */
export interface ApiKeyCreateSpec {
  label: string
  home_scope: string
  allowed_scopes?: string[]
}

/** Mints a new key bound to the caller's tenant and returns the plaintext
 * `api_key` ONCE (never re-derivable). A non-server-admin may mint only scopes
 * its own tenant owns (firstScopeOutsideTenant → 403); the client sends regardless
 * and renders that 403 — it never gates the write on its own scope guess. */
export function createApiKey(spec: ApiKeyCreateSpec): Promise<ApiKeyCreateResult> {
  return apiFetch<ApiKeyCreateResult>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'api-key-create', data: spec }),
  })
}

/** Lists the caller's tenant keys (a server-admin sees every tenant). active_only
 * defaults to TRUE server-side (revoked keys hidden); pass false to include the
 * soft-deleted keys (the `show revoked` toggle). An omitted flag rides as no data
 * so the server applies its own default (a *bool tells absent from explicit
 * false, context_manage.go:1391). */
export function listApiKeys(activeOnly?: boolean): Promise<ApiKeyListResponse> {
  const req: Record<string, unknown> = { action: 'api-key-list' }
  if (activeOnly !== undefined) req.data = { active_only: activeOnly }
  return apiFetch<ApiKeyListResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify(req),
  })
}

/** Soft-deletes (revokes) one key by id, carried as `data.id` (NOT a top-level id
 * — context_manage.go:1404 unmarshals req.Data). Tenant-constrained + 404-no-
 * oracle: absent / foreign / inactive / malformed all answer the same
 * {success:false, error:"key not found"} (surfaced as ApiError) so it is never an
 * existence oracle for a foreign tenant's key. The server does NOT exclude the
 * caller's own key — the self-revoke guard is a client concern (TK5). */
export function deleteApiKey(id: string): Promise<{ success: true; deleted: string }> {
  return apiFetch<{ success: true; deleted: string }>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'api-key-delete', data: { id } }),
  })
}
