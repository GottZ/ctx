// TenantsModel gates: load → ready + populated list; load → error maps the
// thrown value to ApiError (the server 403 a non-admin key gets is the real-world
// failure path). Lifecycle (FE-M1, design 05 §3): create returns the reveal-once
// result and never retains the owner-key plaintext; suspend/activate patch the
// status + reload; remove deletes + reloads; busyId guards the in-flight row and a
// failure lands on actionError + rethrows.

import { describe, expect, it } from 'vitest'
import { ApiError } from '../../lib/api'
import type {
  Tenant,
  TenantCreateResult,
  TenantDeleteResponse,
  TenantListResponse,
  TenantResponse,
} from '../../lib/api/types'
import type { TenantSpec } from '../../lib/api/tenants'
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

interface MutCall {
  m: 'create' | 'update' | 'del'
  id?: string
  status?: string
  spec?: TenantSpec
}

function fakeApi(
  tenants: Tenant[],
  fail?: ApiError,
  mutFail?: Partial<Record<MutCall['m'], ApiError>>,
) {
  let calls = 0
  const log: MutCall[] = []
  return {
    get calls() {
      return calls
    },
    log,
    list: (): Promise<TenantListResponse> => {
      calls++
      if (fail) return Promise.reject(fail)
      return Promise.resolve({ success: true, tenants })
    },
    create: (spec: TenantSpec): Promise<TenantCreateResult> => {
      log.push({ m: 'create', spec })
      if (mutFail?.create) return Promise.reject(mutFail.create)
      return Promise.resolve({
        success: true,
        tenant: tenant({ slug: spec.slug, display_name: spec.display_name }),
        scope: `${spec.slug}:main`,
        owner_key_id: 'ok-id',
        owner_key: 'ctx_OWNER_PLAINTEXT_ONCE',
      })
    },
    update: (id: string, patch: { status?: string }): Promise<TenantResponse> => {
      log.push({ m: 'update', id, status: patch.status })
      if (mutFail?.update) return Promise.reject(mutFail.update)
      return Promise.resolve({ success: true, tenant: tenant({ slug: id }) })
    },
    del: (id: string): Promise<TenantDeleteResponse> => {
      log.push({ m: 'del', id })
      if (mutFail?.del) return Promise.reject(mutFail.del)
      return Promise.resolve({ success: true, deleted: id })
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
    const api = { ...fakeApi([]), list: (): Promise<TenantListResponse> => Promise.reject(new Error('boom')) }
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

describe('TenantsModel lifecycle', () => {
  it('create sends the spec, reloads the register, and returns the reveal-once result', async () => {
    const api = fakeApi([])
    const m = new TenantsModel(api)
    const res = await m.create({ slug: 'acme', display_name: 'Acme' })
    expect(res.owner_key).toBe('ctx_OWNER_PLAINTEXT_ONCE')
    expect(res.scope).toBe('acme:main')
    expect(api.log.find((c) => c.m === 'create')?.spec?.slug).toBe('acme')
    expect(api.calls).toBe(1) // reloaded after create
  })

  it('never retains the owner-key plaintext on the model', async () => {
    const api = fakeApi([])
    const m = new TenantsModel(api)
    await m.create({ slug: 'acme', display_name: 'Acme' })
    expect(JSON.stringify(m.tenants)).not.toContain('ctx_OWNER_PLAINTEXT_ONCE')
  })

  it('suspend patches status→suspended, reloads, clears busyId', async () => {
    const api = fakeApi([tenant({ slug: 'acme', id: 'id-acme' })])
    const m = new TenantsModel(api)
    await m.suspend('id-acme')
    expect(api.log.find((c) => c.m === 'update')).toMatchObject({ id: 'id-acme', status: 'suspended' })
    expect(api.calls).toBe(1) // reloaded
    expect(m.busyId).toBeNull()
  })

  it('activate patches status→active', async () => {
    const api = fakeApi([tenant({ slug: 'acme', id: 'id-acme' })])
    const m = new TenantsModel(api)
    await m.activate('id-acme')
    expect(api.log.find((c) => c.m === 'update')).toMatchObject({ id: 'id-acme', status: 'active' })
  })

  it('remove deletes + reloads', async () => {
    const api = fakeApi([tenant({ slug: 'acme', id: 'id-acme' })])
    const m = new TenantsModel(api)
    await m.remove('id-acme')
    expect(api.log.map((c) => c.m)).toContain('del')
    expect(api.calls).toBe(1)
    expect(m.busyId).toBeNull()
  })

  it('surfaces a default-tenant 400 on remove and rethrows', async () => {
    const api = fakeApi([tenant({ slug: 'default', id: 'id-default' })], undefined, {
      del: new ApiError(400, 'bad_request', 'cannot delete the default tenant'),
    })
    const m = new TenantsModel(api)
    await expect(m.remove('id-default')).rejects.toBeInstanceOf(ApiError)
    expect(m.actionError).toContain('default tenant')
    expect(m.busyId).toBeNull()
  })
})
