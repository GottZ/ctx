// BlockDeleteModel gates (block-workbench W5): remove(id) calls deleteBlock with
// the FULL uuid; on success it closes the detail panel AND reloads the list (the
// deleted block must disappear); on failure it surfaces `error`, leaves the
// panel OPEN and does NOT reload. The real failure shapes are the 200
// {success:false} not-found envelope and the 409 ambiguous-prefix — block delete
// has NO referenced_by/FK-conflict path (that is the SECRET delete), so there is
// deliberately no referenced_by case here. busy brackets the round-trip. fakeApi
// + reload/close spies, call-tracking (pool.svelte.test / edit.svelte.test
// pattern). The native <dialog> confirm gate lives in BlockDetail.svelte (DOM,
// not unit-tested here) — the model is invoked only AFTER the user confirms.

import { describe, expect, it } from 'vitest'
import { ApiError } from '../../lib/api'
import type { BlockDeleteApi, DeleteBlockResponse } from '../../lib/api/blocks'
import { BlockDeleteModel } from './delete.svelte'

interface Call {
  m: 'del'
  id: string
}

const okDeleted: DeleteBlockResponse = {
  success: true,
  deleted: {
    id: 'b1',
    category: 'learnings',
    title: 'a block',
    scope: 'private',
    created_at: 't',
    updated_at: 't',
  },
}

function fakeApi(fail?: ApiError): BlockDeleteApi & { calls: Call[] } {
  const calls: Call[] = []
  return {
    calls,
    del: (id: string): Promise<DeleteBlockResponse> => {
      calls.push({ m: 'del', id })
      if (fail) return Promise.reject(fail)
      return Promise.resolve({ ...okDeleted, deleted: { ...okDeleted.deleted, id } })
    },
  }
}

function reloadSpy() {
  const reloads: number[] = []
  const fn = async (): Promise<void> => {
    reloads.push(reloads.length)
  }
  return { fn, reloads }
}

function closeSpy() {
  const closes: number[] = []
  const fn = (): void => {
    closes.push(closes.length)
  }
  return { fn, closes }
}

describe('BlockDeleteModel remove (success)', () => {
  it('calls deleteBlock with the full uuid, then closes the panel and reloads', async () => {
    const api = fakeApi()
    const r = reloadSpy()
    const c = closeSpy()
    const m = new BlockDeleteModel(api, r.fn, c.fn)
    const ok = await m.remove('full-uuid-1')
    expect(ok).toBe(true)
    const del = api.calls.find((cc) => cc.m === 'del')
    expect(del?.id).toBe('full-uuid-1')
    // On success: the panel closes AND the list reloads (the block disappears).
    expect(c.closes).toHaveLength(1)
    expect(r.reloads).toHaveLength(1)
    expect(m.error).toBeNull()
    expect(m.busy).toBe(false)
    expect(m.status).toBe('ready')
  })
})

describe('BlockDeleteModel remove (failure)', () => {
  it('surfaces a not-found {success:false} (200) envelope, no reload, panel stays open', async () => {
    // handleDelete answers HTTP 200 {success:false,"Block not found"} for a
    // block the key cannot delete; apiFetch rejects that envelope as ApiError.
    const api = fakeApi(new ApiError(200, 'api_error', 'Block not found'))
    const r = reloadSpy()
    const c = closeSpy()
    const m = new BlockDeleteModel(api, r.fn, c.fn)
    const ok = await m.remove('gone')
    expect(ok).toBe(false)
    expect(m.error).toBe('Block not found')
    // No reload and the panel does NOT close — the user keeps the failed block open.
    expect(r.reloads).toHaveLength(0)
    expect(c.closes).toHaveLength(0)
    expect(m.busy).toBe(false)
    expect(m.status).toBe('error')
  })

  it('surfaces a 409 ambiguous-prefix as error, no reload, panel stays open', async () => {
    // The only 409 a block delete can return is the ambiguous-id-prefix conflict
    // (context_manage.go handleDelete) — NOT a referenced_by conflict. A full
    // UUID never triggers it; this proves the model surfaces the conflict shape.
    const api = fakeApi(new ApiError(409, 'conflict', 'Ambiguous id prefix — re-specify with a longer prefix or full id'))
    const r = reloadSpy()
    const c = closeSpy()
    const m = new BlockDeleteModel(api, r.fn, c.fn)
    const ok = await m.remove('b')
    expect(ok).toBe(false)
    expect(m.error).toContain('Ambiguous id prefix')
    expect(r.reloads).toHaveLength(0)
    expect(c.closes).toHaveLength(0)
    expect(m.busy).toBe(false)
  })
})

describe('BlockDeleteModel busy bracketing', () => {
  it('sets busy=true while the delete is in flight and clears it on settle', async () => {
    const api = fakeApi()
    const r = reloadSpy()
    const c = closeSpy()
    const m = new BlockDeleteModel(api, r.fn, c.fn)
    const pending = m.remove('uuid')
    expect(m.busy).toBe(true)
    await pending
    expect(m.busy).toBe(false)
  })
})
