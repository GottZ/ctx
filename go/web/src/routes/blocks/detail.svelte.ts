// Block detail panel state (block-workbench W3, read-only). When a hit in the
// /blocks list is picked (BlocksPage select(id)), the model lazy-loads the full
// block through the scope-gated getBlock (POST /api/manage {action:"get"}) and
// the panel renders content + metadata + an "open in graph" link. A not-found /
// not-visible block is surfaced as an error state (apiFetch rejects the 200
// {success:false} envelope as ApiError), never a crash. Plain $state class with
// an injectable api so vitest covers the flow without a DOM (pool.svelte
// pattern). Editing/delete/sensitivity are later waves (W4/W5/W6).

import { toApiError, type ApiError } from '../../lib/api'
import { getBlock, type BlockDetail, type BlocksApi } from '../../lib/api/blocks'
import type { ResourceStatus } from '../../lib/resource.svelte'

/** The single getBlock dependency, injectable for testing (mirrors BlocksApi.get). */
export type DetailApi = Pick<BlocksApi, 'get'>

export class BlockDetailModel {
  /** The loaded block, or null when closed / not yet loaded / errored. */
  block = $state<BlockDetail | null>(null)
  /** idle → loading → ready | error (lib/resource.svelte convention). */
  status = $state<ResourceStatus>('idle')
  /** The load failure (incl. not-found / not-visible), null otherwise. */
  loadError = $state<ApiError | null>(null)
  /** The id currently open in the panel (drives the ?focus= graph link); null when closed. */
  openId = $state<string | null>(null)

  #api: DetailApi

  constructor(api: DetailApi = { get: getBlock }) {
    this.#api = api
  }

  /**
   * Open the panel on id and lazy-load the full block. openId/loading/loadError
   * flip SYNCHRONOUSLY (before the await) so the panel shows loading on the
   * pending promise. A rejected getBlock — including the not-found / not-visible
   * case, which arrives as an HTTP 200 {success:false} envelope that apiFetch
   * rejects as ApiError — lands in the error state without ever throwing.
   */
  async load(id: string): Promise<void> {
    this.openId = id
    this.status = 'loading'
    this.loadError = null
    try {
      const res = await this.#api.get(id)
      this.block = res.block
      this.status = 'ready'
    } catch (err) {
      this.loadError = toApiError(err)
      this.status = 'error'
      this.block = null
    }
  }

  /** Close the panel and drop the loaded block (back to a non-content state). */
  close(): void {
    this.block = null
    this.openId = null
    this.loadError = null
    this.status = 'idle'
  }
}
