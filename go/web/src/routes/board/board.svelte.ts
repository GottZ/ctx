// /board state (design 04 §4.2/§4.5/§6.2, waves U07 + U08). Injectable runes
// class (pool/blocks pattern) so the classification + collapse + per-column
// window + the OPTIMISTIC TRANSITION logic is vitest-covered without a DOM.
//
// TWO reads, ONE model:
//   - GET /api/project/{id}/board  → per-status columns (status/count/first page/
//     opaque per-column cursor). The column SET + ORDER come from the wire
//     (server-derived from the type-config status set — never hardcoded here).
//   - GET /api/types               → the registry workflow config; the board wire
//     has no category, so the open/closed/unmapped verdict + the closed-collapse
//     come from config.workflow.{states,terminal} (joined in board-columns.ts).
//
// U08 write path: transition(issueId, from, to) moves a card OPTIMISTICALLY
// (card + counts move at once, §4.5) then PATCHes the status (W7 wire). Success
// reconciles the card from the server issue; an ApiError ROLLS BACK the move and
// surfaces transitionError. A 409/422 additionally re-reads the board + registry
// (§4.8 staleness) so the columns reflect the wire truth after a policy drift.
//
// Scale (10k+/column): the board loads only a FIRST page per column; a column
// keyset-appends via the list endpoint. The count shown is always the wire count
// (B7 aggregate), NEVER the loaded card length.

import { toApiError, type ApiError } from '../../lib/api'
import {
  getBoard as getBoardApi,
  listIssues as listIssuesApi,
  patchIssue as patchIssueApi,
  type BoardParams,
  type IssueListParams,
} from '../../lib/api/issues'
import { listTypes as listTypesApi } from '../../lib/api/types-registry'
import type {
  BoardResponse,
  IssueBlock,
  IssueListResponse,
  IssueMutateResponse,
  IssueRow,
  TypesListResponse,
} from '../../lib/api/types'
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
  patchIssue: (projectId: string, blockId: string, body: { status: string }) => Promise<IssueMutateResponse>
}

const defaultApi: BoardApi = {
  getBoard: getBoardApi,
  listTypes: listTypesApi,
  listIssues: listIssuesApi,
  patchIssue: (projectId, blockId, body) => patchIssueApi(projectId, blockId, body),
}

/** Fold the server issue block back onto a board row after a confirmed
 * transition — the id is stable, the status/updated_at/title come from the wire
 * truth (the block carries the full record; the row is the slim projection). */
function mergeRow(row: IssueRow, block: IssueBlock): IssueRow {
  return {
    ...row,
    title: block.title ?? row.title,
    workflow_status: block.workflow_status ?? row.workflow_status,
    updated_at: block.updated_at ?? row.updated_at,
    scope: block.scope ?? row.scope,
    type_name: block.type ?? row.type_name,
  }
}

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
  /** issueId → a transition is in flight (disables its move affordance, §4.5). */
  transitioning = $state<Record<string, boolean>>({})
  /** The last transition failure (409/422/403/network) — surfaced as an alert;
   * cleared when a fresh transition starts. */
  transitionError = $state<ApiError | null>(null)

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

  /** Is a target status a real drop target (a known column, not the synthetic
   * unmapped one)? Drop onto an unmapped column is refused up front (§4.2). */
  isDropTarget(status: string): boolean {
    const col = this.columns.find((c) => c.status === status)
    return col !== undefined && col.category !== 'unmapped'
  }

  /** The other droppable statuses for a card currently in `from` — the Move
   * dialog's target list (the registry vocabulary, minus the current column and
   * the unmapped synthetic ones). */
  moveTargets(from: string): string[] {
    return this.columns.filter((c) => c.category !== 'unmapped' && c.status !== from).map((c) => c.status)
  }

  /**
   * Move an issue to another status. OPTIMISTIC (§4.5): the card + counts move
   * before the round-trip. On success the card is reconciled from the server
   * issue; on ApiError the move is ROLLED BACK (visible immediately) and
   * transitionError is set. A 409/422 additionally re-reads board + registry
   * (§4.8) so the columns reflect the wire truth after a policy drift. The
   * promise resolves either way (the caller announces via transitionError) —
   * a same-column or unknown move is a no-op.
   */
  async transition(issueId: string, from: string, to: string): Promise<void> {
    if (from === to || !this.isDropTarget(to)) return
    const before = this.columns
    const card = this.#findCard(before, issueId, from)
    if (card === null) return

    this.transitionError = null
    this.transitioning = { ...this.transitioning, [issueId]: true }
    // Optimistic move: pull from `from`, prepend to `to` (a transition bumps
    // updated_at, so the card sorts to the top of its new column).
    this.columns = this.#withMove(before, issueId, from, to)

    try {
      const res = await this.#api.patchIssue(this.#projectId, issueId, { status: to })
      const serverStatus = res.issue.workflow_status ?? to
      if (serverStatus !== to) {
        // The server landed the card in a different status than requested —
        // re-read the wire truth instead of guessing (rare policy remap).
        await this.load()
      } else {
        this.columns = this.columns.map((c) =>
          c.status === to
            ? { ...c, issues: c.issues.map((i) => (i.id === issueId ? mergeRow(i, res.issue) : i)) }
            : c,
        )
      }
    } catch (err) {
      const e = toApiError(err)
      this.columns = before // visible rollback (§4.5 — trivially correct, no order state)
      this.transitionError = e
      // Registry staleness (§4.8): a 409/422 re-reads board + registry so the
      // columns (and the unmapped mechanic) reflect the wire truth.
      if (e.status === 409 || e.status === 422) await this.load()
    } finally {
      this.transitioning = { ...this.transitioning, [issueId]: false }
    }
  }

  #findCard(columns: ClassifiedColumn[], issueId: string, from: string): IssueRow | null {
    const col = columns.find((c) => c.status === from)
    return col?.issues.find((i) => i.id === issueId) ?? null
  }

  #withMove(columns: ClassifiedColumn[], issueId: string, from: string, to: string): ClassifiedColumn[] {
    const card = this.#findCard(columns, issueId, from)
    if (card === null) return columns
    const moved: IssueRow = { ...card, workflow_status: to }
    return columns.map((c) => {
      if (c.status === from) {
        return {
          ...c,
          issues: c.issues.filter((i) => i.id !== issueId),
          count: Math.max(0, c.count - 1),
        }
      }
      if (c.status === to) {
        return { ...c, issues: [moved, ...c.issues], count: c.count + 1 }
      }
      return c
    })
  }
}
