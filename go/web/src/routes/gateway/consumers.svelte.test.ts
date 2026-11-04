// ConsumersModel gates (design/05 §4.6 store table, §4.7 polling cadence,
// wave W05-05):
//   G4 — the 30 s poll runs ONLY while the tab is visible: 90 s fake time in a
//        hidden tab ⇒ 0 list calls after the initial load; visible ⇒ 3 polls;
//        dispose() ⇒ no further call and the visibility listener is gone.
//   G5 — keyset: loadMore() hands next_cursor through unchanged, next_cursor
//        null ⇒ no further call, a filter change drops the cursor (page 1 again).
// Plus the wire projection of the filter (status set / all / q / tenant_id),
// stale-response dropping and the poll walk over the loaded pages.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { readdirSync, readFileSync } from 'node:fs'
import { ApiError } from '../../lib/api'
import type { ConsumerListParams } from '../../lib/api/modelapi'
import type { ConsumerListResponse, ConsumerView, TenantListResponse } from '../../lib/api/types'
import { CONSUMER_POLL_MS, ConsumersModel, type VisibilitySource } from './consumers.svelte'

function consumer(label: string, p: Partial<ConsumerView> = {}): ConsumerView {
  return {
    id: `id-${label}`,
    tenant_id: 't-a',
    tenant_slug: 'acme',
    label,
    description: '',
    kind: 'app',
    tenant_scope: null,
    all_models: false,
    prompt_sensitivity: 'credentials',
    fair_weight: 1,
    admission_lane: 'bulk',
    allowed_cidrs: null,
    allow_plaintext: false,
    active: true,
    status: 'active',
    last_used_at: null,
    created_at: '2026-06-29T11:00:00Z',
    created_by_label: null,
    keys: [],
    keys_total: 1,
    peer: null,
    ...p,
  }
}

function page(labels: string[], next: string | null): ConsumerListResponse {
  return { success: true, consumers: labels.map((l) => consumer(l)), next_cursor: next }
}

/** Fake visibility source: the test flips `visible` and fires the listener. */
function fakeVisibility(initial: 'visible' | 'hidden') {
  let state = initial
  let listener: (() => void) | null = null
  const source: VisibilitySource = {
    visible: () => state === 'visible',
    subscribe(onChange) {
      listener = onChange
      return () => {
        listener = null
      }
    },
  }
  return {
    source,
    set(next: 'visible' | 'hidden') {
      state = next
      listener?.()
    },
    hasListener: () => listener !== null,
  }
}

/** Scripted list api: each call answers the next queued response (or the last one again). */
function fakeApi(responses: ConsumerListResponse[] = [page(['one'], null)]) {
  const calls: ConsumerListParams[] = []
  let i = 0
  const list = vi.fn(async (params: ConsumerListParams = {}) => {
    calls.push(params)
    const r = responses[Math.min(i, responses.length - 1)]
    i++
    return r
  })
  const tenants = vi.fn(
    async (): Promise<TenantListResponse> => ({
      success: true,
      tenants: [
        { id: 't-a', slug: 'acme', display_name: 'Acme Corp', status: 'active', created_at: '2026-05-01T08:00:00Z', updated_at: '2026-06-01T08:00:00Z' },
      ],
    }),
  )
  return { api: { list, tenants }, calls, list, tenants }
}

describe('ConsumersModel polling (G4, design/05 §4.7)', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it('polls every 30 s (the design/05 §4.7 cadence constant)', () => {
    expect(CONSUMER_POLL_MS).toBe(30_000)
  })

  it('hidden tab ⇒ 0 polls over 90 s fake time after the initial load', async () => {
    const vis = fakeVisibility('hidden')
    const f = fakeApi()
    const m = new ConsumersModel(f.api, vis.source)
    m.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(f.list).toHaveBeenCalledTimes(1) // the initial load
    await vi.advanceTimersByTimeAsync(90_000)
    expect(f.list).toHaveBeenCalledTimes(1)
    m.dispose()
  })

  it('visible tab ⇒ exactly 3 polls over 90 s fake time', async () => {
    const vis = fakeVisibility('visible')
    const f = fakeApi()
    const m = new ConsumersModel(f.api, vis.source)
    m.start()
    await vi.advanceTimersByTimeAsync(0)
    expect(f.list).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(90_000)
    expect(f.list).toHaveBeenCalledTimes(4)
    m.dispose()
  })

  it('dispose() ⇒ no further call and the visibility listener is removed', async () => {
    const vis = fakeVisibility('visible')
    const f = fakeApi()
    const m = new ConsumersModel(f.api, vis.source)
    m.start()
    await vi.advanceTimersByTimeAsync(30_000)
    expect(f.list).toHaveBeenCalledTimes(2)
    expect(vis.hasListener()).toBe(true)
    m.dispose()
    expect(vis.hasListener()).toBe(false)
    await vi.advanceTimersByTimeAsync(90_000)
    expect(f.list).toHaveBeenCalledTimes(2)
  })

  it('a tick skipped while hidden is caught up once when the tab turns visible, then the cadence resumes', async () => {
    const vis = fakeVisibility('hidden')
    const f = fakeApi()
    const m = new ConsumersModel(f.api, vis.source)
    m.start()
    await vi.advanceTimersByTimeAsync(31_000) // one tick skipped while hidden
    expect(f.list).toHaveBeenCalledTimes(1)
    vis.set('visible')
    await vi.advanceTimersByTimeAsync(0)
    expect(f.list).toHaveBeenCalledTimes(2) // the single catch-up
    vis.set('visible') // a second visible event without a missed tick: nothing
    await vi.advanceTimersByTimeAsync(0)
    expect(f.list).toHaveBeenCalledTimes(2)
    await vi.advanceTimersByTimeAsync(29_000) // next interval tick at 60 s
    expect(f.list).toHaveBeenCalledTimes(3)
    m.dispose()
  })

  it('becoming visible without a missed tick does not poll', async () => {
    const vis = fakeVisibility('visible')
    const f = fakeApi()
    const m = new ConsumersModel(f.api, vis.source)
    m.start()
    await vi.advanceTimersByTimeAsync(0)
    vis.set('hidden')
    vis.set('visible')
    await vi.advanceTimersByTimeAsync(0)
    expect(f.list).toHaveBeenCalledTimes(1)
    m.dispose()
  })

  it('start() is idempotent: one timer, one initial load', async () => {
    const vis = fakeVisibility('visible')
    const f = fakeApi()
    const m = new ConsumersModel(f.api, vis.source)
    m.start()
    m.start()
    await vi.advanceTimersByTimeAsync(30_000)
    expect(f.list).toHaveBeenCalledTimes(2)
    m.dispose()
  })

  it('a poll re-walks every loaded page from the start (keyset, cursor from the fresh page)', async () => {
    const vis = fakeVisibility('visible')
    const f = fakeApi([page(['a1', 'a2'], 'CUR-1'), page(['b1'], null), page(['a1', 'a2'], 'CUR-1b'), page(['b1', 'b2'], null)])
    const m = new ConsumersModel(f.api, vis.source)
    m.start()
    await vi.advanceTimersByTimeAsync(0)
    await m.loadMore()
    expect(m.consumers.map((c) => c.label)).toEqual(['a1', 'a2', 'b1'])
    await vi.advanceTimersByTimeAsync(30_000)
    // poll = page 1 without a cursor, then page 2 with the FRESH page-1 cursor
    expect(f.calls.slice(2).map((c) => c.cursor)).toEqual([undefined, 'CUR-1b'])
    expect(m.consumers.map((c) => c.label)).toEqual(['a1', 'a2', 'b1', 'b2'])
    expect(m.nextCursor).toBeNull()
    m.dispose()
  })

  it('a failed poll keeps the list and surfaces refreshError; the next good poll clears it', async () => {
    const vis = fakeVisibility('visible')
    let fail = false
    const list = vi.fn(async () => {
      if (fail) throw new ApiError(500, 'internal', 'boom')
      return page(['keep'], null)
    })
    const m = new ConsumersModel({ list, tenants: fakeApi().tenants }, vis.source)
    m.start()
    await vi.advanceTimersByTimeAsync(0)
    fail = true
    await vi.advanceTimersByTimeAsync(30_000)
    expect(m.consumers.map((c) => c.label)).toEqual(['keep'])
    expect(m.status).toBe('ready')
    expect(m.refreshError).toBe('boom')
    fail = false
    await vi.advanceTimersByTimeAsync(30_000)
    expect(m.refreshError).toBeNull()
    m.dispose()
  })
})

describe('ConsumersModel keyset + filter (G5)', () => {
  it('loadMore() hands next_cursor through byte-unchanged and appends the page', async () => {
    // Opaque on purpose: '+', '/', '=' and a '%2B' sequence — any decode,
    // encode or trim of the token changes these bytes.
    const opaque = ' MjAy%2BNi0wNi0y+OVQx/MTowMDowMFp8aWQ_== '
    const f = fakeApi([page(['p1a', 'p1b'], opaque), page(['p2a'], null)])
    const m = new ConsumersModel(f.api, fakeVisibility('hidden').source)
    await m.load()
    expect(m.nextCursor).toBe(opaque)
    await m.loadMore()
    expect(f.calls[1].cursor).toBe(opaque)
    expect(m.consumers.map((c) => c.label)).toEqual(['p1a', 'p1b', 'p2a'])
    expect(m.nextCursor).toBeNull()
  })

  it('next_cursor null ⇒ loadMore() issues no further call', async () => {
    const f = fakeApi([page(['only'], null)])
    const m = new ConsumersModel(f.api, fakeVisibility('hidden').source)
    await m.load()
    await m.loadMore()
    expect(f.list).toHaveBeenCalledTimes(1)
  })

  it('a filter change drops the cursor and reloads page 1', async () => {
    const f = fakeApi([page(['p1'], 'CUR-A'), page(['f1'], 'CUR-F')])
    const m = new ConsumersModel(f.api, fakeVisibility('hidden').source)
    await m.load()
    await m.setFilter({ q: 'ci' })
    expect(f.calls[1].cursor).toBeUndefined()
    expect(f.calls[1].q).toBe('ci')
    expect(m.consumers.map((c) => c.label)).toEqual(['f1'])
    expect(m.nextCursor).toBe('CUR-F')
  })

  it('default filter = the four statuses of an active consumer, sent explicitly (design/05 §3.2)', async () => {
    const f = fakeApi()
    const m = new ConsumersModel(f.api, fakeVisibility('hidden').source)
    await m.load()
    expect(m.filter.statuses).toEqual(['active', 'expiring', 'rotating', 'no_key'])
    expect(f.calls[0]).toEqual({ status: ['active', 'expiring', 'rotating', 'no_key'] })
  })

  it('all five statuses travel as ["all"]; a set travels in the server vocabulary order', async () => {
    const f = fakeApi()
    const m = new ConsumersModel(f.api, fakeVisibility('hidden').source)
    await m.setAllStatuses(true)
    expect(m.allStatuses).toBe(true)
    expect(f.calls[0].status).toEqual(['all'])
    await m.setFilter({ statuses: ['inactive', 'active'] })
    expect(f.calls[1].status).toEqual(['active', 'inactive'])
    // leaving "all" restores the default set
    await m.setAllStatuses(false)
    expect(m.filter.statuses).toEqual(['active', 'expiring', 'rotating', 'no_key'])
  })

  it('toggleStatus never empties the set (an empty status list is not a filter the server knows)', async () => {
    const f = fakeApi()
    const m = new ConsumersModel(f.api, fakeVisibility('hidden').source)
    await m.setFilter({ statuses: ['inactive'] })
    await m.toggleStatus('inactive')
    expect(m.filter.statuses).toEqual(['inactive'])
    expect(f.list).toHaveBeenCalledTimes(1)
    await m.toggleStatus('active')
    expect(m.filter.statuses).toEqual(['active', 'inactive'])
  })

  it('q and tenant_id ride only when set', async () => {
    const f = fakeApi()
    const m = new ConsumersModel(f.api, fakeVisibility('hidden').source)
    await m.setFilter({ q: 'ci-', tenantId: 't-b' })
    expect(f.calls[0]).toEqual({ status: ['active', 'expiring', 'rotating', 'no_key'], q: 'ci-', tenant_id: 't-b' })
    await m.setFilter({ q: '', tenantId: null })
    expect(f.calls[1]).toEqual({ status: ['active', 'expiring', 'rotating', 'no_key'] })
  })

  it('a stale page-1 answer never overwrites the newer filter', async () => {
    let resolveFirst: (r: ConsumerListResponse) => void = () => {}
    const list = vi
      .fn()
      .mockImplementationOnce(() => new Promise<ConsumerListResponse>((res) => (resolveFirst = res)))
      .mockImplementationOnce(async () => page(['new'], null))
    const m = new ConsumersModel({ list, tenants: fakeApi().tenants }, fakeVisibility('hidden').source)
    const first = m.load()
    await m.setFilter({ q: 'n' })
    resolveFirst(page(['stale'], 'CUR-STALE'))
    await first
    expect(m.consumers.map((c) => c.label)).toEqual(['new'])
    expect(m.nextCursor).toBeNull()
  })

  it('a load error lands on loadError/status; loadMore error on moreError, the list stays', async () => {
    const list = vi
      .fn()
      .mockImplementationOnce(async () => {
        throw new ApiError(403, 'forbidden', 'admin key required')
      })
      .mockImplementationOnce(async () => page(['x'], 'CUR'))
      .mockImplementationOnce(async () => {
        throw new ApiError(400, 'bad_request', 'invalid cursor')
      })
    const m = new ConsumersModel({ list, tenants: fakeApi().tenants }, fakeVisibility('hidden').source)
    await m.load()
    expect(m.status).toBe('error')
    expect(m.loadError?.message).toBe('admin key required')
    await m.load()
    expect(m.status).toBe('ready')
    await m.loadMore()
    expect(m.moreError).toBe('invalid cursor')
    expect(m.consumers.map((c) => c.label)).toEqual(['x'])
    expect(m.nextCursor).toBe('CUR')
  })

  it('loadTenants() fills the server-admin tenant filter options', async () => {
    const f = fakeApi()
    const m = new ConsumersModel(f.api, fakeVisibility('hidden').source)
    await m.loadTenants()
    expect(m.tenants.map((t) => t.slug)).toEqual(['acme'])
    expect(f.list).not.toHaveBeenCalled()
  })
})

// B-11 (design/05 §5): the status of a consumer or key comes from the server —
// the gateway surface never reads the client clock. A static scan of the area
// is the mechanical half of that rule (a `new Date(ts)` formatter is fine; a
// `Date.now` / argument-less `new Date` — with or without parentheses, e.g.
// `+new Date` — is not). The e2e half: the fixture's ci-chat key is `active`
// although its expires_at lies before the page clock (server clock skew), and
// the contract's primaryFlow asserts the server's word wins.
describe('no client clock in the gateway surface (B-11)', () => {
  it('src/routes/gateway/* reads neither Date.now nor an argument-less new Date', () => {
    const dir = new URL('.', import.meta.url)
    const files = readdirSync(dir).filter((f) => /\.(svelte|ts)$/.test(f) && !f.endsWith('.test.ts'))
    expect(files.length).toBeGreaterThan(0)
    // the pattern itself: allowed formatter vs. every forbidden clock read
    const clock = /\bDate\.now\b|\bnew\s+Date\b(?!\s*\(\s*[^)\s])|\bperformance\.now\b/
    expect('new Date(ts)').not.toMatch(clock)
    for (const bad of ['Date.now()', 'new Date()', 'new Date', '+new Date <= x', 'new Date( )', 'performance.now()']) {
      expect(bad, bad).toMatch(clock)
    }
    for (const f of files) {
      const src = readFileSync(new URL(f, dir), 'utf8')
      expect(src, f).not.toMatch(clock)
    }
  })
})
