// BoardModel pins (design 04 §4.2/§4.5/§6.2, waves U07 + U08). DOM-free via an
// injected api: the board+registry join, the closed-collapse seed + toggle, the
// per-column keyset window (count from the wire, cards appended, cap), and the
// U08 OPTIMISTIC TRANSITION (move → confirm | rollback + wire re-read).
//
// The CLOSED-COLLAPSE negative gate (RED-then-GREEN): a terminal column starts
// collapsed. RED against a seed that ignores the registry — make initialCollapsed
// return [] (or drop the `for (…initialCollapsed…)` loop in load()) and the
// "done starts collapsed" assertion below fails.

import { describe, expect, it, vi } from 'vitest'
import { BOARD_COLUMN_CAP, BoardModel, type BoardApi } from './board.svelte'
import { ApiError } from '../../lib/api'
import type {
  BlockTypeView,
  BoardResponse,
  IssueBlock,
  IssueListResponse,
  IssueMutateResponse,
  IssueRow,
  TypesListResponse,
} from '../../lib/api/types'

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

function issueBlock(id: string, status: string): IssueBlock {
  return {
    id,
    category: 'issue',
    tags: [],
    title: 'moved',
    content: '',
    metadata: {},
    scope: 'acme:main',
    type: 'issue',
    workflow_status: status,
    created_at: '2026-07-03T00:00:00Z',
    updated_at: '2026-07-04T00:00:00Z',
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
    patchIssue: vi.fn(async (_id, blockId, body): Promise<IssueMutateResponse> => ({
      success: true,
      render: 'untrusted',
      issue: issueBlock(blockId, body.status),
    })),
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

describe('BoardModel.transition (U08)', () => {
  it('optimistically moves the card + counts, then reconciles from the server issue', async () => {
    const a = api()
    const m = new BoardModel('p1', a)
    await m.load()
    await m.transition('open-0000', 'open', 'review')
    const open = m.columns.find((c) => c.status === 'open')!
    const review = m.columns.find((c) => c.status === 'review')!
    // open lost the card + a count; review gained it (at the top) + a count.
    expect(open.issues.map((i) => i.id)).not.toContain('open-0000')
    expect(open.count).toBe(2)
    expect(review.issues[0].id).toBe('open-0000')
    expect(review.issues[0].workflow_status).toBe('review')
    expect(review.count).toBe(2)
    expect(m.transitionError).toBeNull()
    // PATCH carried the target status (B5).
    expect(a.patchIssue).toHaveBeenCalledWith('p1', 'open-0000', { status: 'review' })
  })

  it('409 rolls the move back AND re-reads the wire (board GET fires again)', async () => {
    const a = api({
      patchIssue: vi.fn(async () => Promise.reject(new ApiError(409, 'conflict', 'transition not allowed'))),
    })
    const m = new BoardModel('p1', a)
    await m.load()
    expect(a.getBoard).toHaveBeenCalledTimes(1)
    await m.transition('open-0000', 'open', 'review')
    // Rolled back: the card is back in open, counts restored.
    const open = m.columns.find((c) => c.status === 'open')!
    const review = m.columns.find((c) => c.status === 'review')!
    expect(open.issues.map((i) => i.id)).toContain('open-0000')
    expect(open.count).toBe(3)
    expect(review.count).toBe(1)
    // Error surfaced + the wire re-read fired (§4.8 registry staleness).
    expect(m.transitionError?.status).toBe(409)
    expect(a.getBoard).toHaveBeenCalledTimes(2)
  })

  it('422 (invalid transition) rolls back, surfaces the error, re-reads the wire', async () => {
    const a = api({
      patchIssue: vi.fn(async () => Promise.reject(new ApiError(422, 'unprocessable', 'not a valid transition'))),
    })
    const m = new BoardModel('p1', a)
    await m.load()
    await m.transition('open-0000', 'open', 'review')
    const open = m.columns.find((c) => c.status === 'open')!
    expect(open.issues.map((i) => i.id)).toContain('open-0000')
    expect(open.count).toBe(3)
    expect(m.transitionError?.status).toBe(422)
    expect(a.getBoard).toHaveBeenCalledTimes(2)
  })

  it('403 (read-only race) rolls back + surfaces, but does NOT re-read (only 409/422 do)', async () => {
    const a = api({
      patchIssue: vi.fn(async () => Promise.reject(new ApiError(403, 'forbidden', 'read-only scope'))),
    })
    const m = new BoardModel('p1', a)
    await m.load()
    await m.transition('open-0000', 'open', 'review')
    expect(m.columns.find((c) => c.status === 'open')!.issues.map((i) => i.id)).toContain('open-0000')
    expect(m.transitionError?.status).toBe(403)
    expect(a.getBoard).toHaveBeenCalledTimes(1) // no wire re-read for a 403
  })

  it('a same-column transition is a no-op (no PATCH)', async () => {
    const a = api()
    const m = new BoardModel('p1', a)
    await m.load()
    await m.transition('open-0000', 'open', 'open')
    expect(a.patchIssue).not.toHaveBeenCalled()
  })

  it('refuses a drop onto an unmapped column (never a drop target)', async () => {
    const a = api({
      getBoard: vi.fn(async () => ({
        success: true as const,
        render: 'untrusted' as const,
        columns: [
          { status: 'open', count: 1, cursor: null, issues: [boardCard('open', 0)] },
          { status: 'on_hold', count: 1, cursor: null, issues: [boardCard('on_hold', 0)] },
        ],
      })),
    })
    const m = new BoardModel('p1', a)
    await m.load()
    expect(m.columns.find((c) => c.status === 'on_hold')?.category).toBe('unmapped')
    await m.transition('open-0000', 'open', 'on_hold')
    expect(a.patchIssue).not.toHaveBeenCalled()
  })

  it('moveTargets = droppable statuses minus the current column and the unmapped ones', async () => {
    const a = api({
      getBoard: vi.fn(async () => ({
        success: true as const,
        render: 'untrusted' as const,
        columns: [
          { status: 'open', count: 0, cursor: null, issues: [] },
          { status: 'review', count: 0, cursor: null, issues: [] },
          { status: 'done', count: 0, cursor: null, issues: [] },
          { status: 'on_hold', count: 0, cursor: null, issues: [] }, // unmapped
        ],
      })),
    })
    const m = new BoardModel('p1', a)
    await m.load()
    expect(m.moveTargets('open')).toEqual(['review', 'done'])
    expect(m.isDropTarget('on_hold')).toBe(false)
    expect(m.isDropTarget('review')).toBe(true)
  })
})
