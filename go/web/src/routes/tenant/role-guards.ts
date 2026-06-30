// Pure visibility-guards for the tenant key-table role/active controls (design
// 05-frontend-a3-selfservice §3 TK7b). These decide ONLY which controls render
// disabled — Sichtbarkeit ist Komfort, der Server bleibt autoritativ: the real
// last-owner protection + owner-only role delegation live in the api-key-update
// transaction (03-be6-roles SEC-7 / OF-2), which answers 409/403 even when a
// client guard let the action through (a concurrent-demote race). Kept here as a
// node-testable module because the web vitest is logic-only (no component DOM,
// vite.config.ts:test) — the component wiring is covered by the e2e spec.
//
// ownerCount caveat (design 05 §3 Korrektur): it is counted over the PASSED key
// list. For a tenant-admin that list is tenant-isolated server-side, so the count
// is the tenant's own owners. For a server-admin, /tenant lists keys store-wide
// (resolveApiKeyListParams leaves the tenant filter empty), so the count is global
// and this guard is only advisory there — the server transaction stays the gate.

import type { ApiKeyView } from '../../lib/api/types'

/** Active owners in the list — the last-owner guard's denominator. */
export function activeOwnerCount(keys: ApiKeyView[]): number {
  return keys.filter((k) => k.active && k.tenant_role === 'owner').length
}

/**
 * True iff k is the sole remaining ACTIVE owner: demoting or deactivating it would
 * orphan the tenant (no owner left). A revoked owner is not "active" so it never
 * counts — re-activating one stays open.
 */
export function isLastActiveOwner(k: ApiKeyView, ownerCount: number): boolean {
  return k.active && k.tenant_role === 'owner' && ownerCount <= 1
}

/**
 * Disable BOTH the role <select> and the active toggle of a row when:
 *  - a mutation on this row is already in flight (busy),
 *  - it is the caller's own key (no self-elevation / self-lockout from the UI —
 *    mirrors the existing self-revoke guard, TK5), or
 *  - it is the last active owner (no orphaning demote/deactivate).
 * Any other row (a member, a co-owner while ≥2 owners exist, a revoked non-owner)
 * stays editable. The predicate is identical for role and active by design — every
 * disabling reason applies to both mutations.
 */
export function controlDisabled(k: ApiKeyView, ownerCount: number, isOwn: boolean, busy: boolean): boolean {
  return busy || isOwn || isLastActiveOwner(k, ownerCount)
}
