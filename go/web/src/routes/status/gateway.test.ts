// status/gateway.ts gates (RC-D C6 S1, W05-17 — design/05 §7: "Mock:
// `gateway = null` ⇒ Kachel aus; tenant-Shape zeigt nur Tiefe-Klasse — rot
// gegen numerische Fremdtiefe"; V-03-b; the int/ext partition of A03-W3).
// Red against the S1 stubs (mode 'off', []).
// S2 (Go wins, C2 W06-16): there is ONE `gateway` field for both tiers (the
// tenant answer carries only its own consumers' figures) — the S1 assumption
// gateway/gateway_tenant is gone. The tile mode therefore comes from the
// field's presence AND the caller's tier, and the tenant tile's depth class
// comes from dispatch_tenant.targets (A03-W3), the existing coarse shape.

import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import type { DispatchStatus, DispatchTarget, DispatchTenantStatus, DispatchTenantTarget, GatewayStatus } from '../../lib/api/types'
import { expiringText, gatewayTileMode, partitionRows, tenantGatewayForeignFields, tenantTargetCells } from './gateway'

const ADMIN: GatewayStatus = {
  enabled: true,
  consumers: 7,
  reservations_open: 2,
  rejections_5m: { rate: 4 },
  instance_envelope: { chat: { used: 1200, limit: 600_000 } },
  top_consumers_1h: [],
  consumers_used_1h: 3,
  consumers_expiring: 2,
  auth: { invalid_key: 5 },
}

// the tenant answer of the same field: own consumers only, no envelope/auth/events
const TENANT_GATEWAY: GatewayStatus = {
  enabled: true,
  consumers: 2,
  reservations_open: 0,
  rejections_5m: {},
  top_consumers_1h: [],
  consumers_used_1h: 1,
  consumers_expiring: 0,
}

function tt(origin: string, depth: string): DispatchTenantTarget {
  return { origin, busy: depth !== 'leer', depth: depth as DispatchTenantTarget['depth'], own_waiting: 0, own_inflight: 0, own_oldest_wait_ms: 0 }
}

const TENANT_DISPATCH: DispatchTenantStatus = {
  targets: [tt('http://spark:30000', 'hoch'), tt('http://spark:30001', 'leer')],
}

describe('gatewayTileMode (W05-17: tile off on null)', () => {
  it('server-admin with the gateway section ⇒ admin tile (positive control)', () => {
    expect(gatewayTileMode({ gateway: ADMIN }, 'server-admin'), 'gate W05-17: tile shown for the admin shape').toBe('admin')
  })
  it('tenant-admin with the gateway section ⇒ tenant tile', () => {
    expect(gatewayTileMode({ gateway: TENANT_GATEWAY }, 'tenant-admin'), 'gate W05-17: tile shown for the tenant shape').toBe('tenant')
  })
  it('gateway = null ⇒ off for every tier; absent section (pre-RC-D server, switched off) ⇒ off; member ⇒ off', () => {
    expect(gatewayTileMode({ gateway: null }, 'server-admin'), 'gate W05-17: gateway=null ⇒ Kachel aus').toBe('off')
    expect(gatewayTileMode({ gateway: null }, 'tenant-admin')).toBe('off')
    expect(gatewayTileMode({}, 'server-admin')).toBe('off')
    expect(gatewayTileMode({ gateway: ADMIN }, 'member')).toBe('off')
  })
})

describe('tenant tile shows the depth class only (W05-17 / V-03-b)', () => {
  it('per target: origin + depth label', () => {
    expect(tenantTargetCells(TENANT_DISPATCH), 'gate W05-17: Tiefe-Klasse je Ziel').toEqual([
      { origin: 'http://spark:30000', depth: 'high' },
      { origin: 'http://spark:30001', depth: 'idle' },
    ])
  })

  it('a numeric foreign depth on the wire never reaches a cell (rot gegen Zahl)', () => {
    const leaky = {
      targets: [{ ...tt('http://spark:30000', 'niedrig'), waiting: 37, held_external: 5 }],
    } as unknown as DispatchTenantStatus
    const cells = tenantTargetCells(leaky)
    expect(cells, 'gate W05-17: only the class').toEqual([{ origin: 'http://spark:30000', depth: 'low' }])
    for (const c of cells) expect(c.depth, 'no number in a tenant depth cell').not.toMatch(/\d/)
    expect(tenantGatewayForeignFields(leaky.targets[0]!), 'gate V-03-b: foreign fields are detected').toEqual([
      'waiting',
      'held_external',
    ])
  })

  it('an unknown depth value reads "—", never echoed raw', () => {
    const odd = { targets: [{ origin: 'o', depth: '42' }] } as unknown as DispatchTenantStatus
    expect(tenantTargetCells(odd)).toEqual([{ origin: 'o', depth: '—' }])
  })
})

function target(over: Partial<DispatchTarget>): DispatchTarget {
  const wait = { waiting: 0, oldest_wait_ms: 0, p95_wait_ms: 0, max_wait_ms: 0, samples: 0 }
  return {
    origin: 'http://spark:30000',
    slots: 8,
    preempt_background: true,
    herald_scope: '',
    held: 0,
    inflight: 0,
    settling: 0,
    held_internal: 0,
    held_external: 0,
    inflight_internal: 0,
    internal_reserved: 4,
    external_offered: true,
    external_capacity: 4,
    external: { ...wait },
    external_rejects: { cap: 0, not_offered: 0 },
    external_bulk: { ...wait },
    external_bulk_rejects: { cap: 0, not_offered: 0 },
    interactive: { ...wait },
    background: { ...wait },
    preempt: {
      preempts_total: 0,
      wasted_ms_total: 0,
      release_ms_last: 0,
      release_ms_max: 0,
      forced_releases_total: 0,
      aged_admits_total: 0,
      aged_preempts_total: 0,
    },
    buckets: [],
    ...over,
  }
}

describe('partitionRows (W05-17: int/ext split from A03-W3)', () => {
  it('maps held/waiting per class and the reservation', () => {
    const t = target({
      held_internal: 3,
      held_external: 2,
      interactive: { waiting: 1, oldest_wait_ms: 10, p95_wait_ms: 120, max_wait_ms: 200, samples: 9 },
      external: { waiting: 4, oldest_wait_ms: 900, p95_wait_ms: 2500, max_wait_ms: 3000, samples: 9 },
      external_bulk: { waiting: 6, oldest_wait_ms: 1, p95_wait_ms: 1, max_wait_ms: 1, samples: 1 },
    })
    const d = { targets: [t] } as unknown as DispatchStatus
    expect(partitionRows(d), 'gate W05-17: int/ext-Split, Partition').toEqual([
      {
        origin: 'http://spark:30000',
        internalHeld: 3,
        internalWaiting: 1,
        internalReserved: 4,
        externalHeld: 2,
        xuWaiting: 4,
        xbWaiting: 6,
        externalCapacity: 4,
        p95InternalMs: 120,
        p95ExternalMs: 2500,
      },
    ])
  })
  it('no dispatch section ⇒ no rows', () => {
    expect(partitionRows(null)).toEqual([])
  })
})

describe('expiringText (W05-17: expiring keys)', () => {
  it('names the count; none ⇒ empty', () => {
    expect(expiringText(2), 'gate W05-17: ablaufende Keys').toBe('2 keys expire soon')
    expect(expiringText(1)).toBe('1 key expires soon')
    expect(expiringText(0)).toBe('')
  })
})

// "Hub-Last ≤ Rollup": the tile rides the status poll — the presentation
// module imports no API client and issues no request (structural ratchet,
// green by construction; red proof = overlay mut/w05-17-own-fetch.json).
describe('the tile issues no request of its own (W05-17 ratchet)', () => {
  it('status/gateway.ts and GatewayTile.svelte import no api module and call no fetch', () => {
    for (const f of ['./gateway.ts', './GatewayTile.svelte']) {
      const src = readFileSync(new URL(f, import.meta.url), 'utf8')
      expect(src, f).not.toMatch(/from '\.\.\/\.\.\/lib\/api(?!\/types)|\bfetch\(|apiFetch/)
    }
  })
})
