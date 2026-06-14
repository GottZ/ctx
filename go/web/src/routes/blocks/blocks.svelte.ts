// /blocks list state (block-workbench W1, read-only). Holds the result list,
// its load status and the load error; load() with an empty query shows the
// newest blocks (updated_at DESC), search(q) runs the stemmed FTS. Plain
// $state class with an injectable api so vitest covers the flow without a DOM
// (pool.svelte pattern).

import { toApiError, type ApiError } from '../../lib/api'
import { getBlock, listMeta, searchBlocks, type BlocksApi, type SearchResult } from '../../lib/api/blocks'
import type { ResourceStatus } from '../../lib/resource.svelte'

export class BlocksModel {
  results = $state<SearchResult[]>([])
  status = $state<ResourceStatus>('idle')
  loadError = $state<ApiError | null>(null)
  /** Current query text driving the list (empty → newest-first default). */
  query = $state('')

  #api: BlocksApi

  constructor(api: BlocksApi = { search: searchBlocks, listMeta, get: getBlock }) {
    this.#api = api
  }

  /**
   * Load the default list: an empty query, which the server orders
   * updated_at DESC (newest-first). Never invents a query string.
   */
  async load(): Promise<void> {
    this.query = ''
    await this.#run('')
  }

  /** Run the (stemmed) FTS for q and populate the list. */
  async search(q: string): Promise<void> {
    this.query = q
    await this.#run(q)
  }

  async #run(query: string): Promise<void> {
    this.status = 'loading'
    this.loadError = null
    try {
      const res = await this.#api.search({ query })
      this.results = res.results
      this.status = 'ready'
    } catch (err) {
      this.loadError = toApiError(err)
      this.status = 'error'
    }
  }
}
