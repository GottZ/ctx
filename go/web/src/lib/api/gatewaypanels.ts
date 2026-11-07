// Model API panels client (RC-D cluster C6: W05-09 consumer detail, W05-10
// usage panel, W05-12 limits panel; design/05 §4.2, design/06 §4.3/§4.8).
// One named function per server call, mirroring modelapi.ts (kept in its own
// module so the RC-D clusters C5 and C6 do not edit the same client file):
//
//   getConsumer          POST /api/manage  model-consumer-get {id}                        (root, W05-02)
//   listConsumerCalls    GET  /api/llmlog?consumer_id=<id>[&limit=]                       (C2 W06-17)
//   getUsage             GET  /api/usage?from&to&bucket&group=consumer&consumer=<id>      (C2 W06-16)
//   getGatewayLimits     POST /api/manage  gateway-limits-get {subject_kind:'consumer', consumer_id}  (C1 W06-15)
//   setGatewayLimits     POST /api/manage  gateway-limits-set {subject_kind, consumer_id, op, <every dimension>}
//   deleteGatewayLimits  POST /api/manage  gateway-limits-delete {subject_kind, consumer_id, op}
//
// No limits payload ever carries a tier: the server derives it from the
// caller (C1: a `tier` field is an unknown field ⇒ 400). Bodies are built from
// explicit field lists, never a spread of the caller's object.

import { apiFetch } from '../api'
import type {
  ConsumerCallsResponse,
  ConsumerGetResponse,
  GatewayLimitsDeleteResponse,
  GatewayLimitsGetResponse,
  GatewayLimitsSetResponse,
  GatewayOp,
  LimitValues,
  UsageBucket,
  UsageResponse,
  UsageWindow,
} from './types'

/** The bucket of each panel window (design/05 §4.6): 24h + 7d hourly, 30d + 90d daily. */
export const USAGE_WINDOW_BUCKET: Readonly<Record<UsageWindow, UsageBucket>> = {
  '24h': 'hour',
  '7d': 'hour',
  '30d': 'day',
  '90d': 'day',
}

/** The span of each window in milliseconds (all ≤ 92 d, design/06 §4.8). */
export const USAGE_WINDOW_MS: Readonly<Record<UsageWindow, number>> = {
  '24h': 24 * 3_600_000,
  '7d': 7 * 86_400_000,
  '30d': 30 * 86_400_000,
  '90d': 90 * 86_400_000,
}

/** The query range of a window: RFC 3339 from/to and the bucket. */
export interface UsageRange {
  from: string
  to: string
  bucket: UsageBucket
}

/** GET /api/usage parameters of the consumer panel. */
export interface UsageParams {
  consumer: string
  window: UsageWindow
}

/** The limit values the wire carries, in the server's column order (a write sends every one). */
const LIMIT_FIELDS: readonly (keyof LimitValues)[] = [
  'rpm',
  'tpm',
  'max_concurrency',
  'daily_tokens',
  'monthly_tokens',
  'daily_requests',
  'monthly_cost_usd',
  'allow_external',
  'allow_unmetered_external',
]

/**
 * The from/to/bucket of a window ending at `nowMs`. Building a REQUEST value
 * is the only use of a client clock here (precedent createConsumer's
 * expires_in_days); the server cuts the range to whole UTC buckets with its
 * own clock and answers 400 beyond 92 d. Never used to derive a status (B-11).
 * null = an unknown window (fail-closed: no request).
 */
export function usageRange(window: UsageWindow, nowMs: number): UsageRange | null {
  const span = USAGE_WINDOW_MS[window]
  const bucket = USAGE_WINDOW_BUCKET[window]
  if (span === undefined || bucket === undefined || !Number.isFinite(nowMs)) return null
  return { from: new Date(nowMs - span).toISOString(), to: new Date(nowMs).toISOString(), bucket }
}

/** model-consumer-get — the consumer, its key chain and audit trail (No-Oracle refusal as ApiError). */
export function getConsumer(id: string): Promise<ConsumerGetResponse> {
  return apiFetch<ConsumerGetResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'model-consumer-get', data: { id } }),
  })
}

/** GET /api/llmlog?consumer_id= — the consumer's newest calls, metadata only (no bodies). */
export function listConsumerCalls(consumerId: string, limit?: number): Promise<ConsumerCallsResponse> {
  const q = new URLSearchParams({ consumer_id: consumerId })
  if (limit !== undefined) q.set('limit', String(limit))
  return apiFetch<ConsumerCallsResponse>(`/api/llmlog?${q.toString()}`)
}

/** GET /api/usage — one consumer's series over a panel window. `now` is injectable for vitest. */
export function getUsage(params: UsageParams, now: () => number = Date.now): Promise<UsageResponse> {
  const range = usageRange(params.window, now())
  if (range === null) return Promise.reject(new Error(`unknown usage window ${String(params.window)}`))
  const q = new URLSearchParams({
    from: range.from,
    to: range.to,
    bucket: range.bucket,
    group: 'consumer',
    consumer: params.consumer,
  })
  return apiFetch<UsageResponse>(`/api/usage?${q.toString()}`)
}

/** gateway-limits-get — the consumer's limits per op (server, tenant, ceiling, effective, sources). */
export function getGatewayLimits(consumerId: string): Promise<GatewayLimitsGetResponse> {
  return apiFetch<GatewayLimitsGetResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'gateway-limits-get', data: { subject_kind: 'consumer', consumer_id: consumerId } }),
  })
}

/**
 * gateway-limits-set — FULL replacement of the caller-tier row of one op: every
 * dimension goes on the wire, an absent one as null (C1: "die SPA sendet die
 * ganze Zeile"). The tier is the server's; a `tier` property on `values` is
 * not read.
 */
export function setGatewayLimits(
  consumerId: string,
  op: GatewayOp,
  values: Partial<LimitValues>,
): Promise<GatewayLimitsSetResponse> {
  const data: Record<string, unknown> = { subject_kind: 'consumer', consumer_id: consumerId, op }
  for (const f of LIMIT_FIELDS) data[f] = values[f] ?? null
  return apiFetch<GatewayLimitsSetResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'gateway-limits-set', data }),
  })
}

/** gateway-limits-delete — drop the caller-tier row of one op (the subject then inherits). */
export function deleteGatewayLimits(consumerId: string, op: GatewayOp): Promise<GatewayLimitsDeleteResponse> {
  return apiFetch<GatewayLimitsDeleteResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'gateway-limits-delete', data: { subject_kind: 'consumer', consumer_id: consumerId, op } }),
  })
}
