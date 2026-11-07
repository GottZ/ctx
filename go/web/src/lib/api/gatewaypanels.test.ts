// gatewaypanels.ts binding gates (RC-D C6 S1 — W05-09, W05-10, W05-12):
// every panel call hits exactly the action / path + payload of the contract
// in types.ts block "// RC-D C6" (Go side: model-consumer-get on root, C2
// /api/llmlog?consumer_id + /api/usage, C1 gateway-limits-*), no limits
// payload carries a tier, and the usage window becomes a ≤ 92-d range with
// the window's bucket. Pure node, fetch stubbed — mirrors modelapi.test.ts.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, configureApi } from '../api'
import {
  USAGE_WINDOW_BUCKET,
  USAGE_WINDOW_MS,
  clearGatewayLimits,
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

  it('getUsage GETs /api/usage?from&to&bucket&consumer for the consumer panel', async () => {
    const mock = stubFetch(
      jsonResponse(200, {
        success: true,
        from: '',
        to: '',
        bucket: 'day',
        group: null,
        complete: true,
        hist: true,
        series: [],
      }),
    )
    await getUsage({ consumer: CID, window: '30d' }, () => NOW).catch((e: unknown) => e)
    const url = sentUrl(mock)
    expect(url.pathname, 'gate W05-10: GET /api/usage (C2 W06-16)').toBe('/api/usage')
    expect(url.searchParams.get('consumer')).toBe(CID)
    expect(url.searchParams.get('bucket')).toBe('day')
    expect(url.searchParams.get('from')).toBe(new Date(NOW - 30 * 86_400_000).toISOString())
    expect(url.searchParams.get('to')).toBe(new Date(NOW).toISOString())
  })
})

describe('W05-12 gateway-limits-* (wire)', () => {
  it('getGatewayLimits POSTs gateway-limits-get {consumer_id}', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, consumer_id: CID, ops: [] }))
    await getGatewayLimits(CID).catch((e: unknown) => e)
    expect(sentBody(mock), 'gate W05-12: action + payload (C1 W06-15)').toEqual({
      action: 'gateway-limits-get',
      data: { consumer_id: CID },
    })
  })

  it('setGatewayLimits sends consumer_id, op and values — and NO tier (derived from the caller)', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, limits: { op: 'chat' } }))
    await setGatewayLimits({ consumer_id: CID, op: 'chat', values: { rpm: 20, max_concurrency: 1 } }).catch((e: unknown) => e)
    const body = sentBody(mock)
    expect(body.action, 'gate W05-12: gateway-limits-set').toBe('gateway-limits-set')
    expect(body.data).toEqual({ consumer_id: CID, op: 'chat', values: { rpm: 20, max_concurrency: 1 } })
    expect(JSON.stringify(body), 'gate W05-12: the tier is never on the wire').not.toMatch(/"tier"/)
  })

  it('setGatewayLimits drops an extra tier property smuggled into the spec', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, limits: { op: 'chat' } }))
    const spec = { consumer_id: CID, op: 'chat' as const, values: { rpm: 20 }, tier: 'server' }
    await setGatewayLimits(spec).catch((e: unknown) => e)
    expect(JSON.stringify(sentBody(mock)), 'gate W05-12: explicit field list, never a spread').not.toMatch(/tier/)
  })

  it('a 422 keeps its fields[] on the ApiError details (the panel shows it at the field)', async () => {
    stubFetch(
      jsonResponse(422, {
        success: false,
        error: 'limit exceeds the server limit',
        fields: [{ field: 'max_concurrency', message: 'at most 2 (server limit)' }],
      }),
    )
    const err = (await setGatewayLimits({ consumer_id: CID, op: 'chat', values: { max_concurrency: 9 } }).catch(
      (e: unknown) => e,
    )) as ApiError
    expect(err, 'gate W05-12: 422 is an ApiError').toBeInstanceOf(ApiError)
    expect(err.status, 'gate W05-12: the 422 of the server, not a client error').toBe(422)
    expect(err.details?.fields).toEqual([{ field: 'max_concurrency', message: 'at most 2 (server limit)' }])
  })

  it('clearGatewayLimits POSTs gateway-limits-clear {consumer_id, op, fields?}', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, limits: { op: 'embed' } }))
    await clearGatewayLimits({ consumer_id: CID, op: 'embed', fields: ['tpm'] }).catch((e: unknown) => e)
    expect(sentBody(mock), 'gate W05-12: clear action').toEqual({
      action: 'gateway-limits-clear',
      data: { consumer_id: CID, op: 'embed', fields: ['tpm'] },
    })
  })
})
