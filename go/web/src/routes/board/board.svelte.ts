// /board state (design 04 §4.2/§6.2, wave U07). Injectable runes class (pool/
// blocks pattern) so the classification + collapse + per-column window logic is
// vitest-covered without a DOM.
//
// TWO reads, ONE model:
//   - GET /api/project/{id}/board  → per-status columns (status/count/first page/
//     opaque per-column cursor). The column SET + ORDER come from the wire
//     (server-derived from the type-config status set — never hardcoded here).
//   - GET /api/types               → the registry workflow config; the board wire
//     has no category, so the open/closed/unmapped verdict + the closed-collapse
//     come from config.workflow.{states,terminal} (joined in board-columns.ts).
//
// Read-only in U07 — the drag-and-drop status transition is U08. Scale (10k+/
// column): the board loads only a FIRST page per column; a column keyset-appends
// via the list endpoint (state = the column status, after = the column cursor).
// The count shown is always the wire count (B7 aggregate), NEVER the loaded card
// length — a column can show "12 of 10 000" with 12 hydrated cards.

import { toApiError, type ApiError } from '../../lib/api'
import {
  getBoard as getBoardApi,
  listIssues as listIssuesApi,
  type BoardParams,
  type IssueListParams,
} from '../../lib/api/issues'
import { listTypes as listTypesApi } from '../../lib/api/types-registry'
import type { BoardResponse, IssueListResponse, TypesListResponse } from '../../lib/api/types'
import type { ResourceStatus } from '../../lib/resource.svelte'
import { classifyColumns, initialCollapsed, vocabFromTypes, type ClassifiedColumn } from './board-columns'

/** Per-column loaded-card ceiling (§6.2 scale guard) — the keyset append stops
 * here so one very hot column cannot grow the data model without bound; the
 * footer then shows "N of count" honestly (the count stays the wire total). */
export const BOARD_COLUMN_CAP = 1000

/** The API surface the board depends on — injectable for DOM-free tests. */
export interface BoardApi {
  getBoard: (projectId: string, params?: BoardParams) => Promise<BoardResponse>
  listTypes: () => Promise<TypesListResponse>
  listIssues: (projectId: string, params?: IssueListParams) => Promise<IssueListResponse>
}

const defaultApi: BoardApi = { getBoard: getBoardApi, listTypes: listTypesApi, listIssues: listIssuesApi }

export class BoardModel {
  /** Classified columns in WIRE order (board-columns preserves it). */
  columns = $state<ClassifiedColumn[]>([])
  status = $state<ResourceStatus>('idle')
  loadError = $state<ApiError | null>(null)
  /** statusId → collapsed? (closed columns start true, §6.2). */
  collapsed = $state<Record<string, boolean>>({})
  /** statusId → an append is in flight (per-column, so one hot column does not
   * block the others). */
  loadingMore = $state<Record<string, boolean>>({})

  readonly #projectId: string
  #api: BoardApi

  constructor(projectId: string, api: BoardApi = defaultApi) {
    this.#projectId = projectId
    this.#api = api
  }

  /** Fetch the board + the registry in parallel, classify, seed the collapse
   * set. Registry failure fails the whole load closed (§5.3: the board never
   * renders with a guessed category) — both reads must succeed. */
  async load(): Promise<void> {
    this.status = 'loading'
    this.loadError = null
    try {
      const [board, types] = await Promise.all([this.#api.getBoard(this.#projectId), this.#api.listTypes()])
      const vocab = vocabFromTypes(types.types)
      this.columns = classifyColumns(board.columns, vocab)
      const collapsed: Record<string, boolean> = {}
      for (const s of initialCollapsed(this.columns)) collapsed[s] = true
      this.collapsed = collapsed
      this.loadingMore = {}
      this.status = 'ready'
    } catch (err) {
      this.loadError = toApiError(err)
      this.status = 'error'
    }
  }

  isCollapsed(status: string): boolean {
    return this.collapsed[status] === true
  }

  /** Toggle a column's collapse (reassign for rune reactivity). */
  toggle(status: string): void {
    this.collapsed = { ...this.collapsed, [status]: !this.collapsed[status] }
  }

  /** A column offers "load more" while the wire handed back a next cursor, no
   * append is in flight, and the loaded set is below the cap. */
  canLoadMore(status: string): boolean {
    const col = this.columns.find((c) => c.status === status)
    if (col === undefined || col.cursor === null || this.loadingMore[status] === true) return false
    return col.issues.length < BOARD_COLUMN_CAP
  }

  /** Append the next keyset page of ONE column (state-filtered list, §6.2). The
   * board cursor feeds ?after= (opaque, never parsed); an append error keeps the
   * cards already shown. */
  async loadMore(status: string): Promise<void> {
    if (!this.canLoadMore(status)) return
    const col = this.columns.find((c) => c.status === status)
    if (col === undefined) return
    this.loadingMore = { ...this.loadingMore, [status]: true }
    this.loadError = null
    try {
      const res = await this.#api.listIssues(this.#projectId, { state: status, after: col.cursor ?? undefined })
      this.columns = this.columns.map((c) => {
        if (c.status !== status) return c
        const issues = [...c.issues, ...res.issues]
        // Stop paging past the ceiling (pin cursor null so canLoadMore closes).
        const cursor = issues.length >= BOARD_COLUMN_CAP ? null : (res.cursor ?? null)
        return { ...c, issues, cursor }
      })
    } catch (err) {
      this.loadError = toApiError(err)
    } finally {
      this.loadingMore = { ...this.loadingMore, [status]: false }
    }
  }
}
