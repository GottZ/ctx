// api.ts gates (design 04-§5.5): envelope normalization, error mapping and
// the 401 interceptor — pure node, fetch stubbed. Test keys are assembled at
// runtime; no secret-shaped literals (repo rule).

import { beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, apiFetch, configureApi, toApiError } from './api'

const TEST_KEY = ['cafe', 'f00d'].join('') // doc-value, assembled at runtime

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
  configureApi({ getKey: () => null, onUnauthorized: () => {} })
})

describe('apiFetch', () => {
  it('returns the parsed body on success', async () => {
    stubFetch(jsonResponse(200, { success: true, label: 'example' }))
    const got = await apiFetch<{ success: boolean; label: string }>('/api/whoami')
    expect(got).toEqual({ success: true, label: 'example' })
  })

  it('sends the stored key as a Bearer header', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true }))
    configureApi({ getKey: () => TEST_KEY })
    await apiFetch('/api/whoami')
    const init = mock.mock.calls[0]?.[1] as RequestInit
    expect(new Headers(init.headers).get('Authorization')).toBe(`Bearer ${TEST_KEY}`)
  })

  it('prefers an explicit key over the stored one', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true }))
    configureApi({ getKey: () => 'stored-key' })
    await apiFetch('/api/whoami', {}, { key: TEST_KEY })
    const init = mock.mock.calls[0]?.[1] as RequestInit
    expect(new Headers(init.headers).get('Authorization')).toBe(`Bearer ${TEST_KEY}`)
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

  it('fires the unauthorized hook on 401 with the stored key', async () => {
    stubFetch(jsonResponse(401, { error: 'unauthorized' }, { 'X-Request-ID': 'req-9' }))
    const onUnauthorized = vi.fn()
    configureApi({ getKey: () => TEST_KEY, onUnauthorized })
    await expect(apiFetch('/api/whoami')).rejects.toMatchObject({
      status: 401,
      code: 'unauthorized',
      requestId: 'req-9',
    })
    expect(onUnauthorized).toHaveBeenCalledTimes(1)
  })

  it('does NOT fire the unauthorized hook on 401 with an explicit key (login probe)', async () => {
    stubFetch(jsonResponse(401, { error: 'unauthorized' }))
    const onUnauthorized = vi.fn()
    configureApi({ onUnauthorized })
    await expect(apiFetch('/api/whoami', {}, { key: TEST_KEY })).rejects.toMatchObject({
      status: 401,
      code: 'unauthorized',
    })
    expect(onUnauthorized).not.toHaveBeenCalled()
  })

  it('maps 403 to forbidden without touching the session', async () => {
    stubFetch(jsonResponse(403, { success: false, error: 'admin key required' }))
    const onUnauthorized = vi.fn()
    configureApi({ getKey: () => TEST_KEY, onUnauthorized })
    await expect(apiFetch('/api/settings')).rejects.toMatchObject({
      status: 403,
      code: 'forbidden',
      message: 'admin key required',
    })
    expect(onUnauthorized).not.toHaveBeenCalled()
  })

  it('maps 5xx to server with the envelope message', async () => {
    stubFetch(jsonResponse(500, { success: false, error: 'internal error' }))
    await expect(apiFetch('/api/whoami')).rejects.toMatchObject({
      status: 500,
      code: 'server',
      message: 'internal error',
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
