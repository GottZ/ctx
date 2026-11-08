// modelapi.ts binding gates (wave W05-05): listConsumers POSTs the exact
// model-consumer-list action + payload the Go handler reads
// (internal/handler/model_consumers_read.go consumerListRequest), hands the
// opaque next_cursor through untouched, and surfaces a {success:false}
// envelope (403 tenant_id is server-admin only, 400 invalid cursor) as
// ApiError. Pure node, fetch stubbed — mirrors keys.test.ts.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, configureApi } from '../api'
import {
  CONSUMER_STATUSES,
  DEFAULT_CONSUMER_STATUSES,
  createConsumer,
  deleteConsumer,
  listConsumers,
  listModelAliases,
  reactivateConsumer,
  revokeModelKey,
  type ConsumerCreateSpec,
} from './modelapi'
import { listScopes } from './scopes'

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

// --- W05-06 mutations (design/05 §4.2 rows Anlegen / Deaktivieren /
// Reaktivieren / Key widerrufen; payloads per W01-5 report §Action-Tabelle and
// internal/handler/model_consumers.go). ---

describe('createConsumer (model-consumer-create)', () => {
  const FIXED = Date.parse('2026-06-29T12:00:05Z')

  it('POSTs only the known fields in data — never kind, never expires_in_days', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, consumer: { id: 'c1' }, api_key: 'ctxm_x' }))
    const spec = {
      label: 'ci-app',
      description: 'nightly',
      tenant_id: 't-b',
      tenant_scope: 's1',
      models: ['a1', 'a2'],
      prompt_sensitivity: 'credentials',
      allowed_cidrs: ['10.0.0.0/8'],
      // an untyped extra field (a future/rogue caller) never reaches the wire
      kind: 'ctx-peer',
    } as unknown as ConsumerCreateSpec
    const res = await createConsumer(spec, () => FIXED)
    expect(res.api_key).toBe('ctxm_x')
    const body = sentBody(mock)
    expect(body).toEqual({
      action: 'model-consumer-create',
      data: {
        label: 'ci-app',
        description: 'nightly',
        tenant_id: 't-b',
        tenant_scope: 's1',
        models: ['a1', 'a2'],
        prompt_sensitivity: 'credentials',
        allowed_cidrs: ['10.0.0.0/8'],
      },
    })
    expect('kind' in (body.data as Record<string, unknown>)).toBe(false)
  })

  it('turns expires_in_days into an absolute expires_at from the injected clock', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, consumer: { id: 'c1' }, api_key: 'k' }))
    await createConsumer({ label: 'x', expires_in_days: 30 }, () => FIXED)
    expect(sentBody(mock)).toEqual({ action: 'model-consumer-create', data: { label: 'x', expires_at: '2026-07-29T12:00:05.000Z' } })
  })

  it('an absolute expires_at and all_models pass through; no expiry field ⇒ the server default', async () => {
    const mock = stubFetch(
      jsonResponse(200, { success: true, consumer: { id: 'c1' }, api_key: 'k' }),
      jsonResponse(200, { success: true, consumer: { id: 'c2' }, api_key: 'k' }),
    )
    await createConsumer({ label: 'x', expires_at: '2026-12-31T23:59:59.000Z', all_models: true })
    expect(sentBody(mock)).toEqual({ action: 'model-consumer-create', data: { label: 'x', expires_at: '2026-12-31T23:59:59.000Z', all_models: true } })
    await createConsumer({ label: 'y' })
    const second = JSON.parse(String((mock.mock.calls[1]?.[1] as RequestInit).body)) as Record<string, unknown>
    expect(second).toEqual({ action: 'model-consumer-create', data: { label: 'y' } })
  })

  it('surfaces a 400 with param models as ApiError carrying the envelope', async () => {
    stubFetch(jsonResponse(400, { success: false, error: 'models: x is not a model alias this consumer can be granted', param: 'models' }))
    const err = await createConsumer({ label: 'x', models: ['x'] }).catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect((err as ApiError).status).toBe(400)
    expect((err as ApiError).details?.param).toBe('models')
  })
})

describe('deactivate / reactivate / revoke payloads', () => {
  it('deleteConsumer → model-consumer-delete {data:{id}}', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, cancelled_inflight: null }))
    const res = await deleteConsumer('c1')
    expect(sentBody(mock)).toEqual({ action: 'model-consumer-delete', data: { id: 'c1' } })
    expect(res.cancelled_inflight).toBeNull()
  })

  it('reactivateConsumer → model-consumer-update {data:{id, active:true}}', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, consumer: { id: 'c1' } }))
    await reactivateConsumer('c1')
    expect(sentBody(mock)).toEqual({ action: 'model-consumer-update', data: { id: 'c1', active: true } })
  })

  it('revokeModelKey → model-key-revoke {data:{key_id}}', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, cancelled_inflight: 2 }))
    const res = await revokeModelKey('k1')
    expect(sentBody(mock)).toEqual({ action: 'model-key-revoke', data: { key_id: 'k1' } })
    expect(res.cancelled_inflight).toBe(2)
  })
})

describe('picker reads', () => {
  it('listModelAliases → model-alias-list without payload', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, model_aliases: [], count: 0 }))
    const res = await listModelAliases()
    expect(sentBody(mock)).toEqual({ action: 'model-alias-list' })
    expect(res.model_aliases).toEqual([])
  })

  it('listScopes(tenantId) carries the tenant as the top-level id (server-admin); without it the body is unchanged', async () => {
    const mock = stubFetch(
      jsonResponse(200, { success: true, scopes: [] }),
      jsonResponse(200, { success: true, scopes: [] }),
    )
    await listScopes('t-b')
    expect(sentBody(mock)).toEqual({ action: 'scope-list', id: 't-b' })
    await listScopes()
    expect(JSON.parse(String((mock.mock.calls[1]?.[1] as RequestInit).body))).toEqual({ action: 'scope-list' })
  })
})

// FOLGE-Z Z4-request (briefings/FOLGE-Z-INDEX.md §3): the create body carries
// the visible lane and the changed start limits — from an explicit field list,
// never a spread, so nothing outside §3 `L` reaches the wire.
describe('FOLGE-Z createConsumer: lane and start limits on the wire', () => {
  it('Z4-request gate: admission_lane and limits ride in data exactly as §3 names them', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, consumer: { id: 'c1' }, api_key: 'ctxm_x' }))
    await createConsumer({ label: 'x', admission_lane: 'urgent', limits: { chat: { max_concurrency: 8, rpm: 120 }, embed: { daily_tokens: 9_000_000 } } })
    expect(sentBody(mock), 'Z4-request: lane + limits on the wire').toEqual({
      action: 'model-consumer-create',
      data: { label: 'x', admission_lane: 'urgent', limits: { chat: { max_concurrency: 8, rpm: 120 }, embed: { daily_tokens: 9_000_000 } } },
    })
  })

  it('Z4-request gate: only §3 dimensions, only values the server accepts; empty ops and a foreign lane stay off the wire', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, consumer: { id: 'c1' }, api_key: 'ctxm_x' }))
    const spec = {
      label: 'x',
      admission_lane: 'interactive',
      limits: {
        chat: { rpm: 0, tpm: Number.POSITIVE_INFINITY, max_concurrency: 4, tier: 'server', allow_external: false },
        embed: {},
        bogus: { rpm: 5 },
        rerank: { daily_tokens: -1 },
      },
    } as unknown as ConsumerCreateSpec
    await createConsumer(spec)
    expect(sentBody(mock)).toEqual({ action: 'model-consumer-create', data: { label: 'x', limits: { chat: { max_concurrency: 4, allow_external: false } } } })
  })

  it('Z4-request: no limits at all ⇒ no limits key', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, consumer: { id: 'c1' }, api_key: 'ctxm_x' }))
    await createConsumer({ label: 'x', admission_lane: 'bulk', limits: {} })
    expect(sentBody(mock)).toEqual({ action: 'model-consumer-create', data: { label: 'x', admission_lane: 'bulk' } })
  })
})
