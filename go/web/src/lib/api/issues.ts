// Workflow-UI API client (design/04 §3.2 / U03). The endpoint family is the
// /api/project/{id}/issues|board|sync style — K04-A is decided by the shipped
// W6 (read) / W7 (write) / W11 (sync) / W4 (register) handlers.
//
// ISSUES_BASE is the SINGLE source of the path prefix: every client function AND
// the e2e fixture namespace matcher (e2e/fixtures.ts) import it, so the
// Fixture-Hard-Fail-Namespace can never drift from the endpoint form (U03 gate,
// design §3.2). There is no second hand-written '/api/project' string anywhere.
//
// Scale (10k+ issues/repo): the list + board + comment reads are keyset-only —
// an opaque `after` cursor + a limit clamp, NEVER offset paging.

import { apiFetch } from '../api'
import type {
  BoardResponse,
  CommentCreateResponse,
  IssueCommentsResponse,
  IssueDetailResponse,
  IssueListResponse,
  IssueMutateResponse,
  ProjectListResponse,
  ProjectResponse,
  SyncStartResponse,
  SyncStatusResponse,
} from './types'

/**
 * The single path prefix of the whole workflow surface (project register +
 * issues + board + sync). EVERY workflow path is built from this constant;
 * mutating it moves every client call AND the e2e fixture namespace in lockstep
 * (issues.test.ts + meta.spec.ts pin exactly that coupling).
 */
export const ISSUES_BASE = '/api/project'

/**
 * Server list/board page ceiling (MaxWorkflowListLimit, store/workflow.go). The
 * server hard-clamps; the client mirrors it so a page never asks past the wall.
 */
export const MAX_ISSUE_LIMIT = 100

/** Closed list-order set (store.SortUpdated | store.SortCreated). */
export type IssueSort = 'updated' | 'created'

/** GET /api/project/{id}/issues query (project_issues.go parseListParams). All
 * optional; `after` is the opaque cursor from a previous page's `cursor`. */
export interface IssueListParams {
  /** one workflow status (board column) — empty = the full per-status merge. */
  state?: string
  /** label AND-filter (repeated ?labels=, server splits comma too). */
  labels?: string[]
  /** free-text → server RRF/FTS path; then `cursor` is ALWAYS null. */
  q?: string
  sort?: IssueSort
  limit?: number
  after?: string
}

/** GET …/comments query (ASC keyset). */
export interface CommentListParams {
  after?: string
  limit?: number
}

/** GET /api/project/{id}/board query. */
export interface BoardParams {
  labels?: string[]
  limit?: number
}

/** POST /api/project/{id}/issues body (restIssueCreate). */
export interface IssueCreateBody {
  title: string
  content?: string
  tags?: string[]
  metadata?: Record<string, unknown>
  status?: string
}

/** PATCH /api/project/{id}/issues/{block_id} body (restIssueUpdate). At least one
 * field required (the server 400s an all-empty patch). */
export interface IssuePatchBody {
  title?: string
  content?: string
  tags?: string[]
  metadata?: Record<string, unknown>
  status?: string
}

/** POST …/comments body (restCommentCreate). */
export interface CommentCreateBody {
  author?: string
  content?: string
  metadata?: Record<string, unknown>
}

/** Build a query string (leading '?') from defined params; '' when none apply.
 * Arrays expand to repeated keys; undefined/empty values are dropped. */
function qs(params: Record<string, string | number | string[] | undefined>): string {
  const sp = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined) continue
    if (Array.isArray(value)) {
      for (const v of value) if (v !== '') sp.append(key, v)
    } else if (value !== '') {
      sp.append(key, String(value))
    }
  }
  const s = sp.toString()
  return s ? `?${s}` : ''
}

/** Path prefix for one project's issue surface. seg-encodes the id (UUID, but
 * encoded defensively so a malformed id can never break out of the path). */
function projectPath(projectId: string): string {
  return `${ISSUES_BASE}/${encodeURIComponent(projectId)}`
}

// --- W4 register (Projekt-Picker-Quelle, §4.1.5) ---

/** GET /api/project — projects visible to the caller (ReadScopes ∩). An optional
 * identity narrows to the single project of that identity (init existence probe). */
export function listProjects(identity?: string): Promise<ProjectListResponse> {
  return apiFetch<ProjectListResponse>(`${ISSUES_BASE}${qs({ identity })}`)
}

/** GET /api/project/{id} — one project (member scope-read; 404 uniform otherwise). */
export function getProject(projectId: string): Promise<ProjectResponse> {
  return apiFetch<ProjectResponse>(projectPath(projectId))
}

// --- W6 reads ---

/** GET /api/project/{id}/issues — one keyset page (state/labels/q/sort). */
export function listIssues(projectId: string, params: IssueListParams = {}): Promise<IssueListResponse> {
  const query = qs({
    state: params.state,
    labels: params.labels,
    q: params.q,
    sort: params.sort,
    limit: params.limit,
    after: params.after,
  })
  return apiFetch<IssueListResponse>(`${projectPath(projectId)}/issues${query}`)
}

/** GET /api/project/{id}/issues/{block_id} — detail + first inline comment page. */
export function getIssue(projectId: string, blockId: string): Promise<IssueDetailResponse> {
  return apiFetch<IssueDetailResponse>(`${projectPath(projectId)}/issues/${encodeURIComponent(blockId)}`)
}

/** GET /api/project/{id}/issues/{block_id}/comments — ASC keyset thread. */
export function listComments(
  projectId: string,
  blockId: string,
  params: CommentListParams = {},
): Promise<IssueCommentsResponse> {
  const query = qs({ after: params.after, limit: params.limit })
  return apiFetch<IssueCommentsResponse>(
    `${projectPath(projectId)}/issues/${encodeURIComponent(blockId)}/comments${query}`,
  )
}

/** GET /api/project/{id}/board — config-status columns + counts + per-column page. */
export function getBoard(projectId: string, params: BoardParams = {}): Promise<BoardResponse> {
  const query = qs({ labels: params.labels, limit: params.limit })
  return apiFetch<BoardResponse>(`${projectPath(projectId)}/board${query}`)
}

// --- W7 writes ---

/** POST /api/project/{id}/issues — create a local issue. */
export function createIssue(projectId: string, body: IssueCreateBody): Promise<IssueMutateResponse> {
  return apiFetch<IssueMutateResponse>(`${projectPath(projectId)}/issues`, {
    method: 'POST',
    body: JSON.stringify(body),
  })
}

/** PATCH /api/project/{id}/issues/{block_id} — field + status mutation (an
 * out-of-policy status transition is a 422 → ApiError). */
export function patchIssue(
  projectId: string,
  blockId: string,
  body: IssuePatchBody,
): Promise<IssueMutateResponse> {
  return apiFetch<IssueMutateResponse>(`${projectPath(projectId)}/issues/${encodeURIComponent(blockId)}`, {
    method: 'PATCH',
    body: JSON.stringify(body),
  })
}

/** POST /api/project/{id}/issues/{block_id}/comments — add a comment. */
export function createComment(
  projectId: string,
  blockId: string,
  body: CommentCreateBody,
): Promise<CommentCreateResponse> {
  return apiFetch<CommentCreateResponse>(
    `${projectPath(projectId)}/issues/${encodeURIComponent(blockId)}/comments`,
    { method: 'POST', body: JSON.stringify(body) },
  )
}

// --- W11 sync ---

/** GET /api/project/{id}/sync — merged run state + DB history (member read). */
export function getSyncStatus(projectId: string): Promise<SyncStatusResponse> {
  return apiFetch<SyncStatusResponse>(`${projectPath(projectId)}/sync`)
}

/** POST /api/project/{id}/sync — start a sync run (write-scope gated). */
export function startSync(projectId: string, opts: { dryRun?: boolean } = {}): Promise<SyncStartResponse> {
  const query = qs({ dry_run: opts.dryRun ? 'true' : undefined })
  return apiFetch<SyncStartResponse>(`${projectPath(projectId)}/sync${query}`, { method: 'POST' })
}
