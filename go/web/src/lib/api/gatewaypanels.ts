// Model API panels client (RC-D cluster C6: W05-09 consumer detail, W05-10
// usage panel, W05-12 limits panel; design/05 §4.2, design/06 §4.3/§4.8).
// One named function per server call, mirroring modelapi.ts (kept in its own
// module so the RC-D clusters C5 and C6 do not edit the same client file):
//
//   getConsumer          POST /api/manage  model-consumer-get {id}          (root, W05-02)
//   listConsumerCalls    GET  /api/llmlog?consumer_id=<id>[&limit=]         (C2 W06-17)
//   getUsage             GET  /api/usage?from&to&bucket&consumer=<id>       (C2 W06-16)
//   getGatewayLimits     POST /api/manage  gateway-limits-get {consumer_id} (C1 W06-15)
//   setGatewayLimits     POST /api/manage  gateway-limits-set {consumer_id, op, values}
//   clearGatewayLimits   POST /api/manage  gateway-limits-clear {consumer_id, op, fields?}
//
// No limits payload ever carries a tier: the server derives it from the
// caller (design/06 §4.3 "Tier aus dem Aufrufer abgeleitet, nie aus dem Body").
//
// RC-D S1: contract stubs — every call answers fail-closed with a 503
// ApiError (`service_unavailable`), usageRange answers null. S2 wires them.

import { ApiError } from '../api'
import type {
  ConsumerCallsResponse,
  ConsumerGetResponse,
  GatewayLimitsClearSpec,
  GatewayLimitsGetResponse,
  GatewayLimitsSetSpec,
  GatewayLimitsWriteResponse,
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

function notWired(what: string): Promise<never> {
  return Promise.reject(new ApiError(503, 'service_unavailable', `${what}: not implemented (RC-D S1 stub)`))
}

/**
 * The from/to/bucket of a window ending at `nowMs`. Building a REQUEST value
 * is the only use of a client clock here (precedent createConsumer's
 * expires_in_days); the server validates the range against its own clock and
 * answers 400 beyond 92 d. Never used to derive a status (B-11).
 * null = not computable (S1 stub).
 */
export function usageRange(window: UsageWindow, nowMs: number): UsageRange | null {
  void window
  void nowMs
  return null
}

/** model-consumer-get — the consumer, its key chain and audit trail (No-Oracle refusal as ApiError). */
export function getConsumer(id: string): Promise<ConsumerGetResponse> {
  void id
  return notWired('model-consumer-get')
}

/** GET /api/llmlog?consumer_id= — the consumer's newest calls, metadata only (no bodies). */
export function listConsumerCalls(consumerId: string, limit?: number): Promise<ConsumerCallsResponse> {
  void consumerId
  void limit
  return notWired('GET /api/llmlog?consumer_id')
}

/** GET /api/usage — one consumer's series over a panel window. `now` is injectable for vitest. */
export function getUsage(params: UsageParams, now: () => number = Date.now): Promise<UsageResponse> {
  void params
  void now
  return notWired('GET /api/usage')
}

/** gateway-limits-get — the consumer's limits per op (server, tenant, defaults, effective). */
export function getGatewayLimits(consumerId: string): Promise<GatewayLimitsGetResponse> {
  void consumerId
  return notWired('gateway-limits-get')
}

/** gateway-limits-set — write the caller-tier row of one op (422 + fields[] above the ceiling). */
export function setGatewayLimits(spec: GatewayLimitsSetSpec): Promise<GatewayLimitsWriteResponse> {
  void spec
  return notWired('gateway-limits-set')
}

/** gateway-limits-clear — drop the caller-tier row (or some of its fields) of one op. */
export function clearGatewayLimits(spec: GatewayLimitsClearSpec): Promise<GatewayLimitsWriteResponse> {
  void spec
  return notWired('gateway-limits-clear')
}
