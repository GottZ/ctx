// PoolModel gates: load/error, the create/update/delete flows (each reloads
// the list so live status stays fresh), the priority swap (distinct prios) vs.
// tie-nudge vs. end no-op, and table-action error surfacing.

import { describe, expect, it } from 'vitest'
import { ApiError } from '../../../lib/api'
import type {
  BackendDeleteResponse,
  BackendListItem,
  BackendListResponse,
  BackendMutateResponse,
  BackendSpec,
  BackendTestResult,
} from '../../../lib/api/types'
import type { ConfirmFlags } from '../../../lib/api/backends'
import type { BackendDraft } from '../../../lib/backends'
import { PoolModel } from './pool.svelte'

function item(p: Partial<BackendListItem> & Pick<BackendListItem, 'name'>): BackendListItem {
  return {
    id: p.id ?? `id-${p.name}`,
    base_url: 'http://x',
    protocol: 'openai',
    provider_class: 'generic',
    api_key_ref: '',
    trust: 'public',
    locality: 'local',
    roles: [],
    model_map: {},
    timeouts: {},
    num_ctx: 0,
    priority: 0,
    enabled: true,
    extra_headers: {},
    extra_body: {},
    limits: {},
    metadata: {},
    effective_state: 'active',
    cooldown_remaining_s: 0,
    consecutive_fails: 0,
    ...p,
  }
}

interface Call {
  m: 'list' | 'create' | 'update' | 'del' | 'test'
  id?: string
  spec?: BackendSpec
  confirm?: ConfirmFlags
  probe?: boolean
}

function fakeApi(initial: BackendListItem[], fail?: Partial<Record<Call['m'], ApiError>>) {
  const calls: Call[] = []
  let current = initial
  return {
    calls,
    setList: (l: BackendListItem[]) => (current = l),
    list: (): Promise<BackendListResponse> => {
      calls.push({ m: 'list' })
      if (fail?.list) return Promise.reject(fail.list)
      return Promise.resolve({ success: true, backends: current })
    },
    create: (spec: BackendSpec, confirm: ConfirmFlags = {}): Promise<BackendMutateResponse> => {
      calls.push({ m: 'create', spec, confirm })
      if (fail?.create) return Promise.reject(fail.create)
      return Promise.resolve({ success: true, backend: item({ name: spec.name ?? 'x' }), warnings: ['w'] })
    },
    update: (id: string, spec: BackendSpec, confirm: ConfirmFlags = {}): Promise<BackendMutateResponse> => {
      calls.push({ m: 'update', id, spec, confirm })
      if (fail?.update) return Promise.reject(fail.update)
      return Promise.resolve({ success: true, backend: item({ name: 'x', id }) })
    },
    del: (id: string): Promise<BackendDeleteResponse> => {
      calls.push({ m: 'del', id })
      if (fail?.del) return Promise.reject(fail.del)
      return Promise.resolve({ success: true, deleted: id })
    },
    test: (id: string, probeChat = false): Promise<BackendTestResult> => {
      calls.push({ m: 'test', id, probe: probeChat })
      if (fail?.test) return Promise.reject(fail.test)
      return Promise.resolve({ success: true, reachable: true, latency_ms: 5, checks: {} })
    },
  }
}

const draft: BackendDraft = {
  base_url: 'http://h',
  protocol: 'openai',
  provider_class: 'generic',
  api_key_ref: '',
  locality: '',
  num_ctx: 4096,
  trust: 'public',
  roles: ['chat'],
  model_map: { chat: { model: 'q' } },
}

describe('PoolModel load', () => {
  it('populates and reaches ready', async () => {
    const api = fakeApi([item({ name: 'a' })])
    const m = new PoolModel(api)
    await m.load()
    expect(m.status).toBe('ready')
    expect(m.backends).toHaveLength(1)
  })

  it('surfaces a load error', async () => {
    const api = fakeApi([], { list: new ApiError(403, 'forbidden', 'admin key required') })
    const m = new PoolModel(api)
    await m.load()
    expect(m.status).toBe('error')
    expect(m.loadError?.status).toBe(403)
  })
})

describe('PoolModel mutations', () => {
  it('create sends the createSpec, reloads, returns warnings', async () => {
    const api = fakeApi([])
    const m = new PoolModel(api)
    const warnings = await m.create('herbert-chat', draft, { confirmTrustElevation: true })
    expect(warnings).toEqual(['w'])
    const create = api.calls.find((c) => c.m === 'create')
    expect(create?.spec?.name).toBe('herbert-chat')
    expect(create?.confirm).toEqual({ confirmTrustElevation: true })
    expect(api.calls.at(-1)?.m).toBe('list') // reloaded after
  })

  it('update reloads after patching', async () => {
    const api = fakeApi([item({ name: 'a', id: 'id-a' })])
    const m = new PoolModel(api)
    await m.update('id-a', { trust: 'no-credentials' })
    expect(api.calls.map((c) => c.m)).toEqual(['update', 'list'])
  })

  it('remove reloads on success', async () => {
    const api = fakeApi([item({ name: 'a', id: 'id-a' })])
    const m = new PoolModel(api)
    await m.remove('id-a')
    expect(api.calls.map((c) => c.m)).toEqual(['del', 'list'])
    expect(m.busyId).toBeNull()
  })

  it('remove surfaces a 409 and rethrows', async () => {
    const api = fakeApi([item({ name: 'a', id: 'id-a' })], {
      del: new ApiError(409, 'conflict', 'backend in use'),
    })
    const m = new PoolModel(api)
    await expect(m.remove('id-a')).rejects.toBeInstanceOf(ApiError)
    expect(m.actionError).toContain('in use')
    expect(m.busyId).toBeNull()
  })

  it('setEnabled patches the single field', async () => {
    const api = fakeApi([item({ name: 'a', id: 'id-a' })])
    const m = new PoolModel(api)
    await m.setEnabled('id-a', false)
    const upd = api.calls.find((c) => c.m === 'update')
    expect(upd?.spec).toEqual({ enabled: false })
  })
})

describe('PoolModel reprioritize', () => {
  it('swaps two distinct priorities (up)', async () => {
    const api = fakeApi([item({ name: 'a', id: 'a', priority: 10 }), item({ name: 'b', id: 'b', priority: 5 })])
    const m = new PoolModel(api)
    await m.load()
    await m.reprioritize('b', 'up')
    const updates = api.calls.filter((c) => c.m === 'update')
    expect(updates).toHaveLength(2)
    expect(updates[0]).toMatchObject({ id: 'b', spec: { priority: 10 } })
    expect(updates[1]).toMatchObject({ id: 'a', spec: { priority: 5 } })
  })

  it('nudges past a tie instead of a no-op swap', async () => {
    const api = fakeApi([item({ name: 'a', id: 'a', priority: 5 }), item({ name: 'b', id: 'b', priority: 5 })])
    const m = new PoolModel(api)
    await m.load()
    await m.reprioritize('b', 'up') // b is below a (name tiebreak), tie on prio
    const updates = api.calls.filter((c) => c.m === 'update')
    expect(updates).toHaveLength(1)
    expect(updates[0]).toMatchObject({ id: 'b', spec: { priority: 6 } })
  })

  it('is a no-op at the top end', async () => {
    const api = fakeApi([item({ name: 'a', id: 'a', priority: 10 }), item({ name: 'b', id: 'b', priority: 5 })])
    const m = new PoolModel(api)
    await m.load()
    await m.reprioritize('a', 'up')
    expect(api.calls.filter((c) => c.m === 'update')).toHaveLength(0)
  })
})
