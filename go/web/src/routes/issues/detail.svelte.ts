// /issues/:id detail state (design 04 §4.1/§4.5/§5.5, wave U06). Injectable
// runes class (IssuesModel pattern) so the load / 404 / paging / mutation logic
// is vitest-covered without a DOM.
//
// 404-uniform (§5.5): a foreign / unknown block id answers the uniform 404 shape
// (the server resolves ids in the caller's read scopes only). The model maps a
// 404 to a NOT-FOUND state (EmptyState), NOT the error band and NEVER a redirect
// loop — status stays 'ready' so the page renders the empty affordance.
//
// Comment thread (§7 line 370: Keyset a 50, virtualisiert ab > 100): the thread
// loads a keyset page at a time (ASC). Render budget is bounded by a PROGRESSIVE
// REVEAL cap, not windowed virtualization: comment bodies are variable-height
// markdown, so the fixed-height window used for the flat issue LIST does not
// apply here — a height model would be guesswork. The cap renders the first N,
// a "show more" reveals the next N; combined with 50-row keyset paging the normal
// load stays small, and the 500-comment probe stays bedienbar (the surface never
// renders 500 markdown bodies at once).
//
// Status/title mutations are PESSIMISTIC: the server verdict is authoritative, so
// a policy-violating transition (422) rejects and the caller (ConfirmDialog)
// keeps the selection + shows the message (§4.5). No optimistic flip to roll back
// on the detail surface — the optimistic path lives on the board (U08).

import { toApiError, type ApiError } from '../../lib/api'
import {
  createComment as createCommentApi,
  getIssue as getIssueApi,
  listComments as listCommentsApi,
  patchIssue as patchIssueApi,
  type CommentCreateBody,
  type CommentListParams,
  type IssuePatchBody,
} from '../../lib/api/issues'
import type {
  CommentCreateResponse,
  IssueBlock,
  IssueCommentsResponse,
  IssueCursor,
  IssueDetailResponse,
  IssueMutateResponse,
} from '../../lib/api/types'
import type { ResourceStatus } from '../../lib/resource.svelte'

/** Keyset comment page size (design 04 §7 line 370). */
export const COMMENT_PAGE_LIMIT = 50
/** How many comment bodies to render at once (progressive-reveal budget). */
export const COMMENT_RENDER_CAP = 100

/** The API surface the model needs, injectable for DOM-free tests. */
export interface DetailApi {
  getIssue: (projectId: string, blockId: string) => Promise<IssueDetailResponse>
  listComments: (
    projectId: string,
    blockId: string,
    params?: CommentListParams,
  ) => Promise<IssueCommentsResponse>
  patchIssue: (projectId: string, blockId: string, body: IssuePatchBody) => Promise<IssueMutateResponse>
  createComment: (projectId: string, blockId: string, body: CommentCreateBody) => Promise<CommentCreateResponse>
}

const DEFAULT_API: DetailApi = {
  getIssue: getIssueApi,
  listComments: listCommentsApi,
  patchIssue: patchIssueApi,
  createComment: createCommentApi,
}

export class IssueDetailModel {
  issue = $state<IssueBlock | null>(null)
  comments = $state<IssueBlock[]>([])
  commentsCursor = $state<IssueCursor>(null)
  status = $state<ResourceStatus>('idle')
  loadError = $state<ApiError | null>(null)
  /** True when the id resolved to no visible block (uniform 404) — EmptyState. */
  notFound = $state(false)
  /** A status/title patch is in flight. */
  mutating = $state(false)
  /** A comment create is in flight. */
  posting = $state(false)
  /** A comment keyset page is in flight. */
  loadingMore = $state(false)
  /** How many of the loaded comments are rendered (progressive reveal). */
  revealed = $state(COMMENT_RENDER_CAP)

  readonly #projectId: string
  readonly #blockId: string
  #api: DetailApi

  constructor(projectId: string, blockId: string, api: DetailApi = DEFAULT_API) {
    this.#projectId = projectId
    this.#blockId = blockId
    this.#api = api
  }

  /** More comment pages exist on the server. */
  get canLoadMoreComments(): boolean {
    return this.commentsCursor !== null
  }

  /** The comments actually rendered (the reveal window). */
  get visibleComments(): IssueBlock[] {
    return this.comments.slice(0, this.revealed)
  }

  /** Loaded-but-not-yet-rendered comments exist (the "show more" affordance). */
  get hasHiddenComments(): boolean {
    return this.comments.length > this.revealed
  }

  /** Load the detail + the first inline comment page. */
  async load(): Promise<void> {
    this.status = 'loading'
    this.loadError = null
    this.notFound = false
    try {
      const res = await this.#api.getIssue(this.#projectId, this.#blockId)
      this.issue = res.issue
      this.comments = res.comments ?? []
      this.commentsCursor = res.comments_cursor ?? null
      this.revealed = COMMENT_RENDER_CAP
      this.status = 'ready'
    } catch (err) {
      const e = toApiError(err)
      if (e.status === 404) {
        // Uniform 404: render the EmptyState, not the error band, never redirect.
        this.notFound = true
        this.issue = null
        this.status = 'ready'
      } else {
        this.loadError = e
        this.status = 'error'
      }
    }
  }

  /** Reveal the next batch of already-loaded comments (render-budget release). */
  revealMore(): void {
    this.revealed += COMMENT_RENDER_CAP
  }

  /** Append the next keyset comment page (ASC). No-op past the last page. */
  async loadMoreComments(): Promise<void> {
    if (!this.canLoadMoreComments || this.loadingMore) return
    this.loadingMore = true
    this.loadError = null
    try {
      const res = await this.#api.listComments(this.#projectId, this.#blockId, {
        after: this.commentsCursor ?? undefined,
        limit: COMMENT_PAGE_LIMIT,
      })
      this.comments = [...this.comments, ...res.comments]
      this.commentsCursor = res.cursor ?? null
    } catch (err) {
      this.loadError = toApiError(err)
    } finally {
      this.loadingMore = false
    }
  }

  /**
   * Change the workflow status. Pessimistic: on success the server's issue
   * replaces the local one; on failure (422 policy violation) the promise
   * REJECTS so the ConfirmDialog stays open, shows the message and keeps the
   * selection (§4.5). The server is always the authority.
   */
  async changeStatus(status: string): Promise<void> {
    this.mutating = true
    try {
      const res = await this.#api.patchIssue(this.#projectId, this.#blockId, { status })
      this.issue = res.issue
    } finally {
      this.mutating = false
    }
  }

  /** Edit the title (only reached when writable). Rejects on error (same as status). */
  async changeTitle(title: string): Promise<void> {
    this.mutating = true
    try {
      const res = await this.#api.patchIssue(this.#projectId, this.#blockId, { title })
      this.issue = res.issue
    } finally {
      this.mutating = false
    }
  }

  /**
   * Post a comment. On success the created comment is appended to the thread and
   * revealed; on error the promise REJECTS so the composer keeps its draft and
   * surfaces the message. Rejects an empty/blank body without a round-trip.
   */
  async addComment(content: string): Promise<void> {
    const body = content.trim()
    if (body === '') return
    this.posting = true
    try {
      const res = await this.#api.createComment(this.#projectId, this.#blockId, { content: body })
      this.comments = [...this.comments, res.comment]
      // Keep the freshly posted comment visible even past the reveal cap.
      if (this.comments.length > this.revealed) this.revealed = this.comments.length
    } finally {
      this.posting = false
    }
  }
}
