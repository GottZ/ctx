// Shared workflow-UI fixture data + namespace dispatcher for the e2e mocks
// (design/04 §7-U03). This module has NO @playwright/test import on purpose, so
// it is unit-testable in vitest (e2e/contract/issue-fixtures.test.ts) AND
// imported by e2e/fixtures.ts into the page.route layer.
//
// The response bodies ARE the contract-freeze JSONs in
// src/lib/api/__fixtures__/*.json — imported, NEVER hand-copied. The Go golden
// TestContractFreezeGolden pins those same files to the live W6/W7/W11/W4/types
// handler structs, so a Go-side wire drift turns the golden red before it can
// silently pass these browser mocks (closes e2e-playwright.md Finding 8).
//
// EVERY matched path is derived from ISSUES_BASE / TYPES_BASE (the single path
// prefixes) — the namespace matcher can never drift from the final endpoint form
// (U03 coupling gate). Un-mocked paths INSIDE the namespace get this module's OWN
// loud hard-fail (599), never the closed benign {success:true} catch-all (N3).

import { ISSUES_BASE } from '../src/lib/api/issues'
import { TYPES_BASE } from '../src/lib/api/types-registry'
import board from '../src/lib/api/__fixtures__/board.json' with { type: 'json' }
import commentCreate from '../src/lib/api/__fixtures__/comment-create.json' with { type: 'json' }
import issueComments from '../src/lib/api/__fixtures__/issue-comments.json' with { type: 'json' }
import issueDetail from '../src/lib/api/__fixtures__/issue-detail.json' with { type: 'json' }
import issueList from '../src/lib/api/__fixtures__/issue-list.json' with { type: 'json' }
import issueMutate from '../src/lib/api/__fixtures__/issue-mutate.json' with { type: 'json' }
import projectList from '../src/lib/api/__fixtures__/project-list.json' with { type: 'json' }
import syncStatus from '../src/lib/api/__fixtures__/sync-status.json' with { type: 'json' }
import typeList from '../src/lib/api/__fixtures__/type-list.json' with { type: 'json' }

/** The imported freeze JSONs, exposed so the vitest probe can prove the served
 * body is the SAME object as the on-disk fixture (import, not hand-copy). */
export const issueFreeze = {
  board,
  commentCreate,
  issueComments,
  issueDetail,
  issueList,
  issueMutate,
  projectList,
  syncStatus,
  typeList,
} as const

/** One mock result: an HTTP status + the JSON body page.route fulfills with. */
export interface MockResult {
  status: number
  json: unknown
}

/** Extra dispatch context threaded from seedSession (wave U05): the request
 * query string (list filters + keyset cursor) and the declarative seed state /
 * empty flag, so the issue-list endpoint can serve the 10k scale corpus, the
 * search-mode Top-N, an empty list, or the freeze default. Optional so the
 * vitest namespace pins (issue-fixtures.test.ts) keep calling workflowMock with
 * two args and get the freeze default. */
export interface WorkflowMockOpts {
  /** location.search of the request (e.g. '?q=bug&status=open&after=…'). */
  search?: string
  /** declarative seed state (design 06 §4.6) — '10k' serves the scale corpus. */
  state?: 'default' | 'empty' | 'error' | '10k' | 'board' | 'board-10k'
  /** empty-corpus variant (empty:true / state 'empty'). */
  empty?: boolean
}

// ---- issue-list scale + search generators (design 04 §5.5/§6.1, wave U05) ----
// Synthetic corpus so the 10k JSON never lives in the repo (mirrors the blocks
// scale generator, fixtures.ts §4.6). The DOM-cap gate needs the MODEL to hold
// far more rows than the DOM renders, so the 10k scale serves the whole corpus
// in ONE page (cursor null): virtua keeps the DOM < 200 while the model holds
// 10k — remove the windowing and the page renders 10k rows (the RED state).

const SCALE_TOTAL = 10_000
const ISSUE_SCALE_STATUSES = ['open', 'in_progress', 'review', 'blocked', 'done']

/** Row 0 carries an XSS-shaped title — the §5.5 "XSS-Titel bleibt Text" proof:
 * the list renders titles via text interpolation (Svelte-escaped), so this stays
 * literal text and never executes. */
function scaleIssueTitle(i: number): string {
  if (i === 0) return `<script>alert('xss-issue')</script> Injected title`
  return `Scale Issue ${String(i).padStart(5, '0')}`
}

function scaleIssueRow(i: number): Record<string, unknown> {
  return {
    id: `11111111-1111-1111-1111-${String(i).padStart(12, '0')}`,
    scope: 'acme:main',
    type_name: 'issue',
    title: scaleIssueTitle(i),
    workflow_status: ISSUE_SCALE_STATUSES[i % ISSUE_SCALE_STATUSES.length],
    updated_at: new Date(Date.UTC(2026, 6, 3, 12, 0, 0) - i * 60_000).toISOString(),
  }
}

/** One issue-list page for the scale corpus. Honours ?limit (clamp ≤ 100) and
 * the opaque ?after cursor (base64 of the next index); with NO limit it returns
 * the whole corpus in one page (the DOM-cap probe) — cursor null. A `state`
 * filter narrows to that workflow status. */
function scaleIssueList(sp: URLSearchParams): Record<string, unknown> {
  const state = sp.get('state')?.trim() ?? ''
  const limitRaw = sp.get('limit')
  const after = sp.get('after')
  // Opaque keyset cursor (client never inspects it): 'idx-<n>' of the last row.
  const start = after ? parseInt(after.replace(/^idx-/, ''), 10) + 1 : 0
  const pageSize = limitRaw ? Math.max(1, Math.min(parseInt(limitRaw, 10) || 0, 100)) : SCALE_TOTAL
  const issues: Record<string, unknown>[] = []
  let i = start
  while (issues.length < pageSize && i < SCALE_TOTAL) {
    const r = scaleIssueRow(i)
    if (state === '' || r.workflow_status === state) issues.push(r)
    i += 1
  }
  const cursor = i < SCALE_TOTAL ? `idx-${i - 1}` : null
  return { success: true, render: 'untrusted', issues, cursor }
}

/** Search-mode Top-N: ranked, cursor ALWAYS null (rank order is not keyset-
 * paginable, §6.1) — the client renders no load-more affordance. */
function searchIssueList(q: string): Record<string, unknown> {
  const issues = Array.from({ length: 12 }, (_, k) => ({
    ...scaleIssueRow(k),
    title: `${q} — match ${k}`,
  }))
  return { success: true, render: 'untrusted', issues, cursor: null }
}

function emptyIssueList(): Record<string, unknown> {
  return { success: true, render: 'untrusted', issues: [], cursor: null }
}

// ---- board scenarios (design 04 §4.2/§6.2, wave U07) -------------------------
// The freeze board.json is a single golden shape (open+closed, TestContract-
// FreezeGolden-pinned — untouchable here). The board GATES need richer worlds:
// a multi-column board in a KNOWN order with a terminal (collapsed) column, plus
// a 10k×6 scale board — so these are GENERATED (never the golden file). The
// registry drives open/closed/unmapped, so the board seeds ALSO override
// GET /api/types with a workflow config the freeze type-list.json lacks (states
// + terminal). Kept out of the golden files entirely: the golden stays pinned,
// the scenario data lives here + in per-test page.route overrides.

/** Default board column order (== the wire order the board must preserve, §5.5).
 * `done` is terminal (the registry marks it closed → the column starts
 * collapsed). */
const BOARD_STATUSES = ['open', 'in_progress', 'review', 'done']
/** Scale board: 6 columns so the 10k×6 DOM-ceiling probe (§5.5) has its width. */
const BOARD_SCALE_STATUSES = ['open', 'in_progress', 'review', 'blocked', 'staged', 'done']
const BOARD_TERMINAL = ['done']

function boardCard(status: string, i: number): Record<string, unknown> {
  return {
    id: `33333333-3333-3333-3333-${String(i).padStart(12, '0')}`,
    scope: 'acme:main',
    type_name: 'issue',
    title: `${status} card ${i}`,
    workflow_status: status,
    updated_at: new Date(Date.UTC(2026, 6, 3, 12, 0, 0) - i * 60_000).toISOString(),
  }
}

/** GET /api/types override for the board seeds: the issue type WITH a workflow
 * config (states + terminal), the policy the board reads for open/closed. */
function boardTypes(states: string[]): Record<string, unknown> {
  return {
    success: true,
    types: [
      {
        id: '77777777-7777-7777-7777-777777777777',
        name: 'issue',
        scope: '_global',
        display_name: 'Issue',
        description: 'A tracked work item',
        builtin: true,
        is_default: false,
        source: 'builtin',
        created_at: '2026-07-03T00:00:00Z',
        updated_at: '2026-07-03T00:00:00Z',
        config: {
          v: 1,
          retrieval: { policy: 'full-pass' },
          parent: { mode: 'none' },
          workflow: { states, initial: states[0], terminal: BOARD_TERMINAL },
        },
      },
    ],
  }
}

/** Default board: four columns in a fixed order, small wire counts, full first
 * pages (cursor null). `done` (terminal) carries count 12 but starts collapsed —
 * the closed-collapse baseline + the count-from-wire proof. */
function boardScenario(): Record<string, unknown> {
  const counts: Record<string, number> = { open: 3, in_progress: 2, review: 1, done: 12 }
  const columns = BOARD_STATUSES.map((status) => {
    const shown = status === 'done' ? 5 : counts[status]
    return {
      status,
      count: counts[status],
      cursor: null,
      issues: Array.from({ length: shown }, (_, k) => boardCard(status, k)),
    }
  })
  return { success: true, render: 'untrusted', columns }
}

/** Scale board: 6 columns × count 10 000, each a bounded 30-card first page with
 * a resume cursor. The count is the WIRE total (10 000), the loaded page tiny —
 * so the board shows "30 of 10 000" and the per-column windowing keeps the DOM
 * bounded (< 300 cards across the open columns; `done` collapsed renders zero). */
function boardScenarioScale(): Record<string, unknown> {
  const columns = BOARD_SCALE_STATUSES.map((status) => ({
    status,
    count: 10_000,
    cursor: 'idx-29',
    issues: Array.from({ length: 30 }, (_, k) => boardCard(status, k)),
  }))
  return { success: true, render: 'untrusted', columns }
}

function emptyBoard(): Record<string, unknown> {
  return { success: true, render: 'untrusted', columns: [] }
}

/** A per-column keyset page for the scale board's load-more (state-filtered list
 * endpoint). Returns one more 30-card page then ends (cursor null) — so an
 * accidental scroll-append terminates cleanly instead of looping. */
function boardColumnPage(sp: URLSearchParams): Record<string, unknown> {
  const state = sp.get('state')?.trim() ?? 'open'
  const after = sp.get('after')
  const start = after ? parseInt(after.replace(/^idx-/, ''), 10) + 1 : 0
  const issues = Array.from({ length: 30 }, (_, k) => boardCard(state, start + k))
  return { success: true, render: 'untrusted', issues, cursor: null }
}

// ---- detail scale: the 500-comment thread (design 04 §5.5, wave U06) --------
// The state '10k' seed serves a detail with 500 inline comments so the render-
// budget proof (progressive-reveal cap keeps the rendered thread bounded while
// the model holds all 500) has a fixture. cursor null = the whole thread in one
// inline page.

const SCALE_COMMENTS = 500

function scaleComment(i: number): Record<string, unknown> {
  const at = new Date(Date.UTC(2026, 6, 1, 0, 0, 0) + i * 60_000).toISOString()
  return {
    id: `22222222-2222-2222-2222-${String(i).padStart(12, '0')}`,
    category: 'comment',
    content: `Scale comment ${i} — thread render-budget probe (design 04 §5.5).`,
    created_at: at,
    lifecycle_state: 'active',
    metadata: {},
    scope: 'acme:main',
    sensitivity: 'internal',
    sensitivity_source: 'auto',
    tags: [],
    title: '',
    type: 'comment',
    type_source: 'manual',
    updated_at: at,
  }
}

function scaleIssueDetail(): Record<string, unknown> {
  return {
    success: true,
    render: 'untrusted',
    issue: (issueDetail as { issue: unknown }).issue,
    comments: Array.from({ length: SCALE_COMMENTS }, (_, k) => scaleComment(k)),
    comments_cursor: null,
  }
}

/**
 * A distinct, LOUD hard-fail for an un-mocked path INSIDE the workflow namespace.
 * Kept separate from the global 599 catch-all so a namespace miss is diagnosable
 * as such — and it must NEVER degrade to a benign {success:true} (the closed N3
 * trap, e2e-playwright.md N3). 599 is non-2xx ⇒ apiFetch throws (api.ts) loudly.
 */
function namespaceHardFail(method: string, path: string): MockResult {
  return {
    status: 599,
    json: { __unmocked: true, success: false, error: `unmocked workflow endpoint: ${method} ${path}` },
  }
}

/**
 * Dispatch a workflow-namespace request to its freeze-JSON body.
 *  - returns `null` when `path` is OUTSIDE the namespace (the caller falls
 *    through to the generic /api/** handler);
 *  - returns a 200 + freeze-JSON body for a mocked endpoint;
 *  - returns the namespace hard-fail for an un-mocked path INSIDE the namespace.
 */
export function workflowMock(method: string, path: string, opts: WorkflowMockOpts = {}): MockResult | null {
  const inTypes = path === TYPES_BASE || path.startsWith(`${TYPES_BASE}/`)
  const inIssues = path === ISSUES_BASE || path.startsWith(`${ISSUES_BASE}/`)
  if (!inTypes && !inIssues) return null

  // --- /api/types[/{name}] ---
  if (inTypes) {
    if (path === TYPES_BASE && method === 'GET') {
      // Board seeds (U07) need the workflow config the freeze type-list.json
      // lacks; they override the registry read with a states+terminal set.
      if (opts.state === 'board') return { status: 200, json: boardTypes(BOARD_STATUSES) }
      if (opts.state === 'board-10k') return { status: 200, json: boardTypes(BOARD_SCALE_STATUSES) }
      return { status: 200, json: typeList }
    }
    if (path.startsWith(`${TYPES_BASE}/`)) {
      const name = decodeURIComponent(path.slice(`${TYPES_BASE}/`.length))
      const first = (typeList as { types: Array<{ name: string; builtin: boolean }> }).types[0]
      if (method === 'GET') return { status: 200, json: { success: true, type: first } }
      // PUT upsert (U10): echo a success type envelope with the URL name. The
      // 422-draft mechanic is proven on the pure form logic (vitest) + a per-test
      // page.route override (admin-types.spec.ts) — a body-blind namespace mock
      // stays deterministic, it does not branch on the payload.
      if (method === 'PUT') return { status: 200, json: { success: true, type: { ...first, name } } }
      // DELETE (U10): the builtin '_global' seed is operator-protected — the
      // server answers 409 ErrBlockTypeBuiltin (the UI also disables the control;
      // double-layer, §4.7). Any custom (non-builtin) name deletes cleanly.
      if (method === 'DELETE') {
        if (name === first.name && first.builtin) {
          return { status: 409, json: { success: false, error: 'builtin block types cannot be deleted' } }
        }
        return { status: 200, json: { success: true, deleted: name } }
      }
    }
    return namespaceHardFail(method, path)
  }

  // --- /api/project family --- strip the base, split the remainder.
  const rest = path === ISSUES_BASE ? '' : path.slice(`${ISSUES_BASE}/`.length)
  const seg = rest === '' ? [] : rest.split('/')

  // GET /api/project  (register list / picker source)
  if (seg.length === 0) {
    if (method === 'GET') return { status: 200, json: projectList }
    return namespaceHardFail(method, path)
  }
  // GET /api/project/{id}  (single project)
  if (seg.length === 1) {
    if (method === 'GET') {
      const first = (projectList as { projects: unknown[] }).projects[0]
      return { status: 200, json: { success: true, project: first } }
    }
    return namespaceHardFail(method, path)
  }

  const sub = seg[1]
  // /api/project/{id}/board
  if (sub === 'board' && seg.length === 2 && method === 'GET') {
    if (opts.empty || opts.state === 'empty') return { status: 200, json: emptyBoard() }
    if (opts.state === 'board') return { status: 200, json: boardScenario() }
    if (opts.state === 'board-10k') return { status: 200, json: boardScenarioScale() }
    return { status: 200, json: board }
  }
  // /api/project/{id}/sync
  if (sub === 'sync' && seg.length === 2) {
    if (method === 'GET') return { status: 200, json: syncStatus }
    if (method === 'POST') return { status: 200, json: { success: true, run: (syncStatus as { run: unknown }).run } }
    return namespaceHardFail(method, path)
  }
  // /api/project/{id}/issues*
  if (sub === 'issues') {
    if (seg.length === 2) {
      if (method === 'GET') {
        const sp = new URLSearchParams(opts.search ?? '')
        const q = sp.get('q')?.trim()
        // Search mode wins (cursor null), then empty, then the scale corpus,
        // else the freeze default (the vitest pins call with no opts → freeze).
        if (q) return { status: 200, json: searchIssueList(q) }
        if (opts.empty || opts.state === 'empty') return { status: 200, json: emptyIssueList() }
        if (opts.state === '10k') return { status: 200, json: scaleIssueList(sp) }
        // Board scale: a column keyset-appends via the state-filtered list.
        if (opts.state === 'board-10k') return { status: 200, json: boardColumnPage(sp) }
        return { status: 200, json: issueList }
      }
      if (method === 'POST') return { status: 200, json: issueMutate }
      return namespaceHardFail(method, path)
    }
    if (seg.length === 3) {
      if (method === 'GET') {
        if (opts.state === '10k') return { status: 200, json: scaleIssueDetail() }
        return { status: 200, json: issueDetail }
      }
      if (method === 'PATCH') return { status: 200, json: issueMutate }
      return namespaceHardFail(method, path)
    }
    if (seg.length === 4 && seg[3] === 'comments') {
      if (method === 'GET') return { status: 200, json: issueComments }
      if (method === 'POST') return { status: 200, json: commentCreate }
      return namespaceHardFail(method, path)
    }
  }
  return namespaceHardFail(method, path)
}
