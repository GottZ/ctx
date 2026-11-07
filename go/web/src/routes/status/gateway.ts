// "Model API" status tile presentation (RC-D C6, wave W05-17, design/05 §4.6
// GatewayTile, §7 gate; design/06 §4.8). Pure formatters over the /api/status
// answer — the tile issues NO request of its own (it rides the existing
// status poll/SSE: "Hub-Last ≤ Rollup").
//
//   gatewayTileMode(s)   'admin' | 'tenant' | 'off': off when the answer
//                        carries neither `gateway` nor `gateway_tenant`
//                        (null or absent = a server before RC-D or a caller
//                        without the section) — the tile is then not drawn.
//   tenantTargetCells(t) the tenant shape per target: origin + the coarse
//                        depth LABEL (idle/low/high via dispatch.ts
//                        depthLabel) — never a number, even if the wire
//                        carried one (V-03-b).
//   tenantGatewayForeignFields(t) keys of the tenant shape outside the
//                        sanctioned set (exposure test, like
//                        dispatch.ts tenantForeignFields).
//   partitionRows(d)     server-admin table "target · internal held/waiting
//                        · external held/waiting (XU/XB) · reserved · p95
//                        external/internal" from dispatch.targets (A03-W3).
//   expiringText(n)      the expiring-keys line ("2 keys expire soon").
//
// RC-D S1: contract stubs — mode 'off', empty rows/cells.

import type {
  DispatchStatus,
  GatewayTenantStatus,
  GatewayTenantTarget,
  StatusResponse,
} from '../../lib/api/types'

export type GatewayTileMode = 'admin' | 'tenant' | 'off'

/** Which tile (if any) the status answer supports. */
export function gatewayTileMode(s: Pick<StatusResponse, 'gateway' | 'gateway_tenant'>): GatewayTileMode {
  void s
  return 'off'
}

/** One tenant-shape target cell: origin + depth label. */
export interface TenantTargetCell {
  origin: string
  depth: string
}

/** The tenant shape's per-target cells (depth class only). */
export function tenantTargetCells(t: GatewayTenantStatus): TenantTargetCell[] {
  void t
  return []
}

/** Keys of one tenant-shape target outside {origin, depth}. */
export function tenantGatewayForeignFields(t: GatewayTenantTarget): string[] {
  void t
  return []
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
  void d
  return []
}

/** The expiring-keys line; '' when none expire. */
export function expiringText(n: number): string {
  void n
  return ''
}
