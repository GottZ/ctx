// Per-tenant quota manage client (design 04 §3, Wave A1) — tenant-quota-get/set
// (quota_manage.go, MT T36b). KEYED BY SCOPE, never by tenant-id: a tenant owns
// 0..N scopes and each scope has its own quota row. A null cost/calls field =
// that dimension is unlimited (the server stores a nil *float64/*int). set is
// server-admin only; get admits a tenant-admin for its OWN scope.

import { apiFetch } from '../api'
import type { QuotaSpec, TenantQuotaResponse } from './types'

/** Reads one scope's quota. A server-admin targets any scope via `data.scope`; a
 * tenant-admin is server-pinned to its own home_scope (the payload is ignored).
 * Omitting scope lets the server default to the caller's own scope
 * (quota_manage.go:64-70). */
export function getTenantQuota(scope?: string): Promise<TenantQuotaResponse> {
  const req: Record<string, unknown> = { action: 'tenant-quota-get' }
  if (scope !== undefined) req.data = { scope }
  return apiFetch<TenantQuotaResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify(req),
  })
}

/** Writes one scope's quota (server-admin only). `scope` is required and must be
 * a real tenant scope (never _global or a '_'-reserved scope → 400). A set
 * refreshes the accountant synchronously, so it takes effect at once. */
export function setTenantQuota(spec: QuotaSpec): Promise<TenantQuotaResponse> {
  return apiFetch<TenantQuotaResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'tenant-quota-set', data: spec }),
  })
}
