// QuotaForm pure logic (design 04 §3 A4, Wave A4) — the null↔unlimited mapping
// between the editable text fields and the scope-keyed tenant-quota-set payload.
// SCOPE-keyed, never tenant-id-keyed (quota_manage.go:94): a tenant owns 0..N
// scopes, each with its own quota row. A blank limit field is `null` (= that
// dimension is UNLIMITED), never 0 — the *float64/*int pointer is stored nil.
// No runes / no DOM here so vitest covers it in the node env (this is the A4
// gate); the .svelte.ts suffix only keeps it next to its component.

import type { QuotaOnExceed, QuotaSpec, TenantQuotaView } from '../../lib/api/types'

/** The on_exceed domain (063 CHECK). external_off is FIRST = the form default:
 * a budget-exhausted tenant degrades to local backends; block hard-errors. */
export const ON_EXCEED_OPTIONS: readonly QuotaOnExceed[] = ['external_off', 'block']

/** True iff v is a member of the on_exceed domain (forward-compat guard for a
 * value read back off the wire). */
export function isOnExceed(v: unknown): v is QuotaOnExceed {
  return v === 'external_off' || v === 'block'
}

/** A limit text field → its wire value. A blank/whitespace field is `null` (the
 * dimension is unlimited), NEVER 0. A non-numeric or negative entry throws so the
 * form blocks submit and shows the message (type=text + inputmode keeps the
 * keyboard numeric without forcing Svelte's number-binding coercion). `integer`
 * additionally rejects a fractional value (daily_calls is an *int server-side). */
export function parseLimit(raw: string, opts: { integer?: boolean } = {}): number | null {
  const t = raw.trim()
  if (t === '') return null
  const n = Number(t)
  if (!Number.isFinite(n) || n < 0) {
    throw new Error('enter a non-negative number, or leave blank for unlimited')
  }
  if (opts.integer && !Number.isInteger(n)) {
    throw new Error('enter a whole number of calls, or leave blank for unlimited')
  }
  return n
}

/** A wire value → its text field. null/undefined (unlimited) renders as the empty
 * string — the inverse of parseLimit's blank case, so a load→edit→save round-trip
 * is stable. A number renders as its decimal string. */
export function limitToField(v: number | null | undefined): string {
  return v === null || v === undefined ? '' : String(v)
}

/** The editable form state (all limits are strings: a blank string is the
 * unlimited state, distinct from "0"). */
export interface QuotaFormFields {
  enabled: boolean
  dailyCost: string
  monthlyCost: string
  dailyCalls: string
  onExceed: QuotaOnExceed
}

/** A loaded quota view → the editable fields (the seed on load and after a save
 * re-get). A nil-policy view ({enabled:false, unlimited:true}) carries no budget
 * fields → every limit seeds blank. */
export function fieldsFromView(v: TenantQuotaView): QuotaFormFields {
  return {
    enabled: v.enabled,
    dailyCost: limitToField(v.daily_cost_usd),
    monthlyCost: limitToField(v.monthly_cost_usd),
    dailyCalls: limitToField(v.daily_calls),
    onExceed: isOnExceed(v.on_exceed) ? v.on_exceed : 'external_off',
  }
}

/** The form fields + the bound scope → the tenant-quota-set payload. scope is
 * MANDATORY — the action 400s without a real tenant scope (quota_manage.go:94),
 * so an empty scope throws here before any request. Blank limit fields collapse
 * to null (unlimited); daily_calls is parsed as an integer. */
export function toQuotaSpec(scope: string, f: QuotaFormFields): QuotaSpec {
  if (scope.trim() === '') {
    throw new Error('scope is required')
  }
  return {
    scope,
    enabled: f.enabled,
    daily_cost_usd: parseLimit(f.dailyCost),
    monthly_cost_usd: parseLimit(f.monthlyCost),
    daily_calls: parseLimit(f.dailyCalls, { integer: true }),
    on_exceed: f.onExceed,
  }
}
