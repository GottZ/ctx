// api.ts gates (design 04-§5.5; Cookie-Session seit OAuth R4): envelope
// normalization, error mapping, CSRF-Synchronizer-Injektion und der
// 401-Refresh-Interceptor — pure node, fetch stubbed. Test keys are
// assembled at runtime; no secret-shaped literals (repo rule).

import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, apiFetch, configureApi, toApiError } from './api'

const TEST_KEY = ['cafe', 'f00d'].join('') // doc-value, assembled at runtime
const TEST_CSRF = ['c5', '4f'].join('') // doc-value, assembled at runtime

function jsonResponse(status: number, body: unknown, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', ...headers },
  })
}

function stubFetch(...responses: Response[]): ReturnType<typeof vi.fn> {
  const mock = vi.fn()
  for (const res of responses) mock.mockResolvedValueOnce(res)
  vi.stubGlobal('fetch', mock)
  return mock
}

beforeEach(() => {
  vi.unstubAllGlobals()
  configureApi({ getCsrfToken: () => null, onUnauthorized: () => {} })
})

describe('apiFetch', () => {
  it('returns the parsed body on success', async () => {
    stubFetch(jsonResponse(200, { success: true, label: 'example' }))
    const got = await apiFetch<{ success: boolean; label: string }>('/api/whoami')
    expect(got).toEqual({ success: true, label: 'example' })
  })

  it('sends the CSRF synchronizer on state-changing methods (cookie path)', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true }))
    configureApi({ getCsrfToken: () => TEST_CSRF })
    await apiFetch('/api/manage', { method: 'POST', body: '{}' })
    const init = mock.mock.calls[0]?.[1] as RequestInit
    const headers = new Headers(init.headers)
    expect(headers.get('X-CSRF-Token')).toBe(TEST_CSRF)
    // Kein gespeicherter Key mehr — das Cookie fährt automatisch mit.
    expect(headers.get('Authorization')).toBeNull()
  })

  it('sends no CSRF header on GET', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true }))
    configureApi({ getCsrfToken: () => TEST_CSRF })
    await apiFetch('/api/whoami')
    const init = mock.mock.calls[0]?.[1] as RequestInit
    expect(new Headers(init.headers).get('X-CSRF-Token')).toBeNull()
  })

  it('sends an explicit key as Bearer and never a CSRF header (probe path)', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true }))
    configureApi({ getCsrfToken: () => TEST_CSRF })
    await apiFetch('/api/whoami', { method: 'POST', body: '{}' }, { key: TEST_KEY })
    const init = mock.mock.calls[0]?.[1] as RequestInit
    const headers = new Headers(init.headers)
    expect(headers.get('Authorization')).toBe(`Bearer ${TEST_KEY}`)
    // Header-Credentials sind nie CSRF-gepflichtig (design 05 §4.4).
    expect(headers.get('X-CSRF-Token')).toBeNull()
  })

  it('sends no Authorization header without a key', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true }))
    await apiFetch('/api/whoami')
    const init = mock.mock.calls[0]?.[1] as RequestInit
    expect(new Headers(init.headers).get('Authorization')).toBeNull()
  })

  it('normalizes a success:false envelope inside HTTP 200 (heartbeat path)', async () => {
    stubFetch(
      jsonResponse(200, { success: false, error: 'synthesis failed' }, { 'X-Request-ID': 'req-7' }),
    )
    await expect(apiFetch('/api/query')).rejects.toMatchObject({
      status: 200,
      code: 'api_error',
      message: 'synthesis failed',
      requestId: 'req-7',
    })
  })

  it('parses bodies with leading keepalive whitespace', async () => {
    stubFetch(
      new Response('   \n  {"success":true,"n":1}', {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    )
    await expect(apiFetch('/api/query')).resolves.toEqual({ success: true, n: 1 })
  })

  it('replays the request ONCE after a successful silent refresh on 401', async () => {
    const mock = stubFetch(
      jsonResponse(401, { error: 'unauthorized' }), // Daten-Request, Access-Token tot
      jsonResponse(200, { success: true }), // POST /auth/refresh
      jsonResponse(200, { success: true, n: 2 }), // Replay
    )
    const onUnauthorized = vi.fn()
    configureApi({ onUnauthorized })
    await expect(apiFetch('/api/search')).resolves.toEqual({ success: true, n: 2 })
    expect(mock.mock.calls[1]?.[0]).toBe('/auth/refresh')
    expect(mock).toHaveBeenCalledTimes(3)
    expect(onUnauthorized).not.toHaveBeenCalled()
  })

  it('fires the unauthorized hook when the refresh fails too (dead session)', async () => {
    const mock = stubFetch(
      jsonResponse(401, { error: 'unauthorized' }, { 'X-Request-ID': 'req-9' }),
      jsonResponse(401, { success: false, error: 'authentication failed' }), // refresh tot
    )
    const onUnauthorized = vi.fn()
    configureApi({ onUnauthorized })
    await expect(apiFetch('/api/whoami')).rejects.toMatchObject({
      status: 401,
      code: 'unauthorized',
    })
    expect(mock).toHaveBeenCalledTimes(2)
    expect(onUnauthorized).toHaveBeenCalledTimes(1)
  })

  it('fires the hook when the replay after a good refresh still 401s (no loop)', async () => {
    const mock = stubFetch(
      jsonResponse(401, { error: 'unauthorized' }),
      jsonResponse(200, { success: true }), // refresh ok …
      jsonResponse(401, { error: 'unauthorized' }), // … aber Replay wieder 401
    )
    const onUnauthorized = vi.fn()
    configureApi({ onUnauthorized })
    await expect(apiFetch('/api/whoami')).rejects.toMatchObject({ status: 401 })
    expect(mock).toHaveBeenCalledTimes(3) // genau EIN Replay, kein Loop
    expect(onUnauthorized).toHaveBeenCalledTimes(1)
  })

  it('does NOT refresh or fire the hook on 401 with an explicit key (probe)', async () => {
    const mock = stubFetch(jsonResponse(401, { error: 'unauthorized' }))
    const onUnauthorized = vi.fn()
    configureApi({ onUnauthorized })
    await expect(apiFetch('/api/whoami', {}, { key: TEST_KEY })).rejects.toMatchObject({
      status: 401,
      code: 'unauthorized',
    })
    expect(mock).toHaveBeenCalledTimes(1)
    expect(onUnauthorized).not.toHaveBeenCalled()
  })

  it('does NOT refresh or fire the hook with skipRefresh (lifecycle calls)', async () => {
    const mock = stubFetch(jsonResponse(401, { error: 'unauthorized' }))
    const onUnauthorized = vi.fn()
    configureApi({ onUnauthorized })
    await expect(apiFetch('/auth/login', { method: 'POST', body: '{}' }, { skipRefresh: true })).rejects.toMatchObject(
      { status: 401 },
    )
    expect(mock).toHaveBeenCalledTimes(1)
    expect(onUnauthorized).not.toHaveBeenCalled()
  })

  it('maps 403 to forbidden without touching the session', async () => {
    stubFetch(jsonResponse(403, { success: false, error: 'admin key required' }))
    const onUnauthorized = vi.fn()
    configureApi({ onUnauthorized })
    await expect(apiFetch('/api/settings')).rejects.toMatchObject({
      status: 403,
      code: 'forbidden',
      message: 'admin key required',
    })
    expect(onUnauthorized).not.toHaveBeenCalled()
  })

  it('maps 5xx to server with the envelope message', async () => {
    stubFetch(jsonResponse(500, { success: false, error: 'Internal server error' }))
    await expect(apiFetch('/api/whoami')).rejects.toMatchObject({
      status: 500,
      code: 'server',
      message: 'Internal server error',
    })
  })

  it('maps a fetch rejection to a network ApiError', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValueOnce(new TypeError('fetch failed')))
    await expect(apiFetch('/api/whoami')).rejects.toMatchObject({
      status: 0,
      code: 'network',
    })
  })

  it('rejects 2xx non-JSON bodies (proxy error pages)', async () => {
    stubFetch(new Response('<html>gateway</html>', { status: 200 }))
    await expect(apiFetch('/api/whoami')).rejects.toMatchObject({ code: 'invalid_response' })
  })
})

describe('toApiError', () => {
  it('passes ApiError through and wraps everything else', () => {
    const original = new ApiError(404, 'not_found', 'nope')
    expect(toApiError(original)).toBe(original)
    const wrapped = toApiError(new Error('boom'))
    expect(wrapped).toBeInstanceOf(ApiError)
    expect(wrapped.message).toBe('boom')
    expect(wrapped.status).toBe(0)
  })
})
