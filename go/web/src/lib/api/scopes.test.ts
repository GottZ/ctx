// scopes.ts binding gates (Wave FE-2): each function POSTs the exact manage
// action + payload the Go handler reads (BE5 scope-create/scope-list), and the
// apiFetch failure path surfaces as ApiError — a 400 name-charset reject, a 429
// quota cap and a 403 from enforceActionTier. Pure node, fetch stubbed — mirrors
// lib/api/keys.test.ts. The action strings are PINNED so a later backend rename
// (scope-create vs scope-assign, design 05 Offene #1) breaks here, not silently.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, configureApi } from '../api'
import { createScope, listScopes } from './scopes'

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
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

function path(mock: ReturnType<typeof vi.fn>): unknown {
  return mock.mock.calls[0]?.[0]
}

beforeEach(() => {
  vi.unstubAllGlobals()
  configureApi({ getKey: () => null, onUnauthorized: () => {} })
})
afterEach(() => vi.unstubAllGlobals())

describe('createScope', () => {
  it('POSTs scope-create with the bare name in data and renders the server-built scope', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, scope: 'acme:research', tenant_id: 't1' }))
    const res = await createScope({ name: 'research' })
    expect(path(mock)).toBe('/api/manage')
    expect(sentBody(mock)).toEqual({ action: 'scope-create', data: { name: 'research' } })
    expect(res.scope).toBe('acme:research')
  })

  it('carries the server-admin tenant_id override when present', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, scope: 'acme:ops', tenant_id: 't9' }))
    await createScope({ name: 'ops', tenant_id: 't9' })
    expect(sentBody(mock)).toEqual({ action: 'scope-create', data: { name: 'ops', tenant_id: 't9' } })
  })

  it('surfaces a 400 (malformed name) as ApiError (bad_request)', async () => {
    stubFetch(jsonResponse(400, { success: false, error: 'scope name may not contain a colon' }))
    await expect(createScope({ name: 'x:y' })).rejects.toMatchObject({ status: 400, code: 'bad_request' })
  })

  it('surfaces a 429 (over max_scopes) as ApiError (rate_limited)', async () => {
    stubFetch(jsonResponse(429, { success: false, error: 'scope limit reached' }))
    await expect(createScope({ name: 'one-too-many' })).rejects.toMatchObject({ status: 429, code: 'rate_limited' })
  })

  it('surfaces a 403 tier rejection as ApiError (forbidden)', async () => {
    stubFetch(jsonResponse(403, { success: false, error: 'admin access required' }))
    await expect(createScope({ name: 'x' })).rejects.toMatchObject({ status: 403, code: 'forbidden' })
  })
})

describe('listScopes', () => {
  it('POSTs scope-list with no payload (server filters on ar.TenantID)', async () => {
    const mock = stubFetch(
      jsonResponse(200, {
        success: true,
        scopes: [{ scope: 'acme:research', block_count: 3, key_count: 1, tenant_id: 't1' }],
      }),
    )
    const res = await listScopes()
    expect(path(mock)).toBe('/api/manage')
    expect(sentBody(mock)).toEqual({ action: 'scope-list' })
    expect('data' in sentBody(mock)).toBe(false)
    expect(res.scopes).toHaveLength(1)
    expect(res.scopes[0].scope).toBe('acme:research')
  })

  it('surfaces a 403 tier rejection as ApiError', async () => {
    stubFetch(jsonResponse(403, { success: false, error: 'admin access required' }))
    await expect(listScopes()).rejects.toBeInstanceOf(ApiError)
  })
})
