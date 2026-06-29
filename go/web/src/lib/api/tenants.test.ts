// tenants.ts binding gates (Wave A1): each function POSTs the exact manage
// action + payload the Go handler reads (tenant_manage.go), and the apiFetch
// failure path (a 403 from enforceActionTier) surfaces as ApiError. Pure node,
// fetch stubbed — mirrors lib/api/blocks.test.ts.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, configureApi } from '../api'
import {
  createTenant,
  createTenantGrant,
  deleteTenant,
  deleteTenantGrant,
  getTenant,
  listTenantGrants,
  listTenants,
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
  it('POSTs tenant-create with the spec in data', async () => {
    const mock = stubFetch(jsonResponse(200, { success: true, tenant: aTenant }))
    const res = await createTenant({ slug: 'team-mueller', display_name: 'Team Müller' })
    expect(path(mock)).toBe('/api/manage')
    expect(sentBody(mock)).toEqual({
      action: 'tenant-create',
      data: { slug: 'team-mueller', display_name: 'Team Müller' },
    })
    expect(res.tenant.id).toBe('t1')
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
