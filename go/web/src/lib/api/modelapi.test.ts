// modelapi.ts binding gates (wave W05-05): listConsumers POSTs the exact
// model-consumer-list action + payload the Go handler reads
// (internal/handler/model_consumers_read.go consumerListRequest), hands the
// opaque next_cursor through untouched, and surfaces a {success:false}
// envelope (403 tenant_id is server-admin only, 400 invalid cursor) as
// ApiError. Pure node, fetch stubbed — mirrors keys.test.ts.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, configureApi } from '../api'
import { CONSUMER_STATUSES, DEFAULT_CONSUMER_STATUSES, listConsumers } from './modelapi'

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

function stubFetch(...responses: Response[]): ReturnType<typeof vi.fn> {
  const mock = vi.fn()
  for (const res of responses) mock.mockResolvedValueOnce(res)
  vi.stubGlobal('fetch', mock)
  return mock
}

function sentBody(mock: ReturnType<typeof vi.fn>): Record<string, unknown> {
  const init = mock.mock.calls[0]?.[1] as RequestInit
  return JSON.parse(String(init.body)) as Record<string, unknown>
}

beforeEach(() => {
  vi.unstubAllGlobals()
  configureApi({ getCsrfToken: () => null, onUnauthorized: () => {} })
})
afterEach(() => vi.unstubAllGlobals())

describe('status vocabulary', () => {
  it('mirrors store.ConsumerStatuses() order; the default is the four statuses of an active consumer', () => {
    expect(CONSUMER_STATUSES).toEqual(['active', 'expiring', 'rotating', 'no_key', 'inactive'])
    expect(DEFAULT_CONSUMER_STATUSES).toEqual(['active', 'expiring', 'rotating', 'no_key'])
  })
})

describe('listConsumers', () => {
  it('POSTs model-consumer-list with the given filter in data', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, consumers: [], next_cursor: null }))
    await listConsumers({ status: ['active', 'inactive'], q: 'ci-', tenant_id: 't-b', cursor: 'opaque==', limit: 25 })
    expect(mock.mock.calls[0]?.[0]).toBe('/api/manage')
    expect((mock.mock.calls[0]?.[1] as RequestInit).method).toBe('POST')
    expect(sentBody(mock)).toEqual({
      action: 'model-consumer-list',
      data: { status: ['active', 'inactive'], q: 'ci-', tenant_id: 't-b', cursor: 'opaque==', limit: 25 },
    })
  })

  it('omits every unset field so the server applies its own defaults (status, page cap)', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, consumers: [], next_cursor: null }))
    await listConsumers({ q: '', tenant_id: '', cursor: '' })
    expect(sentBody(mock)).toEqual({ action: 'model-consumer-list', data: {} })
  })

  it('returns consumers and the opaque next_cursor unchanged', async () => {
    const next = 'MjAyNi0wNi0yOVQxMTowMDowMFp8eA'
    stubFetch(jsonResponse(200, { success: true, consumers: [{ id: 'c1', label: 'ci' }], next_cursor: next }))
    const res = await listConsumers()
    expect(res.next_cursor).toBe(next)
    expect(res.consumers.map((c) => c.id)).toEqual(['c1'])
  })

  it('surfaces a {success:false} refusal as ApiError (403 tenant_id is server-admin only)', async () => {
    stubFetch(jsonResponse(403, { success: false, error: 'tenant_id is server-admin only' }))
    const err = await listConsumers({ tenant_id: 't-x' }).catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect((err as ApiError).status).toBe(403)
    expect((err as ApiError).message).toBe('tenant_id is server-admin only')
  })
})
