// /issues list state (design 04 §4.2, wave U05). Injectable runes class (pool/
// blocks pattern) so the two-mode logic is vitest-covered without a DOM.
//
// Memory model (§6.1): holds ALL loaded summaries — no front-eviction. virtua-
// style windowing virtualises the DOM (virtual-window.ts), NOT the data, so
// there is no index shift and the scroll geometry stays stable. A HARD cap of
// 50 000 rows stops the keyset append (footer hint "refine the filter"); 50k
// slim rows ≈ 15-25 MB, a bounded ceiling, not a leak.
//
// Two modes, mirrors of B1 (§3.2, IST W6 handler):
//   - BROWSE (q empty): updated_at DESC keyset, Load-more appends, cursor
//     drives the next page.
//   - SEARCH (q set):   the server RRF/FTS path returns cursor === null ALWAYS
//     (rank order is not keyset-paginable, context_search.go:110-114). The
//     model refuses append UI in search mode STRUCTURALLY — canLoadMore is
//     false regardless of the cursor value, so a server that (wrongly) handed
//     back a cursor could not resurrect the infinite-scroll affordance. That is
//     the "erst rot gegen eine Liste, die im Such-Modus Append-UI rendert"-gate.

import { toApiError, type ApiError } from '../../lib/api'
import { listIssues as listIssuesApi, type IssueListParams } from '../../lib/api/issues'
import type { IssueCursor, IssueListResponse, IssueRow } from '../../lib/api/types'
import type { ResourceStatus } from '../../lib/resource.svelte'

/** Hard ceiling on loaded rows (§4.2/§6.1) — the append stops here. */
export const ISSUE_ROW_CAP = 50_000

/** The one API dependency, injectable for DOM-free tests. */
export interface IssuesApi {
  listIssues: (projectId: string, params?: IssueListParams) => Promise<IssueListResponse>
}

/** The server-filter slice the list fetch sends (URL filters minus scope/type,
 * which the project id and the client-side narrowing carry — the W6 list
 * endpoint has no `type` param, api/issues.ts). */
export interface IssueQuery {
  state?: string
  labels?: string[]
  q?: string
}

export class IssuesModel {
  /** All loaded summaries (browse: appended pages; search: the Top-N set). */
  rows = $state<IssueRow[]>([])
  status = $state<ResourceStatus>('idle')
  loadError = $state<ApiError | null>(null)
  /** Next-page cursor (browse) — null on the last page AND always in search mode. */
  cursor = $state<IssueCursor>(null)
  loadingMore = $state(false)
  /** True once the 50k cap is hit — the append stops, the footer explains why. */
  capped = $state(false)

  readonly #projectId: string
  #api: IssuesApi
  /** The current query — remembered so loadMore re-issues it with the cursor. */
  #query: IssueQuery = {}

  constructor(projectId: string, api: IssuesApi = { listIssues: listIssuesApi }) {
    this.#projectId = projectId
    this.#api = api
  }

  /** True when the current query is a free-text search (Top-N, no append). */
  get searchMode(): boolean {
    return typeof this.#query.q === 'string' && this.#query.q !== ''
  }

  /**
   * The "Load more" / scroll-append affordance is offered ONLY in browse mode,
   * while the server handed back a next cursor, and below the cap. Search mode
   * forces this false independent of `cursor` — the structural guard the
   * negative gate proves (append UI can never appear on a ranked result set).
   */
  get canLoadMore(): boolean {
    return !this.searchMode && this.cursor !== null && !this.capped
  }

  /** Load page 1 for a query (replaces the list). */
  async load(query: IssueQuery = {}): Promise<void> {
    this.#query = query
    this.status = 'loading'
    this.loadError = null
    this.capped = false
    try {
      const res = await this.#api.listIssues(this.#projectId, this.#paramsFor(query))
      this.rows = res.issues
      // Search mode never paginates: pin the cursor null even if the wire sent one.
      this.cursor = this.searchMode ? null : (res.cursor ?? null)
      this.status = 'ready'
    } catch (err) {
      this.loadError = toApiError(err)
      this.status = 'error'
    }
  }

  /**
   * Append the next keyset page (browse only). A no-op when there is no next
   * page, an append is in flight, or the cap is reached. Appends (never
   * replaces); adopts the response cursor; stops at null or at the cap. An
   * append error keeps the rows already shown (never blanks the list).
   */
  async loadMore(): Promise<void> {
    if (!this.canLoadMore || this.loadingMore) return
    this.loadingMore = true
    this.loadError = null
    try {
      const res = await this.#api.listIssues(this.#projectId, {
        ...this.#paramsFor(this.#query),
        after: this.cursor ?? undefined,
      })
      this.rows = [...this.rows, ...res.issues]
      this.cursor = res.cursor ?? null
      if (this.rows.length >= ISSUE_ROW_CAP) {
        this.capped = true
        this.cursor = null // stop paging past the ceiling
      }
    } catch (err) {
      this.loadError = toApiError(err)
    } finally {
      this.loadingMore = false
    }
  }

  #paramsFor(query: IssueQuery): IssueListParams {
    const params: IssueListParams = {}
    if (query.state) params.state = query.state
    if (query.labels && query.labels.length > 0) params.labels = query.labels
    if (query.q) params.q = query.q
    return params
  }
}
