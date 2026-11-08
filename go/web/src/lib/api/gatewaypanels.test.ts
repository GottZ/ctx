// gatewaypanels.ts binding gates (RC-D C6 S1 — W05-09, W05-10, W05-12):
// every panel call hits exactly the action / path + payload of the contract
// in types.ts block "// RC-D C6" (Go side: model-consumer-get on root, C2
// /api/llmlog?consumer_id + /api/usage, C1 gateway-limits-*), no limits
// payload carries a tier, and the usage window becomes a ≤ 92-d range with
// the window's bucket. Pure node, fetch stubbed — mirrors modelapi.test.ts.
// S2 (rc-d-s2-common.md §1, Go wins): the limits wire follows C1 — subject_kind
// in every payload, a FLAT full-replacement set body, gateway-limits-delete
// instead of -clear, field errors as 400 {error, param} instead of 422
// fields[]; the usage query carries group=consumer (C2 llmusage.GroupBy).

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, configureApi } from '../api'
import {
  USAGE_WINDOW_BUCKET,
  USAGE_WINDOW_MS,
  deleteGatewayLimits,
  getConsumer,
  getGatewayLimits,
  getUsage,
  listConsumerCalls,
  setGatewayLimits,
  usageRange,
} from './gatewaypanels'

const CID = '0190000000007000800000000000c001'
const NOW = Date.parse('2026-06-29T12:00:05Z')

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

function stubFetch(...responses: Response[]): ReturnType<typeof vi.fn> {
  const mock = vi.fn()
  for (const res of responses) mock.mockResolvedValueOnce(res)
  vi.stubGlobal('fetch', mock)
  return mock
}

function sentUrl(mock: ReturnType<typeof vi.fn>): URL {
  expect(mock, 'gate: the call reached the wire (exactly one request)').toHaveBeenCalledTimes(1)
  return new URL(String(mock.mock.calls[0]?.[0]), 'http://ctx.local')
}

function sentInit(mock: ReturnType<typeof vi.fn>): RequestInit {
  return (mock.mock.calls[0]?.[1] ?? {}) as RequestInit
}

function sentBody(mock: ReturnType<typeof vi.fn>): Record<string, unknown> {
  expect(mock, 'gate: the call reached the wire (exactly one request)').toHaveBeenCalledTimes(1)
  return JSON.parse(String(sentInit(mock).body)) as Record<string, unknown>
}

beforeEach(() => {
  vi.unstubAllGlobals()
  configureApi({ getCsrfToken: () => null, onUnauthorized: () => {} })
})
afterEach(() => vi.unstubAllGlobals())

describe('W05-09 model-consumer-get + last calls (wire)', () => {
  it('getConsumer POSTs model-consumer-get {id} and returns the answer', async () => {
    const answer = { success: true, consumer: { id: CID }, keys_truncated: false, audit: [] }
    const mock = stubFetch(jsonResponse(200, answer))
    const res = (await getConsumer(CID).catch((e: unknown) => e)) as { consumer: { id: string } }
    expect(mock, 'gate W05-09: exactly one request').toHaveBeenCalledTimes(1)
    expect(sentUrl(mock).pathname).toBe('/api/manage')
    expect(sentInit(mock).method).toBe('POST')
    expect(sentBody(mock), 'gate W05-09: action + payload of handleModelConsumerGet').toEqual({
      action: 'model-consumer-get',
      data: { id: CID },
    })
    expect(res.consumer.id).toBe(CID)
  })

  it('the No-Oracle refusal (404) surfaces as ApiError "model consumer not found"', async () => {
    stubFetch(jsonResponse(404, { success: false, error: 'model consumer not found' }))
    const err = await getConsumer(CID).catch((e: unknown) => e)
    expect(err, 'gate W05-09: refusal is an ApiError, never a resolved shape').toBeInstanceOf(ApiError)
    expect((err as ApiError).message, 'gate W05-09: the server’s No-Oracle text, unchanged').toBe('model consumer not found')
  })

  it('listConsumerCalls GETs /api/llmlog?consumer_id=<id> (no other filter, no body)', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, entries: [] }))
    await listConsumerCalls(CID, 25).catch((e: unknown) => e)
    const url = sentUrl(mock)
    expect(url.pathname, 'gate W05-09: the llmlog consumer branch (C2 W06-17)').toBe('/api/llmlog')
    expect(url.searchParams.get('consumer_id')).toBe(CID)
    expect(url.searchParams.get('limit')).toBe('25')
    expect(sentInit(mock).method ?? 'GET').toBe('GET')
  })
})

describe('W05-10 /api/usage (wire)', () => {
  it('each window maps to its bucket (24h/7d hourly, 30d/90d daily) and spans ≤ 92 d', () => {
    expect(USAGE_WINDOW_BUCKET).toEqual({ '24h': 'hour', '7d': 'hour', '30d': 'day', '90d': 'day' })
    for (const ms of Object.values(USAGE_WINDOW_MS)) expect(ms).toBeLessThanOrEqual(92 * 86_400_000)
  })

  it('usageRange builds from = now − window, to = now, with the bucket', () => {
    const r = usageRange('7d', NOW)
    expect(r, 'gate W05-10: a computable range (stub answers null)').not.toBeNull()
    expect(r).toEqual({
      from: new Date(NOW - 7 * 86_400_000).toISOString(),
      to: new Date(NOW).toISOString(),
      bucket: 'hour',
    })
  })

  it('getUsage GETs /api/usage?from&to&bucket&group=consumer&consumer for the consumer panel', async () => {
    const mock = stubFetch(
      jsonResponse(200, {
        success: true,
        from: '',
        to: '',
        bucket: 'day',
        group: 'consumer',
        complete: true,
        hist: true,
        series: [],
      }),
    )
    await getUsage({ consumer: CID, window: '30d' }, () => NOW).catch((e: unknown) => e)
    const url = sentUrl(mock)
    expect(url.pathname, 'gate W05-10: GET /api/usage (C2 W06-16)').toBe('/api/usage')
    expect(url.searchParams.get('consumer')).toBe(CID)
    expect(url.searchParams.get('group'), 'one series keyed by the consumer').toBe('consumer')
    expect(url.searchParams.get('bucket')).toBe('day')
    expect(url.searchParams.get('from')).toBe(new Date(NOW - 30 * 86_400_000).toISOString())
    expect(url.searchParams.get('to')).toBe(new Date(NOW).toISOString())
  })
})

describe('W05-12 gateway-limits-* (wire)', () => {
  it('getGatewayLimits POSTs gateway-limits-get {subject_kind: consumer, consumer_id}', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, limits: { subject_kind: 'consumer', consumer_id: CID, ops: {} } }))
    await getGatewayLimits(CID).catch((e: unknown) => e)
    expect(sentBody(mock), 'gate W05-12: action + payload (C1 W06-15)').toEqual({
      action: 'gateway-limits-get',
      data: { subject_kind: 'consumer', consumer_id: CID },
    })
  })

  it('setGatewayLimits sends a flat full row (every dimension, unset = null) — and NO tier (derived from the caller)', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, limits: { op: 'chat' } }))
    await setGatewayLimits(CID, 'chat', { rpm: 20, max_concurrency: 1 }).catch((e: unknown) => e)
    const body = sentBody(mock)
    expect(body.action, 'gate W05-12: gateway-limits-set').toBe('gateway-limits-set')
    expect(body.data).toEqual({
      subject_kind: 'consumer',
      consumer_id: CID,
      op: 'chat',
      rpm: 20,
      tpm: null,
      max_concurrency: 1,
      daily_tokens: null,
      monthly_tokens: null,
      daily_requests: null,
      monthly_cost_usd: null,
      allow_external: null,
      allow_unmetered_external: null,
    })
    expect(JSON.stringify(body), 'gate W05-12: the tier is never on the wire').not.toMatch(/"tier"/)
  })

  it('setGatewayLimits drops an extra tier property smuggled into the values', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, limits: { op: 'chat' } }))
    const values = { rpm: 20, tier: 'server' } as unknown as Parameters<typeof setGatewayLimits>[2]
    await setGatewayLimits(CID, 'chat', values).catch((e: unknown) => e)
    expect(JSON.stringify(sentBody(mock)), 'gate W05-12: explicit field list, never a spread').not.toMatch(/tier/)
  })

  it('a 400 keeps its param on the ApiError details (the panel shows it at the field)', async () => {
    stubFetch(jsonResponse(400, { success: false, error: 'limit widens: max_concurrency', param: 'max_concurrency' }))
    const err = (await setGatewayLimits(CID, 'chat', { max_concurrency: 9 }).catch((e: unknown) => e)) as ApiError
    expect(err, 'gate W05-12: the refusal is an ApiError').toBeInstanceOf(ApiError)
    expect(err.status, 'gate W05-12: the 400 of the server, not a client error').toBe(400)
    expect(err.details?.param).toBe('max_concurrency')
    expect(err.message).toBe('limit widens: max_concurrency')
  })

  it('deleteGatewayLimits POSTs gateway-limits-delete {subject_kind, consumer_id, op}', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, deleted: true }))
    await deleteGatewayLimits(CID, 'embed').catch((e: unknown) => e)
    expect(sentBody(mock), 'gate W05-12: delete action').toEqual({
      action: 'gateway-limits-delete',
      data: { subject_kind: 'consumer', consumer_id: CID, op: 'embed' },
    })
  })
})
