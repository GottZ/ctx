// /blocks list state (block-workbench W1, read-only). Holds the result list,
// its load status and the load error; load() with an empty query shows the
// newest blocks (updated_at DESC), search(q) runs the stemmed FTS. Plain
// $state class with an injectable api so vitest covers the flow without a DOM
// (pool.svelte pattern).

import { toApiError, type ApiError } from '../../lib/api'
import {
  getBlock,
  listCategories,
  listMeta,
  searchBlocks,
  type BlocksApi,
  type CategoryCount,
  type SearchCursor,
  type SearchResult,
} from '../../lib/api/blocks'
import { defaultFilters, toSearchParams, type BlockFilters } from '../../lib/blocks/filters'
import type { SearchRequest } from '../../lib/api/blocks'
import type { ResourceStatus } from '../../lib/resource.svelte'

export class BlocksModel {
  results = $state<SearchResult[]>([])
  status = $state<ResourceStatus>('idle')
  loadError = $state<ApiError | null>(null)
  /** Current query text driving the list (empty → newest-first default). */
  query = $state('')
  /** The one reducer state: query + category + tags + (client-side) scope. */
  filters = $state<BlockFilters>(defaultFilters())
  /** Category facet options from manage list-categories (W2). */
  categories = $state<CategoryCount[]>([])
  /**
   * Cursor for the NEXT page (W7), or null when there is none (FTS mode, or the
   * last page was reached). The "Load more" button is shown only while this is
   * non-null AND the list is in newest-first (empty-query) mode.
   */
  nextCursor = $state<SearchCursor | null>(null)
  /** True while a loadMore() append is in flight (button disable / spinner). */
  loadingMore = $state(false)

  #api: BlocksApi
  /**
   * The body of the CURRENT page-1 search, kept so loadMore() can re-issue it
   * with the keyset cursor appended (same query/category/tags, next page).
   */
  #lastBody: SearchRequest = { query: '' }

  constructor(api: BlocksApi = { search: searchBlocks, listMeta, listCategories, get: getBlock }) {
    this.#api = api
  }

  /**
   * Load the default list: an empty query, which the server orders
   * updated_at DESC (newest-first). Never invents a query string. The bare
   * {query:''} body is W1-identical — no category/tags keys.
   */
  async load(): Promise<void> {
    this.query = ''
    await this.#run({ query: '' })
  }

  /** Run the (stemmed) FTS for q and populate the list. */
  async search(q: string): Promise<void> {
    this.query = q
    await this.#run({ query: q })
  }

  /**
   * Replace the filter state (always a new object) and reload through the
   * server contract: the active facets are serialized via toSearchParams, so
   * category/tags reach /api/search and default filters collapse to the bare
   * {query:''} body (no category/tags keys — W1-identical).
   */
  async setFilters(next: BlockFilters): Promise<void> {
    this.filters = next
    this.query = next.query
    await this.#run(toSearchParams(next))
  }

  /** Load the category facet options from manage list-categories. */
  async loadCategories(): Promise<void> {
    try {
      const res = await this.#api.listCategories()
      this.categories = res.categories
    } catch {
      // The facet is a convenience — a failed category list must not break the
      // page; the user can still type a free-text query.
      this.categories = []
    }
  }

  /**
   * Append the NEXT page to results using the keyset cursor (W7 "Load more").
   * A no-op when there is no next page (nextCursor === null) or an append is
   * already in flight. On success it APPENDS the page's results (never
   * replaces), adopts the response's next_after as the new nextCursor, and
   * stops paging when that comes back null. Errors land in loadError WITHOUT
   * discarding the rows already shown.
   *
   * RED stub: no-op — it neither calls the api nor appends, so the loadMore
   * vitest (append + adopt-cursor + stop-at-null) fails until GREEN.
   */
  async loadMore(): Promise<void> {
    // Inert when there is no next page or an append is already running. The
    // "Load more" button is only shown in newest-first (empty-query) mode AND
    // while nextCursor is non-null, but guard here too so a stray call is safe.
    if (this.nextCursor === null || this.loadingMore) return
    this.loadingMore = true
    this.loadError = null
    try {
      // Re-issue the SAME page-1 body (query/category/tags) with the keyset
      // cursor appended — the server resumes strictly after it.
      const res = await this.#api.search({ ...this.#lastBody, after: this.nextCursor })
      // APPEND, never replace: the visible list grows by one page.
      this.results = [...this.results, ...res.results]
      // Adopt the next boundary; null ends paging (button hides, loadMore inert).
      this.nextCursor = res.next_after ?? null
    } catch (err) {
      // Keep the rows already shown — an append failure must not blank the list.
      this.loadError = toApiError(err)
    } finally {
      this.loadingMore = false
    }
  }

  async #run(body: SearchRequest): Promise<void> {
    this.status = 'loading'
    this.loadError = null
    // Remember the page-1 body so loadMore() can re-issue it with the cursor.
    this.#lastBody = body
    try {
      const res = await this.#api.search(body)
      this.results = res.results
      // The next-page cursor for "Load more" (null on the last page / FTS mode).
      this.nextCursor = res.next_after ?? null
      this.status = 'ready'
    } catch (err) {
      this.loadError = toApiError(err)
      this.status = 'error'
    }
  }
}
