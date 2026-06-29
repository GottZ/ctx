// TenantsModel gates (Wave A2): load → ready + populated list; load → error
// maps the thrown value to ApiError (the server 403 a non-admin key gets is the
// real-world failure path). Read-only model — no mutation tests (that is A3).

import { describe, expect, it } from 'vitest'
import { ApiError } from '../../lib/api'
import type { Tenant, TenantListResponse } from '../../lib/api/types'
import { TenantsModel } from './tenants.svelte'

function tenant(p: Partial<Tenant> & Pick<Tenant, 'slug'>): Tenant {
  return {
    id: `id-${p.slug}`,
    display_name: p.slug,
    status: 'active',
    created_at: '2026-06-29T00:00:00Z',
    updated_at: '2026-06-29T00:00:00Z',
    ...p,
  }
}

function fakeApi(tenants: Tenant[], fail?: ApiError) {
  let calls = 0
  return {
    get calls() {
      return calls
    },
    list: (): Promise<TenantListResponse> => {
      calls++
      if (fail) return Promise.reject(fail)
      return Promise.resolve({ success: true, tenants })
    },
  }
}

describe('TenantsModel load', () => {
  it('populates and reaches ready', async () => {
    const api = fakeApi([tenant({ slug: 'default' }), tenant({ slug: 'team-mueller' })])
    const m = new TenantsModel(api)
    await m.load()
    expect(m.status).toBe('ready')
    expect(m.tenants).toHaveLength(2)
    expect(m.tenants[1].slug).toBe('team-mueller')
    expect(m.loadError).toBeNull()
  })

  it('reaches ready with only the default tenant (empty signal)', async () => {
    const api = fakeApi([tenant({ slug: 'default' })])
    const m = new TenantsModel(api)
    await m.load()
    expect(m.status).toBe('ready')
    expect(m.tenants).toHaveLength(1)
  })

  it('maps a thrown ApiError to the error state (403 non-admin)', async () => {
    const api = fakeApi([], new ApiError(403, 'forbidden', 'server-admin required'))
    const m = new TenantsModel(api)
    await m.load()
    expect(m.status).toBe('error')
    expect(m.loadError).toBeInstanceOf(ApiError)
    expect(m.loadError?.status).toBe(403)
    expect(m.tenants).toHaveLength(0)
  })

  it('normalizes a non-ApiError throw via toApiError', async () => {
    const api = {
      list: (): Promise<TenantListResponse> => Promise.reject(new Error('boom')),
    }
    const m = new TenantsModel(api)
    await m.load()
    expect(m.status).toBe('error')
    expect(m.loadError).toBeInstanceOf(ApiError)
    expect(m.loadError?.status).toBe(0)
  })

  it('reload re-invokes the list action', async () => {
    const api = fakeApi([tenant({ slug: 'default' })])
    const m = new TenantsModel(api)
    await m.load()
    await m.reload()
    expect(api.calls).toBe(2)
  })
})
