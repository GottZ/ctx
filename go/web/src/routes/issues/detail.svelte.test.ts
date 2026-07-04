// IssueDetailModel pins (design 04 §4.1/§4.5/§5.5, wave U06). load / uniform-404 /
// comment keyset paging + progressive reveal / pessimistic mutations — all
// DOM-free via an injected api.

import { describe, expect, it, vi } from 'vitest'
import { ApiError } from '../../lib/api'
import {
  COMMENT_RENDER_CAP,
  IssueDetailModel,
  type DetailApi,
} from './detail.svelte'
import type {
  CommentCreateResponse,
  IssueBlock,
  IssueCommentsResponse,
  IssueCursor,
  IssueDetailResponse,
  IssueMutateResponse,
} from '../../lib/api/types'

function block(over: Partial<IssueBlock> = {}): IssueBlock {
  return {
    id: '11111111-1111-1111-1111-111111111111',
    category: 'task',
    tags: [],
    title: 'Example issue',
    content: '# Body',
    metadata: {},
    scope: 'acme:main',
    type: 'issue',
    workflow_status: 'open',
    created_at: '2026-07-01T00:00:00Z',
    updated_at: '2026-07-03T00:00:00Z',
    ...over,
  }
}

function comment(i: number): IssueBlock {
  return block({
    id: `c-${String(i).padStart(6, '0')}`,
    category: 'comment',
    type: 'comment',
    title: '',
    content: `comment ${i}`,
    workflow_status: undefined,
  })
}

function detailResponse(comments: IssueBlock[], cursor: IssueCursor): IssueDetailResponse {
  return { success: true, render: 'untrusted', issue: block(), comments, comments_cursor: cursor }
}

function okApi(over: Partial<DetailApi> = {}): DetailApi {
  return {
    getIssue: vi.fn(async () => detailResponse([comment(0)], null)),
    listComments: vi.fn(
      async (): Promise<IssueCommentsResponse> => ({ success: true, render: 'untrusted', comments: [], cursor: null }),
    ),
    patchIssue: vi.fn(
      async (_p: string, _b: string, body): Promise<IssueMutateResponse> => ({
        success: true,
        render: 'untrusted',
        issue: block({ workflow_status: body.status ?? 'open', title: body.title ?? 'Example issue' }),
      }),
    ),
    createComment: vi.fn(
      async (_p: string, _b: string, body): Promise<CommentCreateResponse> => ({
        success: true,
        render: 'untrusted',
        comment: comment(999),
      }),
    ),
    ...over,
  }
}

describe('IssueDetailModel — load', () => {
  it('loads the issue + first comment page', async () => {
    const m = new IssueDetailModel('p', 'b', okApi())
    await m.load()
    expect(m.status).toBe('ready')
    expect(m.issue?.title).toBe('Example issue')
    expect(m.comments).toHaveLength(1)
    expect(m.notFound).toBe(false)
  })

  it('maps a 404 to notFound (EmptyState), NOT the error band', async () => {
    const api = okApi({
      getIssue: vi.fn(async () => {
        throw new ApiError(404, 'not_found', 'issue not found')
      }),
    })
    const m = new IssueDetailModel('p', 'b', api)
    await m.load()
    expect(m.notFound).toBe(true)
    expect(m.status).toBe('ready') // ready + notFound → EmptyState, no redirect
    expect(m.loadError).toBeNull()
    expect(m.issue).toBeNull()
  })

  it('surfaces a non-404 failure as the error state', async () => {
    const api = okApi({
      getIssue: vi.fn(async () => {
        throw new ApiError(500, 'internal', 'boom')
      }),
    })
    const m = new IssueDetailModel('p', 'b', api)
    await m.load()
    expect(m.notFound).toBe(false)
    expect(m.status).toBe('error')
    expect(m.loadError?.status).toBe(500)
  })
})

describe('IssueDetailModel — comment thread', () => {
  it('progressive reveal caps the rendered comments and reveals more', async () => {
    const many = Array.from({ length: 500 }, (_, k) => comment(k))
    const m = new IssueDetailModel('p', 'b', okApi({ getIssue: vi.fn(async () => detailResponse(many, null)) }))
    await m.load()
    expect(m.comments).toHaveLength(500)
    expect(m.visibleComments).toHaveLength(COMMENT_RENDER_CAP)
    expect(m.hasHiddenComments).toBe(true)
    m.revealMore()
    expect(m.visibleComments).toHaveLength(COMMENT_RENDER_CAP * 2)
  })

  it('appends a keyset comment page and adopts the next cursor', async () => {
    const api = okApi({
      getIssue: vi.fn(async () => detailResponse([comment(0)], 'CUR')),
      listComments: vi.fn(async () => ({
        success: true as const,
        render: 'untrusted' as const,
        comments: [comment(1), comment(2)],
        cursor: null,
      })),
    })
    const m = new IssueDetailModel('p', 'b', api)
    await m.load()
    expect(m.canLoadMoreComments).toBe(true)
    await m.loadMoreComments()
    expect(m.comments.map((c) => c.id)).toEqual(['c-000000', 'c-000001', 'c-000002'])
    expect(m.canLoadMoreComments).toBe(false)
  })
})

describe('IssueDetailModel — mutations (pessimistic)', () => {
  it('changeStatus adopts the server issue on success', async () => {
    const m = new IssueDetailModel('p', 'b', okApi())
    await m.load()
    await m.changeStatus('closed')
    expect(m.issue?.workflow_status).toBe('closed')
    expect(m.mutating).toBe(false)
  })

  it('changeStatus REJECTS on a 422 policy violation and keeps the old status', async () => {
    const api = okApi({
      patchIssue: vi.fn(async () => {
        throw new ApiError(422, 'invalid_transition', 'transition open→done not allowed')
      }),
    })
    const m = new IssueDetailModel('p', 'b', api)
    await m.load()
    await expect(m.changeStatus('done')).rejects.toThrow(/not allowed/)
    expect(m.issue?.workflow_status).toBe('open') // unchanged — selection retained
    expect(m.mutating).toBe(false)
  })

  it('changeTitle adopts the server title on success', async () => {
    const m = new IssueDetailModel('p', 'b', okApi())
    await m.load()
    await m.changeTitle('New title')
    expect(m.issue?.title).toBe('New title')
  })

  it('addComment appends the created comment and reveals it', async () => {
    const m = new IssueDetailModel('p', 'b', okApi())
    await m.load()
    await m.addComment('  hello world  ')
    expect(m.comments.at(-1)?.id).toBe('c-000999')
  })

  it('addComment no-ops on a blank body (no round-trip)', async () => {
    const api = okApi()
    const m = new IssueDetailModel('p', 'b', api)
    await m.load()
    await m.addComment('   ')
    expect(api.createComment).not.toHaveBeenCalled()
  })

  it('addComment REJECTS on server error so the draft is kept', async () => {
    const api = okApi({
      createComment: vi.fn(async () => {
        throw new ApiError(403, 'forbidden', 'read-only scope')
      }),
    })
    const m = new IssueDetailModel('p', 'b', api)
    await m.load()
    await expect(m.addComment('hi')).rejects.toThrow(/read-only/)
    expect(m.posting).toBe(false)
  })
})
