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

  #api: BlocksApi

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

  async #run(body: SearchRequest): Promise<void> {
    this.status = 'loading'
    this.loadError = null
    try {
      const res = await this.#api.search(body)
      this.results = res.results
      this.status = 'ready'
    } catch (err) {
      this.loadError = toApiError(err)
      this.status = 'error'
    }
  }
}
