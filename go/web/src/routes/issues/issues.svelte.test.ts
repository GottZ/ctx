// IssuesModel pins (design 04 §4.2/§5.5, wave U05). Two-mode logic, keyset
// append with a stable order + no duplicate, the 50k cap, and the SEARCH-mode
// append-suppression negative gate — all DOM-free via an injected api.

import { describe, expect, it, vi } from 'vitest'
import { ISSUE_ROW_CAP, IssuesModel, type IssuesApi } from './issues.svelte'
import type { IssueCursor, IssueListResponse, IssueRow } from '../../lib/api/types'

function row(i: number): IssueRow {
  return {
    id: `id-${String(i).padStart(6, '0')}`,
    scope: 'acme:main',
    type_name: 'issue',
    title: `Issue ${i}`,
    workflow_status: i % 2 === 0 ? 'open' : 'in_progress',
    updated_at: new Date(Date.UTC(2026, 6, 3) - i * 60_000).toISOString(),
  }
}

function page(from: number, count: number, cursor: IssueCursor): IssueListResponse {
  return {
    success: true,
    render: 'untrusted',
    issues: Array.from({ length: count }, (_, k) => row(from + k)),
    cursor,
  }
}

/** An api that serves two keyset pages then ends (cursor null). */
function twoPageApi(): IssuesApi {
  return {
    listIssues: vi.fn(async (_id: string, params = {}) => {
      return params.after === undefined ? page(0, 50, 'CURSOR_P2') : page(50, 50, null)
    }),
  }
}

describe('IssuesModel — browse mode', () => {
  it('loads page 1 and adopts the next cursor', async () => {
    const m = new IssuesModel('p1', twoPageApi())
    await m.load({})
    expect(m.status).toBe('ready')
    expect(m.rows).toHaveLength(50)
    expect(m.cursor).toBe('CURSOR_P2')
    expect(m.searchMode).toBe(false)
    expect(m.canLoadMore).toBe(true)
  })

  it('loadMore appends the 2nd page — stable order, no duplicate, stops at null', async () => {
    const m = new IssuesModel('p1', twoPageApi())
    await m.load({})
    await m.loadMore()
    expect(m.rows).toHaveLength(100)
    // Order stable and contiguous, no duplicated id across the page boundary.
    const ids = m.rows.map((r) => r.id)
    expect(new Set(ids).size).toBe(100)
    expect(ids[49]).toBe('id-000049')
    expect(ids[50]).toBe('id-000050')
    // 2nd page returned cursor null → paging stops.
    expect(m.cursor).toBeNull()
    expect(m.canLoadMore).toBe(false)
  })

  it('re-issues the SAME query WITH the keyset cursor on loadMore', async () => {
    const api = twoPageApi()
    const m = new IssuesModel('p1', api)
    await m.load({ state: 'open' })
    await m.loadMore()
    const calls = (api.listIssues as ReturnType<typeof vi.fn>).mock.calls
    expect(calls[0][1]).toEqual({ state: 'open' })
    expect(calls[1][1]).toEqual({ state: 'open', after: 'CURSOR_P2' })
  })

  it('stops appending at the 50k cap and flags capped', async () => {
    // Every page returns a non-null cursor → only the cap can stop it.
    const api: IssuesApi = {
      listIssues: vi.fn(async (_id, params = {}) => {
        const start = params.after ? Number(params.after) : 0
        return page(start, 25_000, String(start + 25_000))
      }),
    }
    const m = new IssuesModel('p1', api)
    await m.load({})
    expect(m.rows).toHaveLength(25_000)
    await m.loadMore()
    expect(m.rows).toHaveLength(50_000)
    expect(m.rows.length).toBeGreaterThanOrEqual(ISSUE_ROW_CAP)
    expect(m.capped).toBe(true)
    expect(m.canLoadMore).toBe(false) // capped kills the affordance even with a cursor
    await m.loadMore() // inert past the cap
    expect(m.rows).toHaveLength(50_000)
  })

  it('keeps the rows already shown when an append fails', async () => {
    let call = 0
    const api: IssuesApi = {
      listIssues: vi.fn(async () => {
        call += 1
        if (call === 1) return page(0, 50, 'P2')
        throw new Error('boom')
      }),
    }
    const m = new IssuesModel('p1', api)
    await m.load({})
    await m.loadMore()
    expect(m.rows).toHaveLength(50) // not blanked
    expect(m.loadError?.message).toContain('boom')
  })
})

describe('IssuesModel — search mode (append suppression gate)', () => {
  it('never offers append in search mode, EVEN IF the wire returns a cursor', async () => {
    // The negative gate: a server that (wrongly) sends a non-null cursor in
    // search mode must NOT resurrect the infinite-scroll affordance. A model
    // that gated only on `cursor !== null` would render Load-more here (RED).
    const api: IssuesApi = {
      listIssues: vi.fn(async () => page(0, 20, 'ROGUE_CURSOR')),
    }
    const m = new IssuesModel('p1', api)
    await m.load({ q: 'flaky' })
    expect(m.searchMode).toBe(true)
    expect(m.cursor).toBeNull() // pinned null despite the wire's ROGUE_CURSOR
    expect(m.canLoadMore).toBe(false)
    await m.loadMore() // inert
    const calls = (api.listIssues as ReturnType<typeof vi.fn>).mock.calls
    expect(calls).toHaveLength(1) // no second fetch
  })
})
