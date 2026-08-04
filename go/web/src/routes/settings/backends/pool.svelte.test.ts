// PoolModel gates: load/error, the create/update/delete flows (each reloads
// the list so live status stays fresh), the atomic reorder (optimistic apply,
// rollback on failure, 409-stale reload), the ▲▼ fallback riding on reorder,
// the single mutation guard (a second table action during flight is a no-op),
// and table-action error surfacing incl. the reload after a failed patch.

import { describe, expect, it } from 'vitest'
import { ApiError } from '../../../lib/api'
import type {
  BackendDeleteResponse,
  BackendListItem,
  BackendListResponse,
  BackendMutateResponse,
  BackendReorderResponse,
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
    scope: '_global',
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
  m: 'list' | 'create' | 'update' | 'del' | 'test' | 'reorder'
  id?: string
  spec?: BackendSpec
  confirm?: ConfirmFlags
  probe?: boolean
  order?: string[]
  expected?: Record<string, number>
}

/**
 * fail rejects EVERY call of that method; failOn decides per call (receives
 * the recorded call + its index) so e.g. a reorder can fail while the
 * reconciling list call still succeeds on a later invocation.
 */
function fakeApi(
  initial: BackendListItem[],
  fail?: Partial<Record<Call['m'], ApiError>>,
  failOn?: (call: Call, index: number) => ApiError | undefined,
) {
  const calls: Call[] = []
  let current = initial
  const gate = (call: Call): ApiError | undefined => {
    calls.push(call)
    return fail?.[call.m] ?? failOn?.(call, calls.length - 1)
  }
  return {
    calls,
    setList: (l: BackendListItem[]) => (current = l),
    list: (): Promise<BackendListResponse> => {
      const err = gate({ m: 'list' })
      if (err) return Promise.reject(err)
      return Promise.resolve({ success: true, backends: current })
    },
    create: (spec: BackendSpec, confirm: ConfirmFlags = {}): Promise<BackendMutateResponse> => {
      const err = gate({ m: 'create', spec, confirm })
      if (err) return Promise.reject(err)
      return Promise.resolve({ success: true, backend: item({ name: spec.name ?? 'x' }), warnings: ['w'] })
    },
    update: (id: string, spec: BackendSpec, confirm: ConfirmFlags = {}): Promise<BackendMutateResponse> => {
      const err = gate({ m: 'update', id, spec, confirm })
      if (err) return Promise.reject(err)
      return Promise.resolve({ success: true, backend: item({ name: 'x', id }) })
    },
    del: (id: string): Promise<BackendDeleteResponse> => {
      const err = gate({ m: 'del', id })
      if (err) return Promise.reject(err)
      return Promise.resolve({ success: true, deleted: id })
    },
    test: (id: string, probeChat = false): Promise<BackendTestResult> => {
      const err = gate({ m: 'test', id, probe: probeChat })
      if (err) return Promise.reject(err)
      return Promise.resolve({ success: true, reachable: true, latency_ms: 5, checks: {} })
    },
    reorder: (order: string[], expected: Record<string, number>): Promise<BackendReorderResponse> => {
      const err = gate({ m: 'reorder', order, expected })
      if (err) return Promise.reject(err)
      return Promise.resolve({ success: true })
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
  disable_profiles: [],
}

const three = (): BackendListItem[] => [
  item({ name: 'a', id: 'a', priority: 100 }),
  item({ name: 'b', id: 'b', priority: 50 }),
  item({ name: 'c', id: 'c', priority: 10 }),
]

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
    expect(m.mutating).toBe(false)
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

  it('setPriority patches the single field', async () => {
    const api = fakeApi([item({ name: 'a', id: 'id-a' })])
    const m = new PoolModel(api)
    await m.setPriority('id-a', 75)
    const upd = api.calls.find((c) => c.m === 'update')
    expect(upd?.spec).toEqual({ priority: 75 })
  })

  it('reloads after a failed table patch so the control snaps back', async () => {
    const api = fakeApi(three(), { update: new ApiError(422, 'validation', 'bad enabled') })
    const m = new PoolModel(api)
    await m.load()
    await m.setEnabled('a', false)
    expect(m.actionError).toContain('bad enabled')
    // the reload after the failure is what reverts an optimistic checkbox
    expect(api.calls.map((c) => c.m)).toEqual(['list', 'update', 'list'])
    expect(m.mutating).toBe(false)
  })
})

describe('PoolModel reorder', () => {
  it('applies the order optimistically, then reconciles via list', async () => {
    const api = fakeApi(three())
    const m = new PoolModel(api)
    await m.load()
    const p = m.reorder(['c', 'a', 'b'])
    // before the server answers: target order, local descending 10-steps
    expect(m.sorted.map((b) => b.id)).toEqual(['c', 'a', 'b'])
    expect(m.sorted.map((b) => b.priority)).toEqual([30, 20, 10])
    expect(m.mutating).toBe(true)
    await p
    const ro = api.calls.find((c) => c.m === 'reorder')
    expect(ro?.order).toEqual(['c', 'a', 'b'])
    expect(ro?.expected).toEqual({ a: 100, b: 50, c: 10 })
    expect(api.calls.at(-1)?.m).toBe('list') // reconcile
    expect(m.mutating).toBe(false)
    expect(m.busyId).toBeNull()
  })

  it('rolls back to the snapshot on failure', async () => {
    const api = fakeApi(three(), { reorder: new ApiError(500, 'internal', 'boom') })
    const m = new PoolModel(api)
    await m.load()
    await m.reorder(['c', 'a', 'b'])
    expect(m.sorted.map((b) => b.id)).toEqual(['a', 'b', 'c'])
    expect(m.sorted.map((b) => b.priority)).toEqual([100, 50, 10])
    expect(m.actionError).toContain('boom')
    // no reload on a generic failure — the snapshot IS the last-known truth
    expect(api.calls.filter((c) => c.m === 'list')).toHaveLength(1)
  })

  it('reloads and hints on a 409 stale order', async () => {
    const api = fakeApi(three(), { reorder: new ApiError(409, 'conflict', 'priorities changed') })
    const m = new PoolModel(api)
    await m.load()
    await m.reorder(['c', 'a', 'b'])
    expect(m.actionError).toContain('changed elsewhere')
    expect(api.calls.map((c) => c.m)).toEqual(['list', 'reorder', 'list'])
  })

  it('rejects an id set that does not match the loaded list', async () => {
    const api = fakeApi(three())
    const m = new PoolModel(api)
    await m.load()
    await m.reorder(['c', 'a']) // stale view: one id short
    expect(api.calls.filter((c) => c.m === 'reorder')).toHaveLength(0)
  })

  it('sends only the WRITABLE subsequence — a tenant view never wires _global rows (T37)', async () => {
    // b is a visible-but-read-only _global row in a tenant view; the wire
    // order and the expected snapshot must both exclude it, and its local
    // priority must survive the optimistic apply untouched.
    const api = fakeApi([
      item({ name: 'a', id: 'a', priority: 100, scope: 'tenant:x' }),
      item({ name: 'b', id: 'b', priority: 50, scope: '_global' }),
      item({ name: 'c', id: 'c', priority: 10, scope: 'tenant:x' }),
    ])
    const m = new PoolModel(api)
    m.isWritable = (b) => b.scope === 'tenant:x'
    await m.load()
    const p = m.reorder(['c', 'b', 'a']) // full visible order, as the table hands it over
    expect(m.backends.find((b) => b.id === 'b')?.priority).toBe(50) // untouched optimistically
    await p
    const ro = api.calls.find((c) => c.m === 'reorder')
    expect(ro?.order).toEqual(['c', 'a'])
    expect(ro?.expected).toEqual({ a: 100, c: 10 })
  })

  it('blocks a second table mutation while one is in flight', async () => {
    const api = fakeApi(three())
    const m = new PoolModel(api)
    await m.load()
    let release!: () => void
    const origUpdate = api.update
    api.update = (id, spec, confirm) =>
      new Promise((resolve) => {
        release = () => resolve(origUpdate(id, spec, confirm))
      })
    const first = m.setEnabled('a', false)
    await m.reorder(['c', 'a', 'b']) // guard: silent no-op
    await m.setPriority('b', 1) // guard: silent no-op
    expect(api.calls.filter((c) => c.m === 'reorder')).toHaveLength(0)
    // the initiator marker survives — no overlapping finally nulled it
    expect(m.busyId).toBe('a')
    expect(m.mutating).toBe(true)
    release()
    await first
    // exactly the first mutation reached the api (recorded on release)
    expect(api.calls.filter((c) => c.m === 'update')).toHaveLength(1)
    expect(m.busyId).toBeNull()
    expect(m.mutating).toBe(false)
  })
})

describe('PoolModel reprioritize', () => {
  it('moves a row up via ONE atomic full-order reorder', async () => {
    const api = fakeApi([item({ name: 'a', id: 'a', priority: 10 }), item({ name: 'b', id: 'b', priority: 5 })])
    const m = new PoolModel(api)
    await m.load()
    await m.reprioritize('b', 'up')
    const ro = api.calls.find((c) => c.m === 'reorder')
    expect(ro?.order).toEqual(['b', 'a'])
    expect(ro?.expected).toEqual({ a: 10, b: 5 })
    expect(api.calls.filter((c) => c.m === 'update')).toHaveLength(0)
  })

  it('handles a priority tie without a special case', async () => {
    const api = fakeApi([item({ name: 'a', id: 'a', priority: 5 }), item({ name: 'b', id: 'b', priority: 5 })])
    const m = new PoolModel(api)
    await m.load()
    await m.reprioritize('b', 'up') // b is below a (name tiebreak), tie on prio
    const ro = api.calls.find((c) => c.m === 'reorder')
    expect(ro?.order).toEqual(['b', 'a'])
  })

  it('swaps within the writable subsequence, skipping read-only neighbors (T37)', async () => {
    const api = fakeApi([
      item({ name: 'a', id: 'a', priority: 100, scope: 'tenant:x' }),
      item({ name: 'b', id: 'b', priority: 50, scope: '_global' }),
      item({ name: 'c', id: 'c', priority: 10, scope: 'tenant:x' }),
    ])
    const m = new PoolModel(api)
    m.isWritable = (b) => b.scope === 'tenant:x'
    await m.load()
    await m.reprioritize('c', 'up') // writable neighbor is a, NOT the adjacent read-only b
    const ro = api.calls.find((c) => c.m === 'reorder')
    expect(ro?.order).toEqual(['c', 'a'])
  })

  it('is a no-op at the top end', async () => {
    const api = fakeApi([item({ name: 'a', id: 'a', priority: 10 }), item({ name: 'b', id: 'b', priority: 5 })])
    const m = new PoolModel(api)
    await m.load()
    await m.reprioritize('a', 'up')
    expect(api.calls.filter((c) => c.m === 'reorder')).toHaveLength(0)
    expect(api.calls.filter((c) => c.m === 'update')).toHaveLength(0)
  })
})
