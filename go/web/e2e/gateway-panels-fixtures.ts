// e2e mocks of the Model API panels (RC-D cluster C6: W05-09 consumer detail,
// W05-10 usage panel, W05-12 limits panel, W05-17 status tile; design/05 §4.6,
// design/06 §4.3/§4.8). The Go side of these calls lands in other RC-D
// clusters (model-consumer-get on root; /api/llmlog?consumer_id + /api/usage +
// /api/status gateway sections = C2; gateway-limits-* = C1). Until the S3
// integration this file IS the server for the SPA — each mock follows the
// contract in src/lib/api/types.ts block "// RC-D C6" and the server rules
// the panels must survive:
//   - No-Oracle: a foreign or unknown consumer id answers the byte-equal
//     404 {success:false, error:'model consumer not found'} (handleModelConsumerGet).
//   - The tenant filter is the SERVER's: a tenant-admin sees only its own
//     tenant's consumers, calls, usage and limits.
//   - Usage carries the hard cases on purpose: a complete:false bucket whose
//     wire values are 0, a null value, hist:false, and an auth refusal COUNT
//     on a consumer series (a server bug the panel must not render, V-06-b).
//   - gateway-limits-set derives the tier from the caller and answers 422 with
//     fields[] above server(d) for a tenant-admin.
//   - /api/status carries exactly one of gateway / gateway_tenant per role,
//     and a tenant target with a numeric `waiting` leak the tile must drop.
// Kept apart from fixtures.ts so the RC-D SPA clusters C5/C6 do not edit the
// same mock body; fixtures.ts calls in through three hooks.

import type { Role, TenantKey } from './fixtures'
import { SENTINEL } from './fixtures'

/** The detail consumer of tenant A — a rotated app consumer (three keys). */
export const PANEL_CONSUMER_A = '0190000000007000800000000000c0a6'
/** Tenant B's own detail consumer (label = SENTINEL.B — the leak probe's positive control). */
export const PANEL_CONSUMER_B = '0190000000007000800000000000c0b3'
/** The tenant ids of the fixture world (fixtures.ts TENANTS). */
export const PANEL_TENANT_ID: Record<TenantKey, string> = {
  A: '550e8400-e29b-41d4-a716-446655440aaa',
  B: '550e8400-e29b-41d4-a716-446655440bbb',
}
/** The seeded call of the detail consumer (W05-09 positive control). */
export const PANEL_CALL_ID = '0190000000007000800000000000L0a6'

/** Seed switches of the panel world. */
export interface PanelSeed {
  /** /api/status gateway sections: 'on' = the role's shape, default absent (pre-RC-D server). */
  gatewayStatus?: 'on' | 'null'
}

interface MockAnswer {
  status: number
  json: unknown
}

// writeConsumerStoreError (internal/handler/model_consumers.go:304-305): 404 + the one text for every case
const NOT_FOUND: MockAnswer = { status: 404, json: { success: false, error: 'model consumer not found' } }

function key(id: string, hint: string, status: string, created: string, rotatedFrom: string | null, revoked: string | null) {
  return { id, hint, status, created_at: created, expires_at: null, revoked_at: revoked, rotated_from: rotatedFrom, last_used_at: null }
}

function consumerView(id: string, tenant: TenantKey, label: string, keys: ReturnType<typeof key>[]) {
  return {
    id,
    tenant_id: PANEL_TENANT_ID[tenant],
    tenant_slug: tenant === 'A' ? 'acme' : 'globex',
    label,
    description: 'rotated twice — chain fixture',
    kind: 'app',
    tenant_scope: null,
    all_models: false,
    models: [],
    prompt_sensitivity: 'credentials',
    fair_weight: 1,
    admission_lane: 'bulk',
    allowed_cidrs: null,
    allow_plaintext: false,
    active: true,
    status: 'rotating',
    last_used_at: '2026-06-29T11:58:00Z',
    created_at: '2026-06-01T10:00:00Z',
    created_by_label: null,
    keys,
    keys_total: keys.length,
    peer: null,
  }
}

// k3 (active) ← k2 (superseded, in grace) ← k1 (revoked at the first rotation)
const CHAIN_A = [
  key('k3a6', 'ctxm_…CCC3', 'active', '2026-06-20T10:00:00Z', 'k2a6', null),
  key('k2a6', 'ctxm_…BBB2', 'superseded', '2026-06-10T10:00:00Z', 'k1a6', null),
  key('k1a6', 'ctxm_…AAA1', 'revoked', '2026-06-01T10:00:00Z', null, '2026-06-10T10:00:01Z'),
]

function auditRow(id: string, type: string, entity: string, action: string, at: string, oldV: unknown, newV: unknown) {
  return { entity_type: type, id, entity_key: entity, scope: '', action, old_value: oldV, new_value: newV, api_key_id: null, actor_label: 'ops-key', metadata: { via: 'manage' }, created_at: at }
}

const AUDIT_A = [
  auditRow('au4', 'model_key', 'k3a6', 'create', '2026-06-20T10:00:00Z', null, { status: 'active', rotated_from: 'k2a6' }),
  auditRow('au3', 'model_consumer', PANEL_CONSUMER_A, 'update', '2026-06-15T09:00:00Z', { description: 'x' }, { description: 'y' }),
  auditRow('au2', 'model_key', 'k2a6', 'create', '2026-06-10T10:00:00Z', null, { status: 'active', rotated_from: 'k1a6' }),
  auditRow('au1', 'model_key', 'k1a6', 'update', '2026-06-10T10:00:01Z', { status: 'active' }, { status: 'revoked' }),
]

type DetailEntry = { tenant: TenantKey; get: Record<string, unknown> }

// Built on first use: fixtures.ts imports this module, so SENTINEL is not yet
// initialised while this module evaluates (ESM cycle) — a top-level table
// would read it in its temporal dead zone.
let detailTable: Record<string, DetailEntry> | null = null
function detailOf(id: string): DetailEntry | undefined {
  detailTable ??= buildDetail()
  return detailTable[id]
}

function buildDetail(): Record<string, DetailEntry> {
  return {
  [PANEL_CONSUMER_A]: {
    tenant: 'A',
    get: { success: true, consumer: consumerView(PANEL_CONSUMER_A, 'A', 'rotated-app', CHAIN_A), keys_truncated: false, audit: AUDIT_A },
  },
  [PANEL_CONSUMER_B]: {
    tenant: 'B',
    get: {
      success: true,
      consumer: consumerView(PANEL_CONSUMER_B, 'B', SENTINEL.B, [key('kb3', 'ctxm_…Sb3z', 'active', '2026-06-22T09:00:00Z', null, null)]),
      keys_truncated: false,
      audit: [],
    },
  },
  }
}

function visible(id: unknown, role: Role, tenant: TenantKey): DetailEntry | null {
  if (typeof id !== 'string') return null
  const d = detailOf(id)
  if (d === undefined) return null
  if (role === 'server-admin' || d.tenant === tenant) return d
  return null
}

function calls(id: string, role: Role): Record<string, unknown>[] {
  if (id !== PANEL_CONSUMER_A) return []
  const ok = {
    id: PANEL_CALL_ID,
    created_at: '2026-06-29T11:58:00Z',
    pipeline: 'gateway-chat',
    model: 'chat',
    backend: 'spark-chat',
    consumer_id: id,
    duration_ms: 812,
    error: null,
    prompt_tokens: 120,
    completion_tokens: 48,
    cost_usd: null,
    queue_wait_ms: 3,
  }
  const failed = {
    ...ok,
    id: '0190000000007000800000000000L0a7',
    created_at: '2026-06-29T11:50:00Z',
    duration_ms: null,
    prompt_tokens: null,
    completion_tokens: null,
    // below server-admin the server never sends the detail (design/05 §4.3)
    error: role === 'server-admin' ? { class: 'upstream', status: 502, detail: 'provider body' } : { class: 'upstream', status: 502 },
  }
  return [ok, failed]
}

function point(bucket: string, over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    bucket_start: bucket,
    complete: true,
    calls: 12,
    errors: 1,
    estimated_calls: 0,
    prompt_tokens: 3400,
    completion_tokens: 900,
    gpu_ms: 15_000,
    cost_usd: null,
    avg_ms: 820,
    avg_queue_ms: 14,
    avg_ttft_ms: 210,
    p50_ms: null,
    p95_ms: null,
    p50_queue_ms: null,
    p95_queue_ms: null,
    p50_ttft_ms: null,
    p95_ttft_ms: null,
    rejected: { rate: { count: 3, wait_ms_avg: 0 } },
    ...over,
  }
}

function usage(search: URLSearchParams): Record<string, unknown> {
  const consumer = search.get('consumer') ?? ''
  const bucket = search.get('bucket') === 'day' ? 'day' : 'hour'
  return {
    success: true,
    from: search.get('from') ?? '',
    to: search.get('to') ?? '',
    bucket,
    group: null,
    complete: false,
    hist: false,
    series:
      consumer === PANEL_CONSUMER_A
        ? [
            {
              key: consumer,
              label: 'rotated-app',
              op: 'chat',
              points: [
                point('2026-06-29T09:00:00Z'),
                // the rollup does not cover this bucket — its wire zeros must read "not recorded"
                point('2026-06-29T10:00:00Z', { complete: false, calls: 0, errors: 0, prompt_tokens: 0, completion_tokens: 0 }),
                // a server bug the panel must survive: an auth COUNT on a consumer series
                point('2026-06-29T11:00:00Z', { calls: null, rejected: { auth: { count: 7, wait_ms_avg: null }, rate: { count: 2, wait_ms_avg: 0 } } }),
              ],
            },
          ]
        : [],
  }
}

function limitValues(over: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    rpm: null,
    tpm: null,
    max_concurrency: null,
    daily_tokens: null,
    monthly_tokens: null,
    daily_requests: null,
    monthly_cost_usd: null,
    allow_external: null,
    allow_unmetered_external: null,
    ...over,
  }
}

const LIMIT_DEFAULTS: Record<string, Record<string, unknown>> = {
  chat: limitValues({ rpm: 30, tpm: 60_000, max_concurrency: 2, daily_tokens: 2_000_000, monthly_cost_usd: 1 }),
  embed: limitValues({ rpm: 120, tpm: 500_000, max_concurrency: 2, daily_tokens: 20_000_000, monthly_cost_usd: 1 }),
  rerank: limitValues({ rpm: 60, tpm: 200_000, max_concurrency: 2, daily_tokens: 5_000_000, monthly_cost_usd: 1 }),
}

const LIMIT_DIMS = ['rpm', 'tpm', 'max_concurrency', 'daily_tokens', 'monthly_tokens', 'daily_requests', 'monthly_cost_usd']

/** The stateful panel world of one session (limits rows survive a set). */
export class PanelWorld {
  /** consumer → op → tier → values */
  #rows = new Map<string, Record<string, unknown>>()

  #row(consumer: string, op: string, tier: 'server' | 'tenant'): Record<string, unknown> {
    const k = `${consumer}|${op}|${tier}`
    let r = this.#rows.get(k)
    if (r === undefined) {
      // the detail consumer carries a server row for chat max_concurrency (4)
      r = limitValues(consumer === PANEL_CONSUMER_A && op === 'chat' && tier === 'server' ? { max_concurrency: 4 } : {})
      this.#rows.set(k, r)
    }
    return r
  }

  #view(consumer: string, op: string, role: Role): Record<string, unknown> {
    const server = this.#row(consumer, op, 'server')
    const tenant = this.#row(consumer, op, 'tenant')
    const defaults = LIMIT_DEFAULTS[op] ?? limitValues()
    const effective: Record<string, unknown> = limitValues()
    for (const d of LIMIT_DIMS) {
      const s = (server[d] as number | null) ?? (defaults[d] as number | null)
      const t = tenant[d] as number | null
      effective[d] = s === null ? t : t === null ? s : Math.min(s, t)
    }
    return {
      op,
      server,
      tenant,
      defaults,
      effective,
      binding: 'consumer',
      tenant_envelope: null,
      instance_envelope: role === 'server-admin' ? limitValues({ daily_tokens: 20_000_000 }) : null,
    }
  }

  /** The /api/manage panel actions; null = not a panel action. */
  manage(action: string | undefined, data: Record<string, unknown> | undefined, role: Role, tenant: TenantKey): MockAnswer | null {
    if (action === 'model-consumer-get') {
      const d = visible(data?.id, role, tenant)
      return d === null ? NOT_FOUND : { status: 200, json: d.get }
    }
    if (action !== 'gateway-limits-get' && action !== 'gateway-limits-set' && action !== 'gateway-limits-clear') return null
    if (role === 'member') return { status: 403, json: { success: false, error: 'admin key required' } }
    const consumer = data?.consumer_id
    if (visible(consumer, role, tenant) === null) return NOT_FOUND
    const id = consumer as string
    if (action === 'gateway-limits-get') {
      return { status: 200, json: { success: true, consumer_id: id, ops: ['chat', 'embed', 'rerank'].map((o) => this.#view(id, o, role)) } }
    }
    const op = typeof data?.op === 'string' ? data.op : ''
    if (!(op in LIMIT_DEFAULTS)) return { status: 400, json: { success: false, error: 'op must be chat, embed or rerank' } }
    if (data !== undefined && 'tier' in data) return { status: 400, json: { success: false, error: 'unknown field: tier' } }
    const tier = role === 'server-admin' ? 'server' : 'tenant'
    const row = this.#row(id, op, tier)
    if (action === 'gateway-limits-clear') {
      const fields = Array.isArray(data?.fields) ? (data.fields as string[]) : LIMIT_DIMS
      for (const f of fields) row[f] = null
      return { status: 200, json: { success: true, limits: this.#view(id, op, role) } }
    }
    const values = (data?.values ?? {}) as Record<string, unknown>
    const view = this.#view(id, op, role)
    const errs: { field: string; message: string }[] = []
    for (const [f, v] of Object.entries(values)) {
      if (!LIMIT_DIMS.includes(f)) continue
      if (v !== null && (typeof v !== 'number' || !(v > 0))) errs.push({ field: f, message: 'must be > 0' })
      if (tier === 'tenant' && typeof v === 'number') {
        const ceiling = ((view.server as Record<string, unknown>)[f] as number | null) ?? ((view.defaults as Record<string, unknown>)[f] as number | null)
        if (ceiling !== null && v > ceiling) errs.push({ field: f, message: `at most ${ceiling} (server limit)` })
      }
    }
    if (errs.length > 0) return { status: 422, json: { success: false, error: 'limit exceeds the server limit', fields: errs } }
    for (const [f, v] of Object.entries(values)) if (LIMIT_DIMS.includes(f)) row[f] = v
    return { status: 200, json: { success: true, limits: this.#view(id, op, role) } }
  }
}

/** GET /api/llmlog?consumer_id= and GET /api/usage; null = not a panel route. */
export function panelGet(path: string, search: URLSearchParams, role: Role, tenant: TenantKey): MockAnswer | null {
  if (path === '/api/llmlog' && search.has('consumer_id')) {
    const id = search.get('consumer_id') ?? ''
    // the server's tenant filter: a foreign consumer answers an EMPTY list (no oracle)
    return { status: 200, json: { success: true, entries: visible(id, role, tenant) === null ? [] : calls(id, role) } }
  }
  if (path === '/api/usage') {
    if (role === 'member') return { status: 403, json: { success: false, error: 'admin key required' } }
    const id = search.get('consumer') ?? ''
    if (id !== '' && visible(id, role, tenant) === null) return { status: 200, json: { ...usage(new URLSearchParams()), series: [] } }
    return { status: 200, json: usage(search) }
  }
  return null
}

/** The /api/status gateway sections of a role (exactly one non-null); {} = absent. */
export function gatewayStatusSections(role: Role, seed: PanelSeed | undefined): Record<string, unknown> {
  if (seed?.gatewayStatus === undefined) return {}
  if (seed.gatewayStatus === 'null') return { gateway: null, gateway_tenant: null }
  if (role === 'server-admin') {
    return {
      gateway: {
        enabled: true,
        consumers: 7,
        reservations_open: 2,
        rejections_5m: { rate: 4, quota: 1 },
        instance_envelope: { chat: { used: 1200, limit: 600_000 } },
        top_consumers_1h: [{ consumer_id: PANEL_CONSUMER_A, label: 'rotated-app', tenant_slug: 'acme', requests: 42, tokens: 9000 }],
        consumers_used_1h: 3,
        consumers_expiring: 2,
        auth: { invalid_key: 5 },
      },
      gateway_tenant: null,
      dispatch: {
        enabled: true,
        enforcing: true,
        demand: 0,
        reaps_total: 0,
        class_downgrades: 0,
        class_unauthorized: 0,
        principal_conflicts: 0,
        uncharged_calls: 0,
        ops_total: 0,
        max_op_ms: 0,
        last_guard_at: null,
        last_digest_at: null,
        last_overview_at: null,
        embed_tokens: [],
        targets: [dispatchTarget()],
      },
      usage_split_24h: [],
    }
  }
  return {
    gateway: null,
    gateway_tenant: {
      enabled: true,
      consumers: 2,
      consumers_used_1h: 1,
      consumers_expiring: 1,
      rejections_5m: {},
      // `waiting: 37` is a leak the tile must never render (V-03-b)
      targets: [{ origin: 'http://spark:30000', depth: 'hoch', waiting: 37 }],
    },
  }
}

function dispatchTarget(): Record<string, unknown> {
  const wait = (waiting: number, p95: number) => ({ waiting, oldest_wait_ms: 0, p95_wait_ms: p95, max_wait_ms: p95, samples: 9 })
  return {
    origin: 'http://spark:30000',
    slots: 8,
    preempt_background: true,
    herald_scope: '',
    held: 5,
    inflight: 5,
    settling: 0,
    held_internal: 3,
    held_external: 2,
    inflight_internal: 3,
    internal_reserved: 4,
    external_offered: true,
    external_capacity: 4,
    external: wait(4, 2500),
    external_rejects: { cap: 0, not_offered: 0 },
    external_bulk: wait(6, 4000),
    external_bulk_rejects: { cap: 0, not_offered: 0 },
    interactive: wait(1, 120),
    background: wait(0, 0),
    preempt: { preempts_total: 0, wasted_ms_total: 0, release_ms_last: 0, release_ms_max: 0, forced_releases_total: 0, aged_admits_total: 0, aged_preempts_total: 0 },
    buckets: [],
  }
}
