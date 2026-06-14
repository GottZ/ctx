// Block delete flow (block-workbench W5). Drives POST /api/manage {action:
// "delete", id} (soft delete server-side) and, on success, closes the detail
// panel and reloads the list (so the deleted block disappears). A failure —
// the 200 {success:false} not-found envelope or a 409 ambiguous-prefix, both
// rejected as ApiError — is surfaced in `error` WITHOUT a reload and WITHOUT
// closing the panel. Block delete has NO referenced_by/FK-conflict path (that
// is the SECRET delete); the real failures are not-found / ambiguous-prefix.
// The native <dialog> confirm gate lives in BlockDetail.svelte (showModal, the
// BlockDialog/BackendDialog pattern) — this model is called only AFTER the user
// confirms, so it holds no dialog state. Plain $state class with an injectable
// api + reload + close callbacks so vitest covers the flow without a DOM
// (pool.svelte / edit.svelte pattern).

import { toApiError } from '../../lib/api'
import { deleteBlock, type BlockDeleteApi } from '../../lib/api/blocks'
import type { ResourceStatus } from '../../lib/resource.svelte'

export class BlockDeleteModel {
  status = $state<ResourceStatus>('idle')
  /** Last delete failure message (not-found / ambiguous-prefix), null otherwise. */
  error = $state<string | null>(null)
  /** True while a delete round-trip is in flight (disables the Confirm button). */
  busy = $state(false)

  #api: BlockDeleteApi
  #reload: () => Promise<void>
  #close: () => void

  constructor(
    api: BlockDeleteApi = { del: deleteBlock },
    reload: () => Promise<void> = async () => {},
    close: () => void = () => {},
  ) {
    this.#api = api
    this.#reload = reload
    this.#close = close
  }

  /**
   * Delete the block by FULL UUID (the server resolves ids in HomeScope only,
   * exactly like update). busy/status/error flip SYNCHRONOUSLY before the await
   * so the Confirm button disables on the pending promise. On success: close the
   * panel AND reload the list (the deleted block disappears), return true. On an
   * ApiError — the 200 {success:false} not-found envelope or a 409 ambiguous-
   * prefix, both rejected by apiFetch — surface `error`, leave the panel OPEN
   * and do NOT reload, return false. There is NO referenced_by/FK-conflict path
   * for a block delete (that is the SECRET delete).
   */
  async remove(id: string): Promise<boolean> {
    this.busy = true
    this.error = null
    this.status = 'loading'
    try {
      await this.#api.del(id)
      this.#close()
      await this.#reload()
      this.status = 'ready'
      return true
    } catch (err) {
      this.error = toApiError(err).message
      this.status = 'error'
      return false
    } finally {
      this.busy = false
    }
  }
}
