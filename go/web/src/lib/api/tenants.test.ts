// tenants.ts binding gates (Wave A1): each function POSTs the exact manage
// action + payload the Go handler reads (tenant_manage.go), and the apiFetch
// failure path (a 403 from enforceActionTier) surfaces as ApiError. Pure node,
// fetch stubbed — mirrors lib/api/blocks.test.ts.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, configureApi } from '../api'
import {
  createTenant,
  createTenantGrant,
  DEFAULT_TENANT_ID,
  deleteTenant,
  deleteTenantGrant,
  getTenant,
  getTenantUsage,
  listTenantGrants,
  listTenants,
  setTenantLimits,
  updateTenant,
} from './tenants'

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

const aTenant = {
  id: 't1',
  slug: 'team-mueller',
  display_name: 'Team Müller',
  status: 'active',
  created_at: '2026-06-29T00:00:00Z',
  updated_at: '2026-06-29T00:00:00Z',
}

describe('createTenant', () => {
  it('POSTs tenant-create and parses the FLAT compound result (tenant + scope + reveal-once owner_key)', async () => {
    const mock = stubFetch(
      jsonResponse(200, {
        success: true,
        tenant: aTenant,
        scope: 'team-mueller:default',
        owner_key_id: 'k1',
        owner_key: 'ctx_live_owner_key_shown_once',
      }),
    )
    const res = await createTenant({ slug: 'team-mueller', display_name: 'Team Müller' })
    expect(path(mock)).toBe('/api/manage')
    expect(sentBody(mock)).toEqual({
      action: 'tenant-create',
      data: { slug: 'team-mueller', display_name: 'Team Müller' },
    })
    expect(res.tenant.id).toBe('t1')
    expect(res.scope).toBe('team-mueller:default')
    expect(res.owner_key).toBe('ctx_live_owner_key_shown_once')
  })

  it('seeds the structural limits into data when given (null = unlimited)', async () => {
    const mock = stubFetch(
      jsonResponse(200, { success: true, tenant: aTenant, scope: 's', owner_key_id: 'k1', owner_key: 'x' }),
    )
    await createTenant({ slug: 'acme', display_name: 'Acme', max_scopes: 5, max_keys: null })
    expect(sentBody(mock)).toEqual({
      action: 'tenant-create',
      data: { slug: 'acme', display_name: 'Acme', max_scopes: 5, max_keys: null },
    })
  })
})

describe('getTenantUsage', () => {
  const usage = { tenant_id: 't1', max_scopes: 5, max_keys: null, scope_count: 2, key_count: 1 }

  it('POSTs tenant-usage-get with no id (tenant-admin, server-pinned) and unwraps the view', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, usage }))
    const res = await getTenantUsage()
    expect(sentBody(mock)).toEqual({ action: 'tenant-usage-get' })
    expect('id' in sentBody(mock)).toBe(false)
    expect(res.scope_count).toBe(2)
    expect(res.max_keys).toBeNull()
  })

  it('carries a top-level id for the server-admin cross-tenant read', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, usage }))
    await getTenantUsage('t1')
    expect(sentBody(mock)).toEqual({ action: 'tenant-usage-get', id: 't1' })
  })

  it('surfaces a 403 (cross-tenant counts) as ApiError', async () => {
    stubFetch(jsonResponse(403, { success: false, error: 'admin access required' }))
    await expect(getTenantUsage('foreign')).rejects.toMatchObject({ status: 403, code: 'forbidden' })
  })
})

describe('setTenantLimits', () => {
  it('POSTs tenant-limit-set with a top-level id and both limits inside data', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, tenant: { ...aTenant, max_scopes: 10, max_keys: null } }))
    const res = await setTenantLimits('t1', { max_scopes: 10, max_keys: null })
    expect(sentBody(mock)).toEqual({
      action: 'tenant-limit-set',
      id: 't1',
      data: { max_scopes: 10, max_keys: null },
    })
    expect(res.tenant.max_scopes).toBe(10)
  })

  it('surfaces a 400 (a limit missing — both required) as ApiError', async () => {
    stubFetch(jsonResponse(400, { success: false, error: 'both max_scopes and max_keys are required' }))
    await expect(setTenantLimits('t1', { max_scopes: 1, max_keys: null })).rejects.toBeInstanceOf(ApiError)
  })
})

describe('DEFAULT_TENANT_ID', () => {
  it('matches store.DefaultTenantID (the delete-disabled guard value)', () => {
    expect(DEFAULT_TENANT_ID).toBe('00000000-0000-0000-0000-0000000d3fa0')
  })
})

describe('listTenants', () => {
  it('POSTs tenant-list with no extra fields', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, tenants: [aTenant] }))
    const res = await listTenants()
    expect(sentBody(mock)).toEqual({ action: 'tenant-list' })
    expect(res.tenants).toHaveLength(1)
  })
})

describe('getTenant', () => {
  it('POSTs tenant-get with a top-level id', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, tenant: aTenant }))
    await getTenant('t1')
    expect(sentBody(mock)).toEqual({ action: 'tenant-get', id: 't1' })
  })
})

describe('updateTenant', () => {
  it('carries status as a TOP-LEVEL field (not inside data)', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, tenant: aTenant }))
    await updateTenant('t1', { status: 'suspended' })
    expect(sentBody(mock)).toEqual({ action: 'tenant-update', id: 't1', status: 'suspended' })
  })

  it('puts display_name inside data and omits an absent status', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, tenant: aTenant }))
    await updateTenant('t1', { display_name: 'Renamed' })
    expect(sentBody(mock)).toEqual({ action: 'tenant-update', id: 't1', data: { display_name: 'Renamed' } })
  })

  it('sends both status (top-level) and display_name (data) when given', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, tenant: aTenant }))
    await updateTenant('t1', { status: 'active', display_name: 'Both' })
    expect(sentBody(mock)).toEqual({
      action: 'tenant-update',
      id: 't1',
      status: 'active',
      data: { display_name: 'Both' },
    })
  })
})

describe('deleteTenant', () => {
  it('POSTs tenant-delete with the id and returns the deleted id', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, deleted: 't1' }))
    const res = await deleteTenant('t1')
    expect(sentBody(mock)).toEqual({ action: 'tenant-delete', id: 't1' })
    expect(res.deleted).toBe('t1')
  })

  it('surfaces the 400 default-tenant guard as ApiError', async () => {
    stubFetch(jsonResponse(400, { success: false, error: 'the default tenant cannot be deleted' }))
    await expect(deleteTenant('default-id')).rejects.toBeInstanceOf(ApiError)
  })
})

describe('tenant grants', () => {
  const aGrant = {
    id: 'g1',
    grantee_tenant: 't2',
    granted_scope: 'work',
    created_at: '2026-06-29T00:00:00Z',
    created_by: 'k1',
  }

  it('createTenantGrant POSTs the pair in data', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, grant: aGrant }))
    await createTenantGrant({ grantee_tenant: 't2', granted_scope: 'work' })
    expect(sentBody(mock)).toEqual({
      action: 'tenant-grant-create',
      data: { grantee_tenant: 't2', granted_scope: 'work' },
    })
  })

  it('listTenantGrants omits id for the global list', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, grants: [aGrant] }))
    await listTenantGrants()
    expect(sentBody(mock)).toEqual({ action: 'tenant-grant-list' })
    expect('id' in sentBody(mock)).toBe(false)
  })

  it('listTenantGrants narrows by grantee tenant via id', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, grants: [] }))
    await listTenantGrants('t2')
    expect(sentBody(mock)).toEqual({ action: 'tenant-grant-list', id: 't2' })
  })

  it('deleteTenantGrant POSTs the grant id', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, deleted: 'g1' }))
    const res = await deleteTenantGrant('g1')
    expect(sentBody(mock)).toEqual({ action: 'tenant-grant-delete', id: 'g1' })
    expect(res.deleted).toBe('g1')
  })

  it('surfaces a 403 tier rejection as ApiError (forbidden)', async () => {
    stubFetch(jsonResponse(403, { success: false, error: 'admin access required' }))
    await expect(createTenantGrant({ grantee_tenant: 't2', granted_scope: 'work' })).rejects.toMatchObject({
      status: 403,
      code: 'forbidden',
    })
  })
})
