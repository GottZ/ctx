// Limits panel form rules (RC-D C6, wave W05-12, design/05 §4.5 row
// "Limits", §7 gate; wire = C1 gateway-limits-get|set|delete). Pure and
// DOM-free. The SERVER is the authority (400 "limit widens: <field> — <why>" with
// param above the ceiling); these rules are the UX mirror so a tenant-admin
// is never offered what the server refuses:
//
//   - two tiers: a server-admin edits the `server` row, a tenant-admin the
//     `tenant` row (the tier is never sent — the server derives it);
//   - a tenant-admin's input maximum per dimension is the op's `ceiling`
//     (C1: effective without the consumer's own tenant row — a missing server
//     row is the setting's value, never unlimited, V-06-e). A null ceiling
//     dimension is genuinely unbounded (only monthly_tokens/daily_requests
//     with a settings floor of 0). An absent ceiling disables the field
//     (fail-closed: no bound known ⇒ nothing offered);
//   - there is NO "unlimited" control for anyone (no 0 sentinel; a
//     server-admin removes a row to inherit), and an empty tenant field means
//     "inherits the server limit", never "unbounded";
//   - limitFieldErrors maps a 400 envelope {error, param} onto the panel's
//     field keys "<op>.<param>" so the message stands at the field.

import { toApiError } from '../../lib/api'
import type { GatewayOp, LimitDimension, LimitTier, LimitsOpView } from '../../lib/api/types'

/** The numeric dimensions in display order. */
export const LIMIT_DIMENSIONS: readonly LimitDimension[] = [
  'rpm',
  'tpm',
  'max_concurrency',
  'daily_tokens',
  'monthly_tokens',
  'daily_requests',
  'monthly_cost_usd',
]

/** Dimensions stored as integers (everything but the cost budget). */
const INTEGER_DIMS = new Set<LimitDimension>(['rpm', 'tpm', 'max_concurrency', 'daily_tokens', 'monthly_tokens', 'daily_requests'])

/** How one numeric field of the form behaves for the caller's tier. */
export interface LimitFieldSpec {
  dim: LimitDimension
  /** The row the caller writes. */
  tier: LimitTier
  /** Current value of the caller-tier row (null = empty field). */
  value: number | null
  /** Input maximum: the ceiling for a tenant-admin (null = unbounded dimension); null for a server-admin. */
  max: number | null
  /** What an empty field means, as text ("inherits 30 (server limit)"); never "unlimited". */
  emptyMeans: string
  /** Whether an explicit "unlimited" control is offered — never (no 0 sentinel). */
  unlimitedOption: boolean
  /** true = no ceiling known for a tenant-admin: the field is not editable (fail-closed). */
  disabled: boolean
}

/** A limit value as text (cost with a dollar sign). */
export function fmtLimit(dim: LimitDimension, v: number): string {
  return dim === 'monthly_cost_usd' ? `$${v}` : v.toLocaleString('en-US')
}

/**
 * The ceiling of a tenant-admin write in one dimension: a number, null =
 * unbounded, undefined = no ceiling known (the get answer carried none).
 */
export function serverCeiling(view: LimitsOpView, dim: LimitDimension): number | null | undefined {
  if (view.ceiling === undefined) return undefined
  return view.ceiling[dim]
}

/** The form spec of one numeric field for the caller's tier. */
export function limitFieldSpec(view: LimitsOpView, dim: LimitDimension, tier: LimitTier): LimitFieldSpec {
  if (tier === 'server') {
    return {
      dim,
      tier,
      value: view.server?.[dim] ?? null,
      max: null,
      emptyMeans: 'inherits the tenant default or the setting',
      unlimitedOption: false,
      disabled: false,
    }
  }
  const ceiling = serverCeiling(view, dim)
  const value = view.tenant?.[dim] ?? null
  if (ceiling === undefined) {
    return { dim, tier, value, max: null, emptyMeans: 'no server limit known — not editable', unlimitedOption: false, disabled: true }
  }
  return {
    dim,
    tier,
    value,
    max: ceiling,
    emptyMeans: ceiling === null ? 'inherits the server limit (none set)' : `inherits ${fmtLimit(dim, ceiling)} (server limit)`,
    unlimitedOption: false,
    disabled: false,
  }
}

/**
 * A raw input the wire cannot carry: non-empty but not a finite number
 * (e.g. "1e400" ⇒ Infinity, which JSON turns into null — the row would be
 * cleared silently). Blocks the submit for every tier; everything else the
 * server decides.
 */
export function unsendableInput(raw: string): boolean {
  const t = raw.trim()
  return t !== '' && !Number.isFinite(Number(t))
}

/** Client-side check of one tenant-admin input (UX only): null = ok, else the message. */
export function tenantInputError(view: LimitsOpView, dim: LimitDimension, raw: string): string | null {
  const t = raw.trim()
  if (t === '') return null
  const v = Number(t)
  if (!Number.isFinite(v) || v <= 0) return 'must be greater than 0'
  if (INTEGER_DIMS.has(dim) && !Number.isInteger(v)) return 'must be a whole number'
  const ceiling = serverCeiling(view, dim)
  if (ceiling === undefined) return 'no server limit known'
  if (ceiling !== null && v > ceiling) return `at most ${fmtLimit(dim, ceiling)} (server limit)`
  return null
}

/** The field error of a gateway-limits-set refusal (400 with `param`), keyed "<op>.<param>". */
export function limitFieldErrors(err: unknown, op: GatewayOp): Record<string, string> {
  const e = toApiError(err)
  const param = e.details?.param
  if (e.status !== 400 || typeof param !== 'string' || param === '') return {}
  return { [`${op}.${param}`]: e.message }
}
