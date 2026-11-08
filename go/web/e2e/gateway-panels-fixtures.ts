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
//   - gateway-limits-set derives the tier from the caller and answers 400
//     {error:'limit widens: <field>', param} above the ceiling for a
//     tenant-admin; a `tier` field is an unknown field ⇒ 400.
//   - /api/status carries `gateway` for both tiers (the tenant one without
//     envelope/auth/events) and, for a tenant-admin, a dispatch_tenant target
//     with a numeric `waiting` leak the tile must drop.
// Kept apart from fixtures.ts so the RC-D SPA clusters C5/C6 do not edit the
// same mock body; fixtures.ts calls in through three hooks.
// S2 (rc-d-s2-common.md §1, Go wins): the mocks follow C1's
// gateway-limits-get|set|delete (subject_kind, ops map, ceiling, flat full-row
// set, 400 {error, param}, delete) and C2's llmusage.Result / status `gateway`
// (one field for both tiers; the tenant depth class rides dispatch_tenant).

import type { Role, TenantKey } from './fixtures'
import { SENTINEL } from './fixtures'

/** The detail consumer of tenant A — a rotated app consumer (three keys). */
// S3 drift fix: dashed UUIDs — GET /api/usage?consumer= and /api/llmlog?consumer_id=
// answer 400 for anything else (handler/usage.go usageUUID, llmlog.go uuidRe).
export const PANEL_CONSUMER_A = '01900000-0000-7000-8000-00000000c0a6'
/** Tenant B's own detail consumer (label = SENTINEL.B — the leak probe's positive control). */
export const PANEL_CONSUMER_B = '01900000-0000-7000-8000-00000000c0b3'
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

function calls(id: string): Record<string, unknown>[] {
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
    dispatch_class: 'external',
    dispatch_abort: null,
  }
  const failed = {
    ...ok,
    id: '0190000000007000800000000000L0a7',
    created_at: '2026-06-29T11:50:00Z',
    duration_ms: null,
    prompt_tokens: null,
    completion_tokens: null,
    // llmlogError {class, detail} (C2 keeps it): the capped raw text may carry a
    // provider body — the panels show the class only, never this detail
    error: { class: 'http_502', detail: 'unexpected status 502: PROMPT-FRAGMENT-DO-NOT-RENDER' },
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
    avg_ms: 820,
    p50_ms: null,
    p95_ms: null,
    queue_p50_ms: null,
    queue_p95_ms: null,
    ttft_p50_ms: null,
    ttft_p95_ms: null,
    cost_usd: null,
    // a rate refusal never waits ⇒ wait_n 0 ⇒ wait_ms_avg null (llmusage/usage.go scanRefusals)
    rejected: [{ reason: 'rate', detail: 'rate_requests', n: 3, wait_ms_avg: null }],
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
    group: search.get('group') ?? 'consumer',
    complete: false,
    hist: false,
    series:
      consumer === PANEL_CONSUMER_A
        ? [
            {
              key: consumer,
              points: [
                point('2026-06-29T09:00:00Z'),
                // the running bucket: partial wire values, complete:false — must read "not recorded"
                point('2026-06-29T10:00:00Z', { complete: false, calls: 0, errors: 0, prompt_tokens: 0, completion_tokens: 0 }),
                // a server bug the panel must survive: an auth COUNT on a consumer series
                point('2026-06-29T11:00:00Z', {
                  calls: null,
                  rejected: [
                    { reason: 'auth', detail: 'invalid_key', n: 7, wait_ms_avg: null },
                    { reason: 'rate', detail: 'rate_tokens', n: 2, wait_ms_avg: null },
                  ],
                }),
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

/** The settings floor per op (gateway.<op>_rpm_default …; monthly_tokens/daily_requests 0 = unbounded ⇒ null). */
const LIMIT_SETTINGS: Record<string, Record<string, unknown>> = {
  chat: limitValues({ rpm: 30, tpm: 60_000, max_concurrency: 2, daily_tokens: 2_000_000, monthly_cost_usd: 1, allow_external: false }),
  embed: limitValues({ rpm: 120, tpm: 500_000, max_concurrency: 2, daily_tokens: 20_000_000, monthly_cost_usd: 1, allow_external: false }),
  rerank: limitValues({ rpm: 60, tpm: 200_000, max_concurrency: 2, daily_tokens: 5_000_000, monthly_cost_usd: 1, allow_external: false }),
}

const LIMIT_DIMS = ['rpm', 'tpm', 'max_concurrency', 'daily_tokens', 'monthly_tokens', 'daily_requests', 'monthly_cost_usd']
const LIMIT_FIELDS = [...LIMIT_DIMS, 'allow_external', 'allow_unmetered_external']
/** dispatch.external_queue_per_consumer — the V48 cap of max_concurrency (C1 A9). */
const CONCURRENCY_CAP = 8

// FOLGE-Z (briefings/FOLGE-Z-INDEX.md §3) — the tenant / instance subjects of
// gateway-limits-get (handler gateway_limits_manage.go: tenant-admin reads its
// OWN tenant, instance is server-admin only ⇒ 403, an unknown or foreign tenant
// the 404 "tenant not found") and the start limits of model-consumer-create
// (§3 `limits` in, `effective_limits` out). Tenant B carries a tenant_server
// row (chat rpm 10) so the create dialog's prefill provably follows the CHOSEN
// tenant, not the instance.

/**
 * Stored tenant rows: tenant id → op → tier → values. Tenant B: a tenant_server
 * row (chat rpm 10 — a consumer_server value REPLACES it) and a tenant_tenant row
 * (embed rpm 60 — it NARROWS every value below it, gatewaylimits.go levels.effective).
 */
const TENANT_LIMIT_ROWS: Record<string, Record<string, Partial<Record<'server' | 'tenant', Record<string, unknown>>>>> = {
  [PANEL_TENANT_ID.B]: { chat: { server: limitValues({ rpm: 10 }) }, embed: { tenant: limitValues({ rpm: 60 }) } },
}

const TENANT_NOT_FOUND: MockAnswer = { status: 404, json: { success: false, error: 'tenant not found' } }
const LIMIT_OPS = ['chat', 'embed', 'rerank']

/**
 * gatewaylimits.go levels.effective for a consumer WITHOUT rows of its own:
 * server bound = consumer_server ?? tenant_server ?? settings (pick, first
 * wins), then the tenant_tenant row narrows (tighter). `cs` is a consumer
 * server-tier row (a server-admin's start limits). Permissions: settings
 * (allow_external) / the fixed default (allow_unmetered_external, source
 * "default"); rows of this world carry none.
 */
function resolveLevels(
  tenantId: string | null,
  op: string,
  cs?: Record<string, unknown>,
  ct?: Record<string, unknown>,
): { values: Record<string, unknown>; sources: Record<string, string> } {
  const base = LIMIT_SETTINGS[op] ?? limitValues()
  const rows = tenantId === null ? undefined : TENANT_LIMIT_ROWS[tenantId]?.[op]
  const values: Record<string, unknown> = {}
  const sources: Record<string, string> = {}
  for (const d of LIMIT_DIMS) {
    let v: number | null = (base[d] as number | null) ?? null
    let src = 'settings'
    const pickOf = (row: Record<string, unknown> | undefined, s: string): boolean => {
      const x = row?.[d]
      if (typeof x === 'number') {
        v = x
        src = s
        return true
      }
      return false
    }
    if (!pickOf(cs, 'consumer_server')) pickOf(rows?.server, 'tenant_server')
    for (const [row, s] of [[rows?.tenant, 'tenant_tenant'], [ct, 'consumer_tenant']] as const) {
      const x = row?.[d]
      if (typeof x === 'number' && (v === null || x < v)) {
        v = x
        src = s
      }
    }
    values[d] = v
    sources[d] = src
  }
  values.allow_external = (cs?.allow_external as boolean | undefined) ?? base.allow_external === true
  sources.allow_external = typeof cs?.allow_external === 'boolean' ? 'consumer_server' : 'settings'
  if (ct?.allow_external === false) {
    values.allow_external = false
    sources.allow_external = 'consumer_tenant'
  }
  values.allow_unmetered_external = (cs?.allow_unmetered_external as boolean | undefined) ?? false
  sources.allow_unmetered_external = typeof cs?.allow_unmetered_external === 'boolean' ? 'consumer_server' : 'default'
  return { values, sources }
}

/** The effective values + sources a new consumer of a tenant (null = the instance) inherits. */
function tenantEffective(tenantId: string | null, op: string): { values: Record<string, unknown>; sources: Record<string, string> } {
  return resolveLevels(tenantId, op)
}

/** gateway-limits-get for subject_kind tenant | instance (read only — the panel world writes consumer rows). */
function subjectLimitsGet(data: Record<string, unknown>, role: Role, tenant: TenantKey): MockAnswer {
  const kind = data.subject_kind
  if (kind === 'instance') {
    if (role !== 'server-admin') return { status: 403, json: { success: false, error: 'admin key required' } }
    if (data.tenant_id !== undefined || data.consumer_id !== undefined)
      return { status: 400, json: { success: false, error: 'invalid data: the instance row names no consumer_id or tenant_id', param: 'subject_kind' } }
  } else if (kind === 'tenant') {
    const id = data.tenant_id
    if (typeof id !== 'string' || id === '' || data.consumer_id !== undefined)
      return { status: 400, json: { success: false, error: 'invalid data: a tenant row names tenant_id and no consumer_id', param: 'tenant_id' } }
    const known = Object.values(PANEL_TENANT_ID).includes(id)
    if (!known || (role !== 'server-admin' && id !== PANEL_TENANT_ID[tenant])) return TENANT_NOT_FOUND
  } else {
    return { status: 400, json: { success: false, error: 'invalid data: subject_kind must be consumer, tenant, instance or tenant_envelope', param: 'subject_kind' } }
  }
  const tenantId = kind === 'tenant' ? (data.tenant_id as string) : null
  const ops: Record<string, unknown> = {}
  for (const op of LIMIT_OPS) {
    const e = tenantEffective(tenantId, op)
    const rows = tenantId === null ? undefined : TENANT_LIMIT_ROWS[tenantId]?.[op]
    const effective: Record<string, unknown> = { ...e.values }
    if (tenantId !== null) effective.tenant_envelope = { daily_tokens: 10_000_000, monthly_tokens: null, daily_requests: null, monthly_cost_usd: 5 }
    if (role === 'server-admin') effective.instance_envelope = { daily_tokens: 20_000_000, monthly_tokens: null, daily_requests: null, monthly_cost_usd: 10 }
    const view: Record<string, unknown> = {
      server: rows?.server ?? null,
      tenant: rows?.tenant ?? null,
      effective,
      sources: e.sources,
    }
    if (tenantId !== null) {
      const ceiling: Record<string, unknown> = {}
      for (const d of LIMIT_DIMS) ceiling[d] = e.values[d] ?? null
      ceiling.allow_external = null
      ceiling.allow_unmetered_external = null
      view.ceiling = ceiling
    }
    ops[op] = view
  }
  const limits: Record<string, unknown> = { subject_kind: kind, ops }
  if (tenantId !== null) limits.tenant_id = tenantId
  return { status: 200, json: { success: true, limits } }
}

/**
 * model-consumer-create `limits` (§3 `L`, the rules of gateway-limits-set at the
 * CALLER's tier) ⇒ the new consumer's `effective_limits` (§3 `E` per op), or the
 * 400 refusal. The consumer inherits its tenant's effective values; a given
 * dimension lands as consumer_server (server-admin, free) or consumer_tenant
 * (tenant-admin, at or under the inherited value — "limit widens" otherwise).
 */
export function mockStartLimits(tenantId: string, role: Role, limits: unknown): { error: MockAnswer } | { effective: Record<string, unknown> } {
  // C-GO writeCreateLimitError: the gateway-limits-set wording, param = the field's path in the create payload
  const bad = (error: string, param: string): { error: MockAnswer } => ({ error: { status: 400, json: { success: false, error, param } } })
  const given: Record<string, Record<string, unknown>> = {}
  if (limits !== undefined && limits !== null) {
    if (typeof limits !== 'object' || Array.isArray(limits)) return bad('invalid data: limits must be an object of ops', 'limits')
    for (const [op, l] of Object.entries(limits as Record<string, unknown>)) {
      if (!LIMIT_OPS.includes(op)) return bad('invalid data: op must be chat, embed or rerank', 'op')
      if (l === null || typeof l !== 'object' || Array.isArray(l)) return bad(`invalid data: limits.${op} must be an object`, 'limits')
      for (const k of Object.keys(l)) if (!LIMIT_FIELDS.includes(k)) return bad(`invalid data: unknown field "${k}"`, `limits.${op}.${k}`)
      given[op] = l as Record<string, unknown>
    }
  }
  const serverAdmin = role === 'server-admin'
  const effective: Record<string, unknown> = {}
  for (const op of LIMIT_OPS) {
    const inherited = tenantEffective(tenantId, op)
    const l = given[op] ?? {}
    const row: Record<string, unknown> = {}
    for (const d of LIMIT_DIMS) {
      const v = l[d]
      if (v === undefined) continue
      if (typeof v !== 'number' || !(v > 0) || !Number.isFinite(v)) return bad(`invalid data: ${d} must be > 0`, `limits.${op}.${d}`)
      if (d === 'max_concurrency' && v > CONCURRENCY_CAP) return bad('invalid data: max_concurrency over the cap', `limits.${op}.max_concurrency`)
      // a tenant-admin narrows under the ceiling = what the new consumer inherits
      const c = inherited.values[d] as number | null
      if (!serverAdmin && c !== null && v > c) return bad(`limit widens: ${d} — ${v} is above ${c}, the limit of the levels above`, `limits.${op}.${d}`)
      row[d] = v
    }
    for (const f of ['allow_external', 'allow_unmetered_external']) {
      const v = l[f]
      if (v === undefined) continue
      if (typeof v !== 'boolean') return bad(`invalid data: ${f} must be a boolean`, `limits.${op}.${f}`)
      if (!serverAdmin && (f === 'allow_unmetered_external' || v === true)) return bad(`limit widens: ${f} — a tenant-tier row can only withdraw the permission (false)`, `limits.${op}.${f}`)
      row[f] = v
    }
    const e = serverAdmin ? resolveLevels(tenantId, op, row) : resolveLevels(tenantId, op, undefined, row)
    effective[op] = { ...e.values, sources: e.sources }
  }
  return { effective }
}

/** The stateful panel world of one session (limits rows survive a write). */
export class PanelWorld {
  /** "<consumer>|<op>|<tier>" → stored row (absent = no row) */
  #rows = new Map<string, Record<string, unknown>>()

  constructor() {
    // the detail consumer carries a server row for chat (max_concurrency 4)
    this.#rows.set(`${PANEL_CONSUMER_A}|chat|server`, limitValues({ max_concurrency: 4 }))
  }

  #row(consumer: string, op: string, tier: 'server' | 'tenant'): Record<string, unknown> | null {
    return this.#rows.get(`${consumer}|${op}|${tier}`) ?? null
  }

  /** C1 Snapshot.Ceiling for a consumer: server row ?? settings (no tenant rows in this world). */
  #ceiling(consumer: string, op: string): Record<string, unknown> {
    const server = this.#row(consumer, op, 'server')
    const base = LIMIT_SETTINGS[op] ?? limitValues()
    const out: Record<string, unknown> = {}
    for (const f of LIMIT_DIMS) out[f] = (server?.[f] as unknown) ?? base[f] ?? null
    // gatewaylimits.Snapshot.Ceiling fills the integer dimensions and the cost only:
    // the two permissions are null in every ceiling (S3 drift fix)
    out.allow_external = null
    out.allow_unmetered_external = null
    return out
  }

  #opView(consumer: string, op: string, role: Role): Record<string, unknown> {
    const ceiling = this.#ceiling(consumer, op)
    const tenant = this.#row(consumer, op, 'tenant')
    const effective: Record<string, unknown> = {}
    for (const d of LIMIT_DIMS) {
      const c = ceiling[d] as number | null
      const t = (tenant?.[d] as number | null | undefined) ?? null
      effective[d] = c === null ? t : t === null ? c : Math.min(c, t)
    }
    const server = this.#row(consumer, op, 'server')
    const extBase = (server?.allow_external as boolean | null | undefined) ?? (LIMIT_SETTINGS[op]?.allow_external as boolean | null | undefined) ?? false
    effective.allow_external = extBase === true && tenant?.allow_external !== false
    effective.allow_unmetered_external = false
    effective.tenant_envelope = { daily_tokens: 10_000_000, monthly_tokens: null, daily_requests: null, monthly_cost_usd: 5 }
    if (role === 'server-admin') effective.instance_envelope = { daily_tokens: 20_000_000, monthly_tokens: null, daily_requests: null, monthly_cost_usd: 10 }
    // GatewayLimitsOp.sources (C1: always present next to effective) — the level per dimension
    const sources: Record<string, string> = {}
    for (const f of LIMIT_FIELDS) {
      const t = tenant?.[f]
      sources[f] = t !== null && t !== undefined ? 'consumer_tenant' : server?.[f] !== null && server?.[f] !== undefined ? 'consumer_server' : 'settings'
    }
    return { server, tenant, ceiling, effective, sources }
  }

  /** The /api/manage panel actions; null = not a panel action. */
  manage(action: string | undefined, data: Record<string, unknown> | undefined, role: Role, tenant: TenantKey): MockAnswer | null {
    if (action === 'model-consumer-get') {
      const d = visible(data?.id, role, tenant)
      return d === null ? NOT_FOUND : { status: 200, json: d.get }
    }
    if (action !== 'gateway-limits-get' && action !== 'gateway-limits-set' && action !== 'gateway-limits-delete') return null
    if (role === 'member') return { status: 403, json: { success: false, error: 'admin key required' } }
    // FOLGE-Z: the tenant / instance subjects of the create dialog's prefill (read only)
    if (action === 'gateway-limits-get' && (data?.subject_kind === 'tenant' || data?.subject_kind === 'instance')) {
      return subjectLimitsGet(data, role, tenant)
    }
    if (data?.subject_kind !== 'consumer') {
      // instance / tenant_envelope are server-admin only; this world serves consumer subjects
      return role === 'server-admin'
        ? { status: 400, json: { success: false, error: 'invalid data: subject_kind', param: 'subject_kind' } }
        : { status: 403, json: { success: false, error: 'admin key required' } }
    }
    const consumer = data.consumer_id
    if (visible(consumer, role, tenant) === null) return NOT_FOUND
    const id = consumer as string
    if (action === 'gateway-limits-get') {
      const ops: Record<string, unknown> = {}
      for (const o of ['chat', 'embed', 'rerank']) ops[o] = this.#opView(id, o, role)
      return { status: 200, json: { success: true, limits: { subject_kind: 'consumer', consumer_id: id, ops } } }
    }
    const op = typeof data.op === 'string' ? data.op : ''
    if (!(op in LIMIT_SETTINGS)) return { status: 400, json: { success: false, error: 'invalid data: op', param: 'op' } }
    const tier = role === 'server-admin' ? 'server' : 'tenant'
    const key = `${id}|${op}|${tier}`
    if (action === 'gateway-limits-delete') {
      const existed = this.#rows.delete(key)
      return { status: 200, json: { success: true, deleted: existed } }
    }
    // set: DisallowUnknownFields + full replacement of the caller-tier row
    for (const k of Object.keys(data)) {
      if (k !== 'subject_kind' && k !== 'consumer_id' && k !== 'op' && !LIMIT_FIELDS.includes(k))
        return { status: 400, json: { success: false, error: `invalid data: unknown field "${k}"`, param: k } }
    }
    const row = limitValues()
    for (const f of LIMIT_FIELDS) row[f] = data[f] ?? null
    for (const d of LIMIT_DIMS) {
      const v = row[d]
      if (v !== null && (typeof v !== 'number' || !(v > 0))) return { status: 400, json: { success: false, error: `invalid data: ${d} must be > 0`, param: d } }
    }
    const mc = row.max_concurrency
    if (typeof mc === 'number' && mc > CONCURRENCY_CAP) return { status: 400, json: { success: false, error: 'invalid data: max_concurrency over the cap', param: 'max_concurrency' } }
    if (tier === 'tenant') {
      const ceiling = this.#ceiling(id, op)
      for (const d of LIMIT_DIMS) {
        const v = row[d]
        const c = ceiling[d] as number | null
        if (typeof v === 'number' && c !== null && v > c)
          return { status: 400, json: { success: false, error: `limit widens: ${d} — ${v} is above ${c}, the limit of the levels above`, param: d } }
      }
      if (row.allow_external === true)
        return { status: 400, json: { success: false, error: 'limit widens: allow_external — a tenant-tier row can only withdraw the permission (false)', param: 'allow_external' } }
      if (row.allow_unmetered_external !== null)
        return { status: 400, json: { success: false, error: 'limit widens: allow_unmetered_external — only a server-admin sets the unmetered permission', param: 'allow_unmetered_external' } }
    }
    this.#rows.set(key, row)
    return { status: 200, json: { success: true, limits: { subject_kind: 'consumer', consumer_id: id, tier, op, ...row } } }
  }
}

/** handler/usage.go usageUUID = llmlog.go uuidRe: the dashed form, any case. */
const UUID_RE = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/

/** GET /api/llmlog?consumer_id= and GET /api/usage; null = not a panel route. */
export function panelGet(path: string, search: URLSearchParams, role: Role, tenant: TenantKey): MockAnswer | null {
  if (path === '/api/llmlog' && search.has('consumer_id')) {
    const id = search.get('consumer_id') ?? ''
    if (id !== '' && !UUID_RE.test(id)) return { status: 400, json: { success: false, error: 'invalid consumer_id' } }
    // the server's tenant filter: a foreign consumer answers an EMPTY list (no oracle)
    return { status: 200, json: { success: true, entries: visible(id, role, tenant) === null ? [] : calls(id) } }
  }
  if (path === '/api/usage') {
    if (role === 'member') return { status: 403, json: { success: false, error: 'admin key required' } }
    const id = search.get('consumer') ?? ''
    if (id !== '' && !UUID_RE.test(id)) return { status: 400, json: { success: false, error: 'invalid consumer' } }
    if (id !== '' && visible(id, role, tenant) === null) return { status: 200, json: { ...usage(new URLSearchParams()), series: [] } }
    return { status: 200, json: usage(search) }
  }
  return null
}

/** The /api/status gateway sections of a role (C2: one `gateway` field for both tiers); {} = absent. */
export function gatewayStatusSections(role: Role, seed: PanelSeed | undefined): Record<string, unknown> {
  if (seed?.gatewayStatus === undefined) return {}
  if (seed.gatewayStatus === 'null') return { gateway: null }
  if (role === 'server-admin') {
    return {
      gateway: {
        enabled: true,
        consumers: 7,
        reservations_open: 2,
        rejections_5m: { rate: 4, quota: 1 },
        top_consumers_1h: [{ consumer_id: PANEL_CONSUMER_A, requests: 42, tokens: 9000 }],
        consumers_used_1h: 3,
        consumers_expiring: 2,
        instance_envelope: { chat: { used: 1200, limit: 600_000 } },
        // ModelAuthStats / usageevents.Stats have NO json tags: the wire keys are the
        // Go field names (status_gateway.go stampGateway; S3 drift fix — subset)
        auth: { Accepted: 40, NoCredential: 1, FormFailures: 0, Locked: 0, Invalid: 5, SemaphoreWaitTotal: 0, Inflight: 0 },
        events: { Noted: 3, Deduped: 1, Written: 2, Dropped: 0, Failed: 0, PeerBound: 0 },
        generated_at: '2026-06-29T11:59:30Z',
      },
      usage_split_24h: [{ target: 'http://spark:30000', source: 'external', class: 'external', calls: 40, gpu_ms: 90_000 }],
      federation: {
        loop_rejects: 0,
        depth_rejects: 0,
        header_invalid: 0,
        header_ignored: 0,
        peer_without_path: 0,
        peer_mismatch: 0,
        embed_space_rejects: 0,
        busy: 0,
        stamp_without_identity: 0,
        peer_bound: 0,
      },
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
      dispatch_tenant: null,
    }
  }
  return {
    // the tenant answer: own consumers only, no instance_envelope / auth / events;
    // reservations_open / rejections_5m null = not fed yet (C2 4b510735, null ≠ 0)
    gateway: {
      enabled: true,
      consumers: 2,
      reservations_open: null,
      rejections_5m: null,
      // stampGateway always sends an array (Ring.Top: make(…, 0, n)) — never null (S3 drift fix)
      top_consumers_1h: [],
      consumers_used_1h: 1,
      consumers_expiring: 1,
      generated_at: '2026-06-29T11:59:30Z',
    },
    dispatch: null,
    dispatch_tenant: {
      // `waiting: 37` is a leak the tile must never render (V-03-b)
      targets: [{ origin: 'http://spark:30000', busy: true, depth: 'hoch', own_waiting: 0, own_inflight: 0, own_oldest_wait_ms: 0, waiting: 37 }],
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
