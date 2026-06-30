// KeysModel gates: load/ready, the active_only-by-default list call, load
// error surfacing, create (sends spec, reloads, returns the show-once result,
// never retains the plaintext), revoke (reload + busyId clear vs. 404-no-oracle
// surfacing + rethrow), and the show-revoked toggle flipping active_only.

import { describe, expect, it } from 'vitest'
import { ApiError } from '../../lib/api'
import type { ApiKeyCreateSpec } from '../../lib/api/keys'
import type {
  ApiKeyCreateResult,
  ApiKeyListResponse,
  ApiKeyUpdateResult,
  ApiKeyUpdateSpec,
  ApiKeyView,
} from '../../lib/api/types'
import { KeysModel } from './keys.svelte'

function key(p: Partial<ApiKeyView> & Pick<ApiKeyView, 'label'>): ApiKeyView {
  return {
    id: `id-${p.label}`,
    home_scope: 'private',
    allowed_scopes: ['private'],
    active: true,
    created_at: '2026-06-29T00:00:00Z',
    ...p,
  }
}

interface Call {
  m: 'list' | 'create' | 'del' | 'update'
  id?: string
  activeOnly?: boolean
  spec?: ApiKeyCreateSpec
  update?: ApiKeyUpdateSpec
}

function fakeApi(initial: ApiKeyView[], fail?: Partial<Record<Call['m'], ApiError>>) {
  const calls: Call[] = []
  let current = initial
  return {
    calls,
    setList: (l: ApiKeyView[]) => (current = l),
    list: (activeOnly?: boolean): Promise<ApiKeyListResponse> => {
      calls.push({ m: 'list', activeOnly })
      if (fail?.list) return Promise.reject(fail.list)
      return Promise.resolve({ success: true, keys: current })
    },
    create: (spec: ApiKeyCreateSpec): Promise<ApiKeyCreateResult> => {
      calls.push({ m: 'create', spec })
      if (fail?.create) return Promise.reject(fail.create)
      return Promise.resolve({
        success: true,
        id: 'new-id',
        label: spec.label,
        home_scope: spec.home_scope,
        allowed_scopes: spec.allowed_scopes ?? [],
        api_key: 'ctx_PLAINTEXT_ONCE',
      })
    },
    del: (id: string): Promise<{ success: true; deleted: string }> => {
      calls.push({ m: 'del', id })
      if (fail?.del) return Promise.reject(fail.del)
      return Promise.resolve({ success: true, deleted: id })
    },
    update: (spec: ApiKeyUpdateSpec): Promise<ApiKeyUpdateResult> => {
      calls.push({ m: 'update', update: spec })
      if (fail?.update) return Promise.reject(fail.update)
      return Promise.resolve({
        success: true,
        key: key({ label: 'updated', id: spec.id, tenant_role: spec.tenant_role, active: spec.active ?? true }),
      })
    },
  }
}

describe('KeysModel load', () => {
  it('populates and reaches ready', async () => {
    const api = fakeApi([key({ label: 'ci' })])
    const m = new KeysModel(api)
    await m.load()
    expect(m.status).toBe('ready')
    expect(m.keys).toHaveLength(1)
  })

  it('lists active-only by default (showRevoked false → active_only true)', async () => {
    const api = fakeApi([])
    const m = new KeysModel(api)
    await m.load()
    expect(api.calls.at(-1)).toMatchObject({ m: 'list', activeOnly: true })
  })

  it('surfaces a load error', async () => {
    const api = fakeApi([], { list: new ApiError(403, 'forbidden', 'tenant-admin required') })
    const m = new KeysModel(api)
    await m.load()
    expect(m.status).toBe('error')
    expect(m.loadError?.status).toBe(403)
  })
})

describe('KeysModel mutations', () => {
  it('create sends the spec, reloads, returns the show-once result', async () => {
    const api = fakeApi([])
    const m = new KeysModel(api)
    const res = await m.create({ label: 'deploy', home_scope: 'private' })
    expect(res.api_key).toBe('ctx_PLAINTEXT_ONCE')
    const create = api.calls.find((c) => c.m === 'create')
    expect(create?.spec?.label).toBe('deploy')
    expect(api.calls.at(-1)?.m).toBe('list') // reloaded after
  })

  it('never retains the plaintext key on the model', async () => {
    const api = fakeApi([])
    const m = new KeysModel(api)
    await m.create({ label: 'deploy', home_scope: 'private' })
    expect(JSON.stringify(m.keys)).not.toContain('ctx_PLAINTEXT_ONCE')
  })

  it('remove reloads on success and clears busyId', async () => {
    const api = fakeApi([key({ label: 'a', id: 'id-a' })])
    const m = new KeysModel(api)
    await m.remove('id-a')
    expect(api.calls.map((c) => c.m)).toEqual(['del', 'list'])
    expect(m.busyId).toBeNull()
  })

  it('remove surfaces a 404-no-oracle error and rethrows', async () => {
    const api = fakeApi([key({ label: 'a', id: 'id-a' })], {
      del: new ApiError(200, 'not_found', 'key not found'),
    })
    const m = new KeysModel(api)
    await expect(m.remove('id-a')).rejects.toBeInstanceOf(ApiError)
    expect(m.actionError).toContain('not found')
    expect(m.busyId).toBeNull()
  })
})

describe('KeysModel update (role/active delegation)', () => {
  it('sends the spec, reloads, and clears busyId', async () => {
    const api = fakeApi([key({ label: 'a', id: 'id-a' })])
    const m = new KeysModel(api)
    await m.update({ id: 'id-a', tenant_role: 'admin' })
    const upd = api.calls.find((c) => c.m === 'update')
    expect(upd?.update).toMatchObject({ id: 'id-a', tenant_role: 'admin' })
    expect(api.calls.at(-1)?.m).toBe('list') // reloaded after
    expect(m.busyId).toBeNull()
  })

  it('guards the in-flight row via busyId until the call settles', async () => {
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    const api = {
      ...fakeApi([key({ label: 'a', id: 'id-a' })]),
      update: (spec: ApiKeyUpdateSpec): Promise<ApiKeyUpdateResult> =>
        gate.then(() => ({ success: true, key: key({ label: 'a', id: spec.id }) })),
    }
    const m = new KeysModel(api)
    const p = m.update({ id: 'id-a', active: false })
    expect(m.busyId).toBe('id-a') // set while in flight
    release()
    await p
    expect(m.busyId).toBeNull()
  })

  it('surfaces a last-owner/self-lockout error and rethrows', async () => {
    const api = fakeApi([key({ label: 'a', id: 'id-a', tenant_role: 'owner' })], {
      update: new ApiError(409, 'conflict', 'last owner cannot be demoted'),
    })
    const m = new KeysModel(api)
    await expect(m.update({ id: 'id-a', tenant_role: 'member' })).rejects.toBeInstanceOf(ApiError)
    expect(m.actionError).toContain('last owner')
    expect(m.busyId).toBeNull()
  })
})

describe('KeysModel show-revoked toggle', () => {
  it('flips showRevoked and reloads with the new active_only', async () => {
    const api = fakeApi([])
    const m = new KeysModel(api)
    await m.load()
    expect(api.calls.at(-1)).toMatchObject({ m: 'list', activeOnly: true })

    await m.toggleRevoked()
    expect(m.showRevoked).toBe(true)
    expect(api.calls.at(-1)).toMatchObject({ m: 'list', activeOnly: false })

    await m.toggleRevoked()
    expect(m.showRevoked).toBe(false)
    expect(api.calls.at(-1)).toMatchObject({ m: 'list', activeOnly: true })
  })
})
