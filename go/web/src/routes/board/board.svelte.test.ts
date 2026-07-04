// BoardModel pins (design 04 §4.2/§6.2, wave U07). DOM-free via an injected api:
// the board+registry join, the closed-collapse seed + toggle, and the per-column
// keyset window (count from the wire, cards appended, cap).
//
// The CLOSED-COLLAPSE negative gate (RED-then-GREEN): a terminal column starts
// collapsed. RED against a seed that ignores the registry — make initialCollapsed
// return [] (or drop the `for (…initialCollapsed…)` loop in load()) and the
// "done starts collapsed" assertion below fails.

import { describe, expect, it, vi } from 'vitest'
import { BOARD_COLUMN_CAP, BoardModel, type BoardApi } from './board.svelte'
import type { BlockTypeView, BoardResponse, IssueListResponse, IssueRow, TypesListResponse } from '../../lib/api/types'

function boardCard(status: string, i: number): IssueRow {
  return {
    id: `${status}-${String(i).padStart(4, '0')}`,
    scope: 'acme:main',
    type_name: 'issue',
    title: `${status} ${i}`,
    workflow_status: status,
    updated_at: '2026-07-03T00:00:00Z',
  }
}

function types(): TypesListResponse {
  const t: BlockTypeView = {
    id: '7',
    name: 'issue',
    scope: '_global',
    display_name: 'Issue',
    description: '',
    builtin: true,
    is_default: false,
    source: 'builtin',
    created_at: '2026-07-03T00:00:00Z',
    updated_at: '2026-07-03T00:00:00Z',
    config: { v: 1, workflow: { states: ['open', 'review', 'done'], initial: 'open', terminal: ['done'] } },
  }
  return { success: true, types: [t] }
}

/** Board: open (count 3, full), review (count 1), done (terminal, count 100 but
 * only a 2-card first page + a cursor). */
function board(): BoardResponse {
  return {
    success: true,
    render: 'untrusted',
    columns: [
      { status: 'open', count: 3, cursor: null, issues: [boardCard('open', 0), boardCard('open', 1), boardCard('open', 2)] },
      { status: 'review', count: 1, cursor: null, issues: [boardCard('review', 0)] },
      { status: 'done', count: 100, cursor: 'idx-1', issues: [boardCard('done', 0), boardCard('done', 1)] },
    ],
  }
}

function api(overrides: Partial<BoardApi> = {}): BoardApi {
  return {
    getBoard: vi.fn(async () => board()),
    listTypes: vi.fn(async () => types()),
    listIssues: vi.fn(async (_id, params = {}): Promise<IssueListResponse> => {
      // Serve one more done page then end.
      const after = params.after
      const start = after ? parseInt(String(after).replace(/^idx-/, ''), 10) + 1 : 0
      return {
        success: true,
        render: 'untrusted',
        issues: [boardCard(params.state ?? 'done', start), boardCard(params.state ?? 'done', start + 1)],
        cursor: null,
      }
    }),
    ...overrides,
  }
}

describe('BoardModel.load', () => {
  it('joins board + registry: columns in WIRE order, categories, counts from the wire', async () => {
    const m = new BoardModel('p1', api())
    await m.load()
    expect(m.status).toBe('ready')
    expect(m.columns.map((c) => c.status)).toEqual(['open', 'review', 'done'])
    expect(m.columns.map((c) => c.category)).toEqual(['open', 'open', 'closed'])
    // Count is the WIRE count, NOT the loaded card length (done: 100 vs 2 loaded).
    expect(m.columns.find((c) => c.status === 'done')?.count).toBe(100)
    expect(m.columns.find((c) => c.status === 'done')?.issues).toHaveLength(2)
  })

  it('CLOSED-COLLAPSE: the terminal column starts collapsed, others expanded', async () => {
    const m = new BoardModel('p1', api())
    await m.load()
    expect(m.isCollapsed('done')).toBe(true)
    expect(m.isCollapsed('open')).toBe(false)
    expect(m.isCollapsed('review')).toBe(false)
  })

  it('toggle flips a column collapse both ways', async () => {
    const m = new BoardModel('p1', api())
    await m.load()
    m.toggle('done')
    expect(m.isCollapsed('done')).toBe(false)
    m.toggle('done')
    expect(m.isCollapsed('done')).toBe(true)
    m.toggle('open')
    expect(m.isCollapsed('open')).toBe(true)
  })

  it('fails closed when the registry read fails (board never renders guessed columns)', async () => {
    const m = new BoardModel('p1', api({ listTypes: vi.fn(async () => Promise.reject(new Error('registry down'))) }))
    await m.load()
    expect(m.status).toBe('error')
    expect(m.columns).toHaveLength(0)
  })
})

describe('BoardModel per-column window', () => {
  it('canLoadMore is true only for a column with a cursor', async () => {
    const m = new BoardModel('p1', api())
    await m.load()
    expect(m.canLoadMore('done')).toBe(true) // cursor 'idx-1'
    expect(m.canLoadMore('open')).toBe(false) // cursor null
  })

  it('loadMore appends the next page to THAT column only, then closes at cursor null', async () => {
    const m = new BoardModel('p1', api())
    await m.load()
    await m.loadMore('done')
    const done = m.columns.find((c) => c.status === 'done')
    expect(done?.issues).toHaveLength(4) // 2 + 2 appended
    expect(done?.count).toBe(100) // count unchanged (wire total)
    expect(done?.cursor).toBeNull()
    expect(m.canLoadMore('done')).toBe(false)
    // Other columns untouched.
    expect(m.columns.find((c) => c.status === 'open')?.issues).toHaveLength(3)
  })

  it('stops paging at the per-column cap', async () => {
    // A column whose pages never end — the cap must stop the append.
    const endless: BoardApi = api({
      getBoard: vi.fn(async () => ({
        success: true as const,
        render: 'untrusted' as const,
        columns: [{ status: 'open', count: 999999, cursor: 'idx-0', issues: [boardCard('open', 0)] }],
      })),
      listIssues: vi.fn(async () => ({
        success: true as const,
        render: 'untrusted' as const,
        issues: Array.from({ length: 400 }, (_, k) => boardCard('open', k)),
        cursor: 'more',
      })),
    })
    const m = new BoardModel('p1', endless)
    await m.load()
    // Append until the cap closes the cursor.
    for (let i = 0; i < 10 && m.canLoadMore('open'); i++) await m.loadMore('open')
    const open = m.columns.find((c) => c.status === 'open')
    expect(open!.issues.length).toBeGreaterThanOrEqual(BOARD_COLUMN_CAP)
    expect(m.canLoadMore('open')).toBe(false)
  })
})
