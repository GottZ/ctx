// Tenant structural-limit form logic (design 02-be5-tenant-quota / design 05 §1,
// the A3b tenant-limit-set form). The null↔unlimited mapping between the two
// editable text fields and the TenantLimitSpec ({max_scopes, max_keys}). DISTINCT
// axis from the per-scope cost/call QuotaForm (063, quota-form.svelte.ts): this is
// STRUCTURE (how many scopes/keys a tenant may own), not spend. A blank field is
// `null` (= that dimension is UNLIMITED), never 0; both fields are mandatory in the
// spec (the server requires both on tenant-limit-set). PURE — no runes / no DOM, so
// vitest covers it in the node env; the .svelte.ts suffix only keeps it next to the
// admin form + the sibling quota-form.svelte.ts.

import type { TenantLimitSpec } from '../../lib/api/types'

/** Upper bound for a structural limit (a sane FE guard against a fat-finger; the
 * server stays authoritative). A value above it is rejected before any request. */
export const MAX_TENANT_LIMIT = 1_000_000

/** A limit text field → its wire value. Blank/whitespace → `null` (unlimited),
 * NEVER 0. A non-numeric, negative, fractional (scopes/keys are counts) or
 * over-range (> MAX_TENANT_LIMIT) entry throws so the form blocks submit and shows
 * the message. */
export function parseTenantLimit(raw: string): number | null {
  const t = raw.trim()
  if (t === '') return null
  const n = Number(t)
  if (!Number.isFinite(n) || n < 0 || !Number.isInteger(n)) {
    throw new Error('enter a non-negative whole number, or leave blank for unlimited')
  }
  if (n > MAX_TENANT_LIMIT) {
    throw new Error(`limit must be at most ${MAX_TENANT_LIMIT}, or blank for unlimited`)
  }
  return n
}

/** A wire value → its text field. null/undefined (unlimited) renders as the empty
 * string — the inverse of parseTenantLimit's blank case, so a load→edit→save
 * round-trip is stable. A number renders as its decimal string. */
export function tenantLimitToField(v: number | null | undefined): string {
  return v === null || v === undefined ? '' : String(v)
}

/** The editable form state (both limits are strings: a blank string is the
 * unlimited state, distinct from "0" = ban all). */
export interface TenantLimitFields {
  maxScopes: string
  maxKeys: string
}

/** A tenant's stored limits → the editable fields (the seed on load + after a
 * save re-get). A null/absent field (unlimited) seeds blank. */
export function fieldsFromLimits(t: { max_scopes?: number | null; max_keys?: number | null }): TenantLimitFields {
  return {
    maxScopes: tenantLimitToField(t.max_scopes),
    maxKeys: tenantLimitToField(t.max_keys),
  }
}

/** The form fields → the tenant-limit-set payload. Blank fields collapse to null
 * (unlimited); an invalid field throws (via parseTenantLimit) before any request. */
export function toTenantLimitSpec(f: TenantLimitFields): TenantLimitSpec {
  return {
    max_scopes: parseTenantLimit(f.maxScopes),
    max_keys: parseTenantLimit(f.maxKeys),
  }
}
