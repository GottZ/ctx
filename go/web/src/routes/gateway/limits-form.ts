// Limits panel form rules (RC-D C6, wave W05-12, design/05 §4.5 row
// "Limits", §7 gate; design/06 §4.3). Pure and DOM-free. The SERVER is the
// authority (422 with fields[] above the ceiling); these rules are the UX
// mirror so a tenant-admin is never offered what the server refuses:
//
//   - two tiers: a server-admin edits the `server` row, a tenant-admin the
//     `tenant` row (the tier is never sent — the server derives it);
//   - a tenant-admin's input maximum per dimension is server(d) =
//     server[d] ?? defaults[d]; NO "unlimited" option exists for a
//     tenant-admin, and an empty tenant field means "inherits server(d)",
//     never "unbounded" (V-06-e: a missing server row is the registry value);
//   - allow_external: a tenant-admin may only switch it off (CHECK
//     ck_gl_tenant_narrow); allow_unmetered_external is server-admin only;
//   - limitFieldErrors maps a 422 envelope fields:[{field, message}] onto the
//     panel's field keys "<op>.<field>" so the message stands at the field.
//
// RC-D S1: contract stubs — answers are fail-closed (no max, no options, no
// field errors).

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

/** How one numeric field of the form behaves for the caller's tier. */
export interface LimitFieldSpec {
  dim: LimitDimension
  /** The row the caller writes. */
  tier: LimitTier
  /** Current value of the caller-tier row (null = empty field). */
  value: number | null
  /** Input maximum: server(d) for a tenant-admin; null = no maximum (server-admin). */
  max: number | null
  /** What an empty field means, as text ("inherits 30/min (server)"); never "unlimited" for a tenant-admin. */
  emptyMeans: string
  /** Whether an explicit "unlimited" control is offered (never for a tenant-admin). */
  unlimitedOption: boolean
}

/** server(d) — the ceiling of a tenant-admin write (server row ?? defaults). null = unbounded registry value. */
export function serverCeiling(view: LimitsOpView, dim: LimitDimension): number | null {
  void view
  void dim
  return null
}

/** The form spec of one numeric field for the caller's tier. */
export function limitFieldSpec(view: LimitsOpView, dim: LimitDimension, tier: LimitTier): LimitFieldSpec | null {
  void view
  void dim
  void tier
  return null
}

/** Client-side check of one tenant-admin input (UX only): null = ok, else the message. */
export function tenantInputError(view: LimitsOpView, dim: LimitDimension, raw: string): string | null {
  void view
  void dim
  void raw
  return null
}

/** The 422 field errors of a gateway-limits-set refusal, keyed "<op>.<field>". */
export function limitFieldErrors(err: unknown, op: GatewayOp): Record<string, string> {
  void err
  void op
  return {}
}
