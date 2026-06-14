// BlockEditModel gates (block-workbench W4): the create flow (storeBlock then
// reload), the edit flow (updateBlock with the blockDiff, only changed fields),
// the sensitivity-DOWNGRADE confirm flow (a 400 without confirm flips
// needsConfirm and does NOT re-send; a second save() carries
// confirm_sensitivity_downgrade:true and succeeds) and 422 field-error
// surfacing. fakeApi + reload-callback call-tracking (pool.svelte.test pattern).

import { describe, expect, it } from 'vitest'
import { ApiError } from '../../lib/api'
import type {
  BlockWriteApi,
  StoreBlockRequest,
  StoreBlockResponse,
  UpdateBlockData,
  UpdateBlockResponse,
} from '../../lib/api/blocks'
import type { BlockDraft } from '../../lib/blocks/edit'
import { BlockEditModel } from './edit.svelte'

function draft(p: Partial<BlockDraft> = {}): BlockDraft {
  return {
    category: 'learnings',
    title: 'a block',
    content: 'body',
    tags: ['go'],
    scope: 'private',
    sensitivity: 'internal',
    ...p,
  }
}

interface Call {
  m: 'store' | 'update'
  id?: string
  req?: StoreBlockRequest
  data?: UpdateBlockData
}

const okBlock = {
  id: 'b1',
  category: 'learnings',
  tags: [] as string[],
  title: 'a block',
  content: 'body',
  metadata: null,
  scope: 'private',
  created_at: 't',
  updated_at: 't',
}

/**
 * fakeApi with optional staged failures. `failUpdateOnce` rejects the FIRST
 * update with the given error then succeeds (the downgrade 400 → confirm →
 * retry path).
 */
function fakeApi(opts: { fail?: Partial<Record<Call['m'], ApiError>>; failUpdateOnce?: ApiError } = {}) {
  const calls: Call[] = []
  let updateCount = 0
  const api: BlockWriteApi & { calls: Call[] } = {
    calls,
    store: (req: StoreBlockRequest): Promise<StoreBlockResponse> => {
      calls.push({ m: 'store', req })
      if (opts.fail?.store) return Promise.reject(opts.fail.store)
      return Promise.resolve({ success: true, block: okBlock })
    },
    update: (id: string, data: UpdateBlockData): Promise<UpdateBlockResponse> => {
      calls.push({ m: 'update', id, data })
      updateCount += 1
      if (opts.failUpdateOnce && updateCount === 1) return Promise.reject(opts.failUpdateOnce)
      if (opts.fail?.update) return Promise.reject(opts.fail.update)
      return Promise.resolve({ success: true, block: okBlock })
    },
  }
  return api
}

function reloadSpy() {
  const reloads: number[] = []
  const fn = async (): Promise<void> => {
    reloads.push(reloads.length)
  }
  return { fn, reloads }
}

const downgrade400 = new ApiError(
  400,
  'bad_request',
  'sensitivity downgrade credentials → internal opens this block to lower-trust backends — repeat with "confirm_sensitivity_downgrade": true',
)

describe('BlockEditModel create', () => {
  it('calls storeBlock with the create request, then reloads', async () => {
    const api = fakeApi()
    const r = reloadSpy()
    const m = new BlockEditModel(api, r.fn)
    const ok = await m.save({ mode: 'create', draft: draft({ tags: ['go'] }) })
    expect(ok).toBe(true)
    const store = api.calls.find((c) => c.m === 'store')
    expect(store?.req).toMatchObject({ category: 'learnings', title: 'a block', content: 'body', tags: ['go'] })
    expect(api.calls.some((c) => c.m === 'update')).toBe(false)
    expect(r.reloads).toHaveLength(1)
    expect(m.status).toBe('ready')
  })

  it('surfaces a 422 from create as fieldErrors, no reload', async () => {
    const api = fakeApi({
      fail: {
        store: new ApiError(422, 'validation', 'validation failed', null, {
          success: false,
          error: 'validation failed',
          fields: [{ field: 'title', message: 'title is required' }],
        }),
      },
    })
    const r = reloadSpy()
    const m = new BlockEditModel(api, r.fn)
    const ok = await m.save({ mode: 'create', draft: draft({ title: '' }) })
    expect(ok).toBe(false)
    expect(m.fieldErrors).toEqual([{ field: 'title', message: 'title is required' }])
    expect(r.reloads).toHaveLength(0)
  })
})

describe('BlockEditModel edit', () => {
  it('sends only the changed fields (blockDiff) and reloads', async () => {
    const api = fakeApi()
    const r = reloadSpy()
    const m = new BlockEditModel(api, r.fn)
    const original = draft({ title: 'old', content: 'c' })
    const next = { ...original, title: 'new' }
    const ok = await m.save({ mode: 'edit', id: 'full-uuid-1', draft: next, original })
    expect(ok).toBe(true)
    const upd = api.calls.find((c) => c.m === 'update')
    expect(upd?.id).toBe('full-uuid-1')
    expect(upd?.data).toEqual({ title: 'new' })
    expect(r.reloads).toHaveLength(1)
  })

  it('an unchanged edit skips the update round-trip (empty diff)', async () => {
    const api = fakeApi()
    const r = reloadSpy()
    const m = new BlockEditModel(api, r.fn)
    const original = draft()
    const ok = await m.save({ mode: 'edit', id: 'uuid', draft: { ...original }, original })
    expect(ok).toBe(true)
    // No field changed → blockDiff is empty → no update call (the server has no
    // meaningful empty patch), no reload.
    expect(api.calls.some((c) => c.m === 'update')).toBe(false)
    expect(r.reloads).toHaveLength(0)
    expect(m.status).toBe('ready')
  })
})

describe('BlockEditModel sensitivity downgrade confirm flow', () => {
  it('a downgrade 400 without confirm flips needsConfirm and does NOT re-send', async () => {
    const api = fakeApi({ failUpdateOnce: downgrade400 })
    const r = reloadSpy()
    const m = new BlockEditModel(api, r.fn)
    const original = draft({ sensitivity: 'credentials' })
    const next = { ...original, sensitivity: 'internal' }
    const ok = await m.save({ mode: 'edit', id: 'uuid', draft: next, original })
    expect(ok).toBe(false)
    expect(m.needsConfirm).toBe(true)
    // exactly one update attempt — the model must NOT auto-retry without confirm.
    expect(api.calls.filter((c) => c.m === 'update')).toHaveLength(1)
    // and the first attempt did NOT carry the confirm flag.
    const first = api.calls.find((c) => c.m === 'update')
    expect(first?.data?.confirm_sensitivity_downgrade).toBeUndefined()
    expect(r.reloads).toHaveLength(0)
  })

  it('a second save() after confirm carries confirm_sensitivity_downgrade and succeeds', async () => {
    const api = fakeApi({ failUpdateOnce: downgrade400 })
    const r = reloadSpy()
    const m = new BlockEditModel(api, r.fn)
    const original = draft({ sensitivity: 'credentials' })
    const next = { ...original, sensitivity: 'internal' }
    await m.save({ mode: 'edit', id: 'uuid', draft: next, original })
    expect(m.needsConfirm).toBe(true)
    const ok = await m.save({ mode: 'edit', id: 'uuid', draft: next, original })
    expect(ok).toBe(true)
    const updates = api.calls.filter((c) => c.m === 'update')
    expect(updates).toHaveLength(2)
    expect(updates[1]?.data?.confirm_sensitivity_downgrade).toBe(true)
    expect(updates[1]?.data?.sensitivity).toBe('internal')
    expect(r.reloads).toHaveLength(1)
  })
})
