// U03 client + wire-freeze pins. Two teeth:
//  1. COMPILE-time — the frozen __fixtures__/*.json (Go-golden-pinned) are typed
//     as the wire interfaces; a shape drift fails `npm run check` / vitest.
//  2. RUNTIME — every client function derives its path from ISSUES_BASE (no
//     second hand-written prefix, keyset-only), proven by capturing the fetch URL.
// The Go golden (TestContractFreezeGolden) is the cross-language anchor; this
// file pins the TS side to the same JSON files.

import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  createComment,
  createIssue,
  getBoard,
  getIssue,
  getProject,
  getSyncStatus,
  ISSUES_BASE,
  listComments,
  listIssues,
  listProjects,
  MAX_ISSUE_LIMIT,
  patchIssue,
  startSync,
} from './issues'
import type {
  BoardResponse,
  CommentCreateResponse,
  IssueDetailResponse,
  IssueCommentsResponse,
  IssueListResponse,
  IssueMutateResponse,
  ProjectListResponse,
  SyncStatusResponse,
} from './types'
import boardJson from './__fixtures__/board.json'
import commentCreateJson from './__fixtures__/comment-create.json'
import issueCommentsJson from './__fixtures__/issue-comments.json'
import issueDetailJson from './__fixtures__/issue-detail.json'
import issueListJson from './__fixtures__/issue-list.json'
import issueMutateJson from './__fixtures__/issue-mutate.json'
import projectListJson from './__fixtures__/project-list.json'
import syncStatusJson from './__fixtures__/sync-status.json'

// --- 1. Freeze-JSON shape conformance (compile-time; toBe keeps a runtime suite) ---
describe('freeze JSONs satisfy the wire types (K5, Go-golden-pinned)', () => {
  it('typed imports compile against types.ts', () => {
    // JSON module imports widen `true`→`boolean` / literals→string, so the
    // handle uses `as` — the STRUCTURAL freeze is enforced cross-language by the
    // Go golden (TestContractFreezeGolden); here we pin the field VOCABULARY at
    // runtime + let the client return types carry the literal shape to consumers.
    const list = issueListJson as IssueListResponse
    const detail = issueDetailJson as IssueDetailResponse
    const comments = issueCommentsJson as IssueCommentsResponse
    const board = boardJson as BoardResponse
    const mutate = issueMutateJson as IssueMutateResponse
    const comment = commentCreateJson as CommentCreateResponse
    const projects = projectListJson as ProjectListResponse
    const sync = syncStatusJson as SyncStatusResponse

    // Ist-shape spot checks (§3.1 → Ist deviations documented in types.ts):
    expect(list.render).toBe('untrusted')
    expect(list.cursor).toBeNull() // opaque base64 | null — never a {after_*} pair
    expect(list.issues[0].workflow_status).toBe('open') // status is first-class, not in metadata
    expect(detail.issue.content).toContain('# Example') // markdown SOURCE, not HTML
    expect(comments.comments[0].type).toBe('comment')
    expect(board.columns.map((c) => c.status)).toEqual(['open', 'closed'])
    expect(mutate.issue.workflow_status).toBe('open')
    expect(comment.comment.type).toBe('comment')
    expect(projects.projects[0].scope).toBe('acme:main')
    expect(sync.run.running).toBe(false)
    expect(sync.last_run?.status).toBe('done')
  })

  it('pins that IssueRow carries NO §3.1 sync_state/comment_count/labels field', () => {
    const row = (issueListJson as IssueListResponse).issues[0] as unknown as Record<string, unknown>
    expect('sync_state' in row).toBe(false)
    expect('comment_count' in row).toBe(false)
    expect('labels' in row).toBe(false)
    expect('status' in row).toBe(false) // the field is workflow_status, Ist truth
  })
})

// --- 2. Client path coupling (runtime): every path starts at ISSUES_BASE ---
describe('issue client paths derive from ISSUES_BASE (keyset-only, no offset)', () => {
  let lastUrl = ''
  let lastInit: RequestInit | undefined

  function stubFetch(body: unknown): void {
    vi.stubGlobal('fetch', (url: string, init?: RequestInit) => {
      lastUrl = url
      lastInit = init
      return Promise.resolve({
        ok: true,
        status: 200,
        headers: { get: () => null } as unknown as Headers,
        text: () => Promise.resolve(JSON.stringify(body)),
      } as Response)
    })
  }

  afterEach(() => vi.unstubAllGlobals())

  it('every function targets a path under ISSUES_BASE', async () => {
    stubFetch(projectListJson)
    await listProjects()
    expect(lastUrl).toBe(ISSUES_BASE)

    stubFetch({ success: true, project: projectListJson.projects[0] })
    await getProject('p-1')
    expect(lastUrl.startsWith(`${ISSUES_BASE}/`)).toBe(true)

    for (const call of [
      () => listIssues('p-1'),
      () => getIssue('p-1', 'b-1'),
      () => listComments('p-1', 'b-1'),
      () => getBoard('p-1'),
      () => getSyncStatus('p-1'),
      () => createIssue('p-1', { title: 't' }),
      () => patchIssue('p-1', 'b-1', { status: 'closed' }),
      () => createComment('p-1', 'b-1', { content: 'c' }),
      () => startSync('p-1'),
    ]) {
      stubFetch(issueListJson)
      await call()
      // The single mechanical coupling: if ISSUES_BASE moved, every URL moves.
      expect(lastUrl.startsWith(`${ISSUES_BASE}/`), `path drifted: ${lastUrl}`).toBe(true)
    }
  })

  it('builds the exact endpoint-family paths (K04-A /api/project/{id}/…)', async () => {
    stubFetch(issueListJson)
    await listIssues('p-1', { state: 'open', labels: ['bug', 'p1'], limit: MAX_ISSUE_LIMIT, after: 'CURSOR', sort: 'created' })
    const u = new URL(lastUrl, 'https://x')
    expect(u.pathname).toBe(`${ISSUES_BASE}/p-1/issues`)
    // Keyset only — the cursor rides ?after=, NEVER an offset.
    expect(u.searchParams.get('after')).toBe('CURSOR')
    expect(u.searchParams.getAll('labels')).toEqual(['bug', 'p1'])
    expect(u.searchParams.get('limit')).toBe('100')
    expect(u.searchParams.has('offset')).toBe(false)

    stubFetch(issueDetailJson)
    await getIssue('p-1', 'b-9')
    expect(new URL(lastUrl, 'https://x').pathname).toBe(`${ISSUES_BASE}/p-1/issues/b-9`)

    stubFetch(boardJson)
    await getBoard('p-1')
    expect(new URL(lastUrl, 'https://x').pathname).toBe(`${ISSUES_BASE}/p-1/board`)

    stubFetch(syncStatusJson)
    await startSync('p-1', { dryRun: true })
    const s = new URL(lastUrl, 'https://x')
    expect(s.pathname).toBe(`${ISSUES_BASE}/p-1/sync`)
    expect(s.searchParams.get('dry_run')).toBe('true')
    expect(lastInit?.method).toBe('POST')
  })
})
