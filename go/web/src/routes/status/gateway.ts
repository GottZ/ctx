// "Model API" status tile presentation (RC-D C6, wave W05-17, design/05 §4.6
// GatewayTile, §7 gate; wire = C2 /api/status `gateway`). Pure formatters
// over the status answer — the tile issues NO request of its own (it rides
// the existing status poll/SSE: "Hub-Last ≤ Rollup").
//
//   gatewayTileMode(s, tier)  'admin' | 'tenant' | 'off': off when the answer
//                        carries no `gateway` section (null or absent = the
//                        gateway is switched off, no feed, or a server before
//                        RC-D) — the tile is then not drawn. The SAME field
//                        serves both tiers (C2); the tenant answer carries
//                        only its own consumers' figures.
//   tenantTargetCells(d) the tenant tile per target: origin + the coarse
//                        depth LABEL from dispatch_tenant (idle/low/high via
//                        dispatch.ts depthLabel) — never a number, even if
//                        the wire carried one (V-03-b).
//   tenantGatewayForeignFields(t) keys of a tenant target outside the
//                        sanctioned set (dispatch.ts tenantForeignFields).
//   partitionRows(d)     server-admin table "target · internal held/waiting
//                        · external held/waiting (XU/XB) · reserved · p95
//                        internal/external" from dispatch.targets (A03-W3).
//   expiringText(n)      the expiring-keys line ("2 keys expire soon").
//   rejectionsText(m)    the 5-min refusals as "quota 1, rate 4" ('none' when empty,
//                        'not recorded' when null — C2 feeds it later; null ≠ 0).
//   countText(n)         a nullable counter (reservations_open) — 'not recorded' for null.

import type { DispatchStatus, DispatchTenantStatus, DispatchTenantTarget, StatusResponse } from '../../lib/api/types'
import type { Tier } from '../../lib/auth/capabilities'
import { depthLabel, tenantForeignFields } from './dispatch'

export type GatewayTileMode = 'admin' | 'tenant' | 'off'

/** Which tile (if any) the status answer supports for the caller's tier. */
export function gatewayTileMode(s: Pick<StatusResponse, 'gateway'>, tier: Tier | null): GatewayTileMode {
  if (s.gateway === null || s.gateway === undefined) return 'off'
  if (tier === 'server-admin') return 'admin'
  if (tier === 'tenant-admin') return 'tenant'
  return 'off'
}

/** One tenant target cell: origin + depth label. */
export interface TenantTargetCell {
  origin: string
  depth: string
}

/** The tenant tile's per-target cells (depth class only). */
export function tenantTargetCells(d: DispatchTenantStatus | null | undefined): TenantTargetCell[] {
  return (d?.targets ?? []).map((t) => ({ origin: t.origin, depth: depthLabel(t.depth) }))
}

/** Keys of one tenant target outside the sanctioned coarse set. */
export function tenantGatewayForeignFields(t: DispatchTenantTarget): string[] {
  return tenantForeignFields(t)
}

/** One row of the server-admin partition table. */
export interface PartitionRow {
  origin: string
  internalHeld: number
  internalWaiting: number
  internalReserved: number
  externalHeld: number
  xuWaiting: number
  xbWaiting: number
  externalCapacity: number
  p95InternalMs: number
  p95ExternalMs: number
}

/** The server-admin internal/external partition per target. */
export function partitionRows(d: DispatchStatus | null | undefined): PartitionRow[] {
  return (d?.targets ?? []).map((t) => ({
    origin: t.origin,
    internalHeld: t.held_internal,
    internalWaiting: t.interactive.waiting,
    internalReserved: t.internal_reserved,
    externalHeld: t.held_external,
    xuWaiting: t.external.waiting,
    xbWaiting: t.external_bulk.waiting,
    externalCapacity: t.external_capacity,
    p95InternalMs: t.interactive.p95_wait_ms,
    p95ExternalMs: t.external.p95_wait_ms,
  }))
}

/** The expiring-keys line; '' when none expire. */
export function expiringText(n: number): string {
  if (!(n > 0)) return ''
  return n === 1 ? '1 key expires soon' : `${n} keys expire soon`
}

/** What a missing status value reads as (C2 null ≠ 0). */
export const NOT_RECORDED = 'not recorded'

/** A status counter that may be null (not fed yet): the number, or NOT_RECORDED — never "0" for null. */
export function countText(n: number | null | undefined): string {
  return typeof n === 'number' && Number.isFinite(n) ? n.toLocaleString('en-US') : NOT_RECORDED
}

/** The 5-minute refusals per reason as one line; null (no source yet) reads NOT_RECORDED, {} reads 'none'. */
export function rejectionsText(m: Record<string, number> | null | undefined): string {
  if (m === null || m === undefined) return NOT_RECORDED
  const parts = Object.entries(m)
    .filter(([, n]) => typeof n === 'number' && n > 0)
    .sort((a, b) => a[0].localeCompare(b[0]))
    .map(([r, n]) => `${r} ${n.toLocaleString('en-US')}`)
  return parts.length === 0 ? 'none' : parts.join(', ')
}
