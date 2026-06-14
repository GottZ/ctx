// BlockDetailModel gates (block-workbench W3, read-only): load(id) walks
// 'loading' -> 'ready', fills `block` from the scope-gated getBlock and records
// `openId`; the api is called with the picked id. A not-found / not-visible
// block (apiFetch rejects the 200 {success:false} envelope as ApiError) lands
// in status 'error' + loadError WITHOUT throwing — the panel must render the
// failure, never crash. close() drops the block (block=null, openId=null) and
// returns status to a non-content state. fakeApi tracks calls (pool.svelte.test
// pattern). No W4-editor / W5-delete / W6-sensitivity cases — read-only.

import { describe, expect, it } from 'vitest'
import { ApiError } from '../../lib/api'
import type { BlockDetail } from '../../lib/api/blocks'
import type { DetailApi } from './detail.svelte'
import { BlockDetailModel } from './detail.svelte'

function block(id: string, p: Partial<BlockDetail> = {}): BlockDetail {
  return {
    id,
    category: 'learnings',
    tags: ['graph', 'go'],
    title: `title-${id}`,
    content: 'full block content body',
    metadata: { source: 'claude-code-2026-06-14' },
    scope: 'private',
    created_at: '2026-06-01T00:00:00Z',
    updated_at: '2026-06-14T00:00:00Z',
    ...p,
  }
}

interface Call {
  m: 'get'
  id: string
}

function fakeApi(detail: BlockDetail, fail?: ApiError): DetailApi & { calls: Call[] } {
  const calls: Call[] = []
  return {
    calls,
    get: (id: string): Promise<{ success: true; block: BlockDetail }> => {
      calls.push({ m: 'get', id })
      if (fail) return Promise.reject(fail)
      return Promise.resolve({ success: true, block: { ...detail, id } })
    },
  }
}

describe('BlockDetailModel load', () => {
  it('starts idle with no block open', () => {
    const m = new BlockDetailModel(fakeApi(block('b1')))
    expect(m.status).toBe('idle')
    expect(m.block).toBeNull()
    expect(m.openId).toBeNull()
  })

  it('lazy-loads the picked block and reaches ready', async () => {
    const api = fakeApi(block('b1', { title: 'graph traversal', content: 'body-1' }))
    const m = new BlockDetailModel(api)
    await m.load('b1')
    expect(m.status).toBe('ready')
    expect(m.block?.id).toBe('b1')
    expect(m.block?.title).toBe('graph traversal')
    expect(m.block?.content).toBe('body-1')
    expect(m.loadError).toBeNull()
  })

  it('calls getBlock with the picked id and records openId', async () => {
    const api = fakeApi(block('b7'))
    const m = new BlockDetailModel(api)
    await m.load('b7')
    const get = api.calls.find((c) => c.m === 'get')
    expect(get?.id).toBe('b7')
    // openId is the id the panel keeps open (drives the ?focus= graph link).
    expect(m.openId).toBe('b7')
  })

  it('passes through loading before ready', async () => {
    const api = fakeApi(block('b1'))
    const m = new BlockDetailModel(api)
    const pending = m.load('b1')
    expect(m.status).toBe('loading')
    await pending
    expect(m.status).toBe('ready')
  })

  it('surfaces a not-found / not-visible block as error without crashing', async () => {
    // handleGet returns HTTP 200 {success:false,"Block not found"} for a block
    // the key cannot see; apiFetch rejects that envelope as ApiError (status is
    // the 2xx it arrived in). The model must catch it — never throw.
    const api = fakeApi(block('gone'), new ApiError(200, 'api_error', 'Block not found'))
    const m = new BlockDetailModel(api)
    await expect(m.load('gone')).resolves.toBeUndefined()
    expect(m.status).toBe('error')
    expect(m.loadError?.message).toBe('Block not found')
    expect(m.block).toBeNull()
  })

  it('surfaces a 403 forbidden load as error', async () => {
    const api = fakeApi(block('x'), new ApiError(403, 'forbidden', 'no access'))
    const m = new BlockDetailModel(api)
    await m.load('x')
    expect(m.status).toBe('error')
    expect(m.loadError?.status).toBe(403)
  })
})

describe('BlockDetailModel close', () => {
  it('drops the loaded block and clears openId', async () => {
    const api = fakeApi(block('b1'))
    const m = new BlockDetailModel(api)
    await m.load('b1')
    expect(m.block).not.toBeNull()
    m.close()
    expect(m.block).toBeNull()
    expect(m.openId).toBeNull()
    // Back to a non-content state — the panel stops rendering content.
    expect(m.status).not.toBe('ready')
  })

  it('clears a prior load error on close', async () => {
    const api = fakeApi(block('x'), new ApiError(403, 'forbidden', 'no access'))
    const m = new BlockDetailModel(api)
    await m.load('x')
    expect(m.loadError).not.toBeNull()
    m.close()
    expect(m.loadError).toBeNull()
  })
})
