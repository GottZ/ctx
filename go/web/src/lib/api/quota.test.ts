// quota.ts binding gates (Wave A1): the quota actions are SCOPE-keyed. set always
// carries `scope` in data; get optionally targets a scope (server-admin path);
// null budget fields ride through verbatim (= unlimited for that dimension); a
// 403 (tenant-admin setting a foreign scope) surfaces as ApiError. Pure node.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, configureApi } from '../api'
import { getTenantQuota, setTenantQuota } from './quota'

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

beforeEach(() => {
  vi.unstubAllGlobals()
  configureApi({ getCsrfToken: () => null, onUnauthorized: () => {} })
})
afterEach(() => vi.unstubAllGlobals())

describe('getTenantQuota', () => {
  it('POSTs tenant-quota-get with no payload (own-scope default)', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, quota: { scope: 'work', enabled: false, unlimited: true } }))
    const res = await getTenantQuota()
    expect(mock.mock.calls[0]?.[0]).toBe('/api/manage')
    expect(sentBody(mock)).toEqual({ action: 'tenant-quota-get' })
    expect(res.quota.unlimited).toBe(true)
  })

  it('carries the target scope in data (server-admin path)', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, quota: { scope: 'work', enabled: true } }))
    await getTenantQuota('work')
    const body = sentBody(mock) as { action: string; data: { scope: string } }
    expect(body.action).toBe('tenant-quota-get')
    expect(body.data.scope).toBe('work')
  })
})

describe('setTenantQuota', () => {
  it('always carries scope in data', async () => {
    const mock = stubFetch(
      jsonResponse(200, {
        success: true,
        quota: { scope: 'work', enabled: true, daily_cost_usd: 5, monthly_cost_usd: null, daily_calls: null, on_exceed: 'external_off' },
      }),
    )
    await setTenantQuota({ scope: 'work', daily_cost_usd: 5, on_exceed: 'external_off' })
    const body = sentBody(mock) as { action: string; data: Record<string, unknown> }
    expect(body.action).toBe('tenant-quota-set')
    expect(body.data.scope).toBe('work')
    expect(body.data.daily_cost_usd).toBe(5)
    expect(body.data.on_exceed).toBe('external_off')
  })

  it('passes explicit null budget fields through (null = unlimited dimension)', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, quota: { scope: 'work', enabled: true } }))
    await setTenantQuota({ scope: 'work', daily_cost_usd: null, monthly_cost_usd: null, daily_calls: null })
    const body = sentBody(mock) as { data: Record<string, unknown> }
    expect(body.data.daily_cost_usd).toBeNull()
    expect(body.data.monthly_cost_usd).toBeNull()
    expect(body.data.daily_calls).toBeNull()
  })

  it('surfaces a 403 (foreign-scope set by tenant-admin) as ApiError', async () => {
    stubFetch(jsonResponse(403, { success: false, error: 'admin access required' }))
    await expect(setTenantQuota({ scope: 'foreign' })).rejects.toMatchObject({ status: 403, code: 'forbidden' })
  })

  it('surfaces the 400 reserved-scope rejection as ApiError', async () => {
    stubFetch(jsonResponse(400, { success: false, error: 'quota scope must be a tenant scope' }))
    await expect(setTenantQuota({ scope: '_global' })).rejects.toBeInstanceOf(ApiError)
  })
})
