// Block editor save flow (block-workbench W4). Drives POST /api/store (create)
// and POST /api/manage {update} (edit, only changed fields via blockDiff) and
// reloads the list (+ open detail) after a successful save. The sensitivity
// DOWNGRADE confirm is the exact analogue of the BackendDialog trust-elevation
// step: the server 400s a downgrade without confirm_sensitivity_downgrade, so
// on that 400 the model flips `needsConfirm` instead of re-sending — the dialog
// shows the confirm step, and the next save() carries the flag. 422 field
// errors land in `fieldErrors` next to the inputs. Plain $state class with an
// injectable api + reload callback so vitest covers the flow without a DOM
// (pool.svelte pattern).

import { toApiError } from '../../lib/api'
import { fieldErrors, type BlockFieldError } from '../../lib/blocks/fields'
import { blockDiff, createStoreRequest, isEmptyDiff, isSensitivityDowngrade, type BlockDraft } from '../../lib/blocks/edit'
import { storeBlock, updateBlock, type BlockWriteApi, type UpdateBlockData } from '../../lib/api/blocks'
import type { ResourceStatus } from '../../lib/resource.svelte'

export type EditMode = 'create' | 'edit'

/** What the model needs to save: the create draft, plus (edit) the id + the
 *  original draft to diff against. */
export interface SaveArgs {
  mode: EditMode
  draft: BlockDraft
  /** edit only: the full block UUID (the server resolves ids in HomeScope only). */
  id?: string
  /** edit only: the pre-edit draft, for blockDiff. */
  original?: BlockDraft
}

export class BlockEditModel {
  status = $state<ResourceStatus>('idle')
  /** Top-level failure message (non-field), null otherwise. */
  error = $state<string | null>(null)
  /** Per-field 422 errors, keyed by field name; empty otherwise. */
  fieldErrors = $state<BlockFieldError[]>([])
  /**
   * Set after a sensitivity-downgrade 400: the dialog shows the in-dialog
   * confirm step and the next save() must carry confirm_sensitivity_downgrade.
   * The model NEVER auto-re-sends without an explicit second save() call.
   */
  needsConfirm = $state(false)

  #api: BlockWriteApi
  #reload: () => Promise<void>

  constructor(
    api: BlockWriteApi = { store: storeBlock, update: updateBlock },
    reload: () => Promise<void> = async () => {},
  ) {
    this.#api = api
    this.#reload = reload
  }

  /**
   * Save the draft. On create → storeBlock; on edit → updateBlock with the
   * blockDiff. A sensitivity downgrade without confirm comes back 400; the
   * model sets needsConfirm and does NOT re-send. A second save() (after the
   * user confirmed) adds confirm_sensitivity_downgrade:true. On any success the
   * reload callback runs and the model returns true; on a non-confirm failure
   * it records error/fieldErrors and returns false.
   */
  async save(args: SaveArgs): Promise<boolean> {
    this.status = 'loading'
    this.error = null
    this.fieldErrors = []
    try {
      if (args.mode === 'create') {
        await this.#api.store(createStoreRequest(args.draft))
      } else {
        const original = args.original ?? args.draft
        const data: UpdateBlockData = blockDiff(original, args.draft)
        // Nothing changed → no patch to persist (the server has no meaningful
        // empty update); close cleanly without a round-trip.
        if (isEmptyDiff(data)) {
          this.needsConfirm = false
          this.status = 'ready'
          return true
        }
        // Second save() after the in-dialog confirm: re-attach the downgrade
        // flag the server demanded. The model never sends it on the first try.
        if (this.needsConfirm) data.confirm_sensitivity_downgrade = true
        await this.#api.update(args.id ?? '', data)
      }
      this.needsConfirm = false
      await this.#reload()
      this.status = 'ready'
      return true
    } catch (err) {
      return this.#handleFailure(err, args)
    }
  }

  /** Reset to a clean state for a fresh dialog open. */
  reset(): void {
    this.status = 'idle'
    this.error = null
    this.fieldErrors = []
    this.needsConfirm = false
  }

  /**
   * Map a save failure onto the model. A sensitivity-downgrade 400 on an edit
   * (the client knows the draft lowers the rank) flips needsConfirm WITHOUT
   * re-sending — the dialog then shows the confirm step and the next save()
   * carries the flag. 422 → fieldErrors. Everything else → error message. In
   * every case: no reload, status='error', return false.
   */
  #handleFailure(err: unknown, args: SaveArgs): boolean {
    const e = toApiError(err)
    const downgrade =
      args.mode === 'edit' &&
      e.status === 400 &&
      !this.needsConfirm &&
      isSensitivityDowngrade((args.original ?? args.draft).sensitivity, args.draft.sensitivity)
    if (downgrade) {
      this.needsConfirm = true
      this.error = e.message
      this.status = 'error'
      return false
    }
    this.fieldErrors = fieldErrors(e)
    this.error = e.message
    this.status = 'error'
    return false
  }
}
