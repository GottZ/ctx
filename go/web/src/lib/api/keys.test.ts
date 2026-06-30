// keys.ts binding gates (Wave TK1): each function POSTs the exact manage action +
// payload the Go handler reads (context_manage.go handleApiKey*), and the apiFetch
// failure path surfaces as ApiError — both a 403 from enforceActionTier and the
// api-key-delete 404-no-oracle ({success:false} inside an HTTP 200). Pure node,
// fetch stubbed — mirrors lib/api/quota.test.ts. getTenantQuota is re-exported
// from quota.ts; we assert keys.ts surfaces it and fires the right action.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, configureApi } from '../api'
import { createApiKey, deleteApiKey, getTenantQuota, listApiKeys, updateApiKey } from './keys'

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

const aKey = {
  id: 'k1',
  label: 'deploy-bot',
  home_scope: 'work',
  allowed_scopes: ['work', 'shared'],
  active: true,
  created_at: '2026-06-29T00:00:00Z',
}

describe('createApiKey', () => {
  it('POSTs api-key-create with the spec (incl. required home_scope) in data', async () => {
    const mock = stubFetch(
      jsonResponse(200, {
        success: true,
        id: 'k1',
        label: 'deploy-bot',
        home_scope: 'work',
        allowed_scopes: ['work'],
        api_key: 'ctx_live_plaintext_shown_once',
      }),
    )
    const res = await createApiKey({ label: 'deploy-bot', home_scope: 'work', allowed_scopes: ['work'] })
    expect(path(mock)).toBe('/api/manage')
    expect(sentBody(mock)).toEqual({
      action: 'api-key-create',
      data: { label: 'deploy-bot', home_scope: 'work', allowed_scopes: ['work'] },
    })
    expect(res.api_key).toBe('ctx_live_plaintext_shown_once')
  })

  it('carries home_scope even when allowed_scopes is omitted', async () => {
    const mock = stubFetch(
      jsonResponse(200, { success: true, id: 'k2', label: 'min', home_scope: 'work', allowed_scopes: [], api_key: 'x' }),
    )
    await createApiKey({ label: 'min', home_scope: 'work' })
    const body = sentBody(mock) as { action: string; data: Record<string, unknown> }
    expect(body.action).toBe('api-key-create')
    expect(body.data.home_scope).toBe('work')
    expect('allowed_scopes' in body.data).toBe(false)
  })

  it('carries the server-admin tenant_id override into data when present', async () => {
    const mock = stubFetch(
      jsonResponse(200, { success: true, id: 'k3', label: 'recovery', home_scope: 'acme:ops', allowed_scopes: [], api_key: 'x' }),
    )
    await createApiKey({ label: 'recovery', home_scope: 'acme:ops', tenant_id: 't9' })
    expect(sentBody(mock)).toEqual({
      action: 'api-key-create',
      data: { label: 'recovery', home_scope: 'acme:ops', tenant_id: 't9' },
    })
  })

  it('surfaces a 403 (scope outside tenant) as ApiError (forbidden)', async () => {
    stubFetch(jsonResponse(403, { success: false, error: 'cannot create keys outside your tenant' }))
    await expect(createApiKey({ label: 'x', home_scope: 'foreign' })).rejects.toMatchObject({
      status: 403,
      code: 'forbidden',
    })
  })
})

describe('updateApiKey', () => {
  it('POSTs api-key-update with the spec in data and returns the re-read row', async () => {
    const mock = stubFetch(
      jsonResponse(200, { success: true, key: { ...aKey, tenant_role: 'admin' } }),
    )
    const res = await updateApiKey({ id: 'k1', tenant_role: 'admin' })
    expect(path(mock)).toBe('/api/manage')
    expect(sentBody(mock)).toEqual({ action: 'api-key-update', data: { id: 'k1', tenant_role: 'admin' } })
    expect(res.key.tenant_role).toBe('admin')
  })

  it('carries the active flag alone when only (de)activation is requested', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, key: { ...aKey, active: false } }))
    await updateApiKey({ id: 'k1', active: false })
    expect(sentBody(mock)).toEqual({ action: 'api-key-update', data: { id: 'k1', active: false } })
  })

  it('surfaces a 409 (last-owner demote) as ApiError (conflict)', async () => {
    stubFetch(jsonResponse(409, { success: false, error: 'cannot demote the last owner' }))
    await expect(updateApiKey({ id: 'k1', tenant_role: 'member' })).rejects.toMatchObject({
      status: 409,
      code: 'conflict',
    })
  })

  it('surfaces a 403 tier rejection as ApiError', async () => {
    stubFetch(jsonResponse(403, { success: false, error: 'admin access required' }))
    await expect(updateApiKey({ id: 'k1', active: true })).rejects.toBeInstanceOf(ApiError)
  })
})

describe('listApiKeys', () => {
  it('POSTs api-key-list with no data when active_only is omitted (server default true)', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, keys: [aKey] }))
    const res = await listApiKeys()
    expect(sentBody(mock)).toEqual({ action: 'api-key-list' })
    expect('data' in sentBody(mock)).toBe(false)
    expect(res.keys).toHaveLength(1)
  })

  it('sends active_only=false in data to include revoked keys', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, keys: [] }))
    await listApiKeys(false)
    expect(sentBody(mock)).toEqual({ action: 'api-key-list', data: { active_only: false } })
  })

  it('sends active_only=true in data when explicitly requested', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, keys: [aKey] }))
    await listApiKeys(true)
    expect(sentBody(mock)).toEqual({ action: 'api-key-list', data: { active_only: true } })
  })

  it('surfaces a 403 tier rejection as ApiError', async () => {
    stubFetch(jsonResponse(403, { success: false, error: 'admin access required' }))
    await expect(listApiKeys()).rejects.toBeInstanceOf(ApiError)
  })
})

describe('deleteApiKey', () => {
  it('POSTs api-key-delete with the id inside data (not a top-level id)', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, deleted: 'k1' }))
    const res = await deleteApiKey('k1')
    expect(sentBody(mock)).toEqual({ action: 'api-key-delete', data: { id: 'k1' } })
    expect('id' in sentBody(mock)).toBe(false)
    expect(res.deleted).toBe('k1')
  })

  it('surfaces the 404-no-oracle ({success:false} inside HTTP 200) as ApiError', async () => {
    stubFetch(jsonResponse(200, { success: false, error: 'key not found' }))
    await expect(deleteApiKey('nope')).rejects.toMatchObject({ code: 'api_error' })
  })
})

describe('getTenantQuota (re-exported from quota.ts)', () => {
  it('fires tenant-quota-get with no payload (own-scope default)', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, quota: { scope: 'work', enabled: false, unlimited: true } }))
    const res = await getTenantQuota()
    expect(path(mock)).toBe('/api/manage')
    expect(sentBody(mock)).toEqual({ action: 'tenant-quota-get' })
    expect(res.quota.unlimited).toBe(true)
  })
})
