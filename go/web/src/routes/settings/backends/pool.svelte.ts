// Pool editing state (design 04-§3.5). Holds the backend-list, its load status
// and table-level action errors; mutations call the manage actions and reload
// the list so the live status fields (effective_state, cooldown) stay fresh.
// Dialog methods (create/update) THROW ApiError on failure — the dialog caller
// surfaces it (a 400 trust-elevation is how the confirm flow re-drives). Table
// mutations share ONE guard (`mutating`): while any of them is in flight every
// other one is a no-op, so an overlapping finally can never null the busy
// state mid-flight. Plain $state class with an injectable api so vitest covers
// the flow without a DOM.

import { toApiError, type ApiError } from '../../../lib/api'
import {
  createBackend,
  deleteBackend,
  listBackends,
  reorderBackends,
  testBackend,
  updateBackend,
  type ConfirmFlags,
} from '../../../lib/api/backends'
import type { BackendListItem, BackendSpec, BackendTestResult } from '../../../lib/api/types'
import { createSpec, sortBackends, type BackendDraft } from '../../../lib/backends'
import type { ResourceStatus } from '../../../lib/resource.svelte'

interface PoolApi {
  list: typeof listBackends
  create: typeof createBackend
  update: typeof updateBackend
  del: typeof deleteBackend
  test: typeof testBackend
  reorder: typeof reorderBackends
}

export class PoolModel {
  backends = $state<BackendListItem[]>([])
  status = $state<ResourceStatus>('idle')
  loadError = $state<ApiError | null>(null)
  /**
   * Initiator row of the in-flight table mutation — the aria-busy anchor. The
   * initiating control stays enabled (a disabled button drops keyboard focus);
   * re-entry is stopped by the `mutating` guard, not by disabling it. null for
   * list-wide mutations (drag reorder).
   */
  busyId = $state<string | null>(null)
  /** True while ANY table mutation is in flight — all others no-op. */
  mutating = $state(false)
  /** Last table-action failure (priority/enabled/delete), shown above the table. */
  actionError = $state<string | null>(null)

  /**
   * Writability split (T37): a tenant-admin SEES `_global ∪ own` but may only
   * MUTATE rows of its own scope — backend-reorder demands EXACTLY the
   * writable subset (uniform 422 otherwise). The page sets this predicate
   * from the session (server-admin ⇒ all); the permissive default keeps the
   * model testable without a session.
   */
  isWritable: (b: BackendListItem) => boolean = () => true

  readonly sorted = $derived(sortBackends(this.backends))

  #api: PoolApi

  constructor(
    api: PoolApi = {
      list: listBackends,
      create: createBackend,
      update: updateBackend,
      del: deleteBackend,
      test: testBackend,
      reorder: reorderBackends,
    },
  ) {
    this.#api = api
  }

  async load(): Promise<void> {
    this.status = 'loading'
    this.loadError = null
    try {
      const res = await this.#api.list()
      this.backends = res.backends
      this.status = 'ready'
    } catch (err) {
      this.loadError = toApiError(err)
      this.status = 'error'
    }
  }

  reload = (): Promise<void> => this.load()

  byId(id: string): BackendListItem | undefined {
    return this.backends.find((b) => b.id === id)
  }

  /** Create — throws ApiError on failure, returns server warnings, reloads. */
  async create(name: string, draft: BackendDraft, confirm: ConfirmFlags = {}): Promise<string[]> {
    const res = await this.#api.create(createSpec(name, draft), confirm)
    await this.load()
    return res.warnings ?? []
  }

  /** Update a patch — throws on failure, returns warnings, reloads. */
  async update(id: string, spec: BackendSpec, confirm: ConfirmFlags = {}): Promise<string[]> {
    const res = await this.#api.update(id, spec, confirm)
    await this.load()
    return res.warnings ?? []
  }

  /** Delete — throws on failure (table caller surfaces), reloads on success. */
  async remove(id: string): Promise<void> {
    if (this.mutating) return
    this.#begin(id)
    try {
      await this.#api.del(id)
      await this.load()
    } catch (err) {
      this.actionError = toApiError(err).message
      throw err
    } finally {
      this.#end()
    }
  }

  /** Toggle enabled via a single-field patch (table switch). */
  async setEnabled(id: string, enabled: boolean): Promise<void> {
    await this.#tablePatch(id, { enabled })
  }

  /** Direct priority entry (the prio cell's click-to-edit input). */
  async setPriority(id: string, priority: number): Promise<void> {
    await this.#tablePatch(id, { priority })
  }

  /**
   * Atomic reorder to the given id order (drag drop / ▲▼). Optimistic: the
   * list is rewritten locally first (descending 10-steps, matching the chain
   * sort) so the drop lands instantly; the server write is guarded by the
   * priorities this view was based on — a 409 means someone else moved rows,
   * so the live truth is reloaded instead of overwritten. Any other failure
   * rolls back to the snapshot. Success reconciles against the server's
   * numbering via load().
   */
  async reorder(idsInOrder: string[], initiatorId: string | null = null): Promise<void> {
    if (this.mutating) return
    const byId = new Map(this.backends.map((b) => [b.id, b]))
    if (idsInOrder.length !== byId.size || !idsInOrder.every((id) => byId.has(id))) return
    // The wire order is the WRITABLE subsequence only (T37): non-writable
    // rows stay untouched server-side and keep their position via their
    // unchanged priorities. Sending the full visible list would 422 for a
    // tenant-admin whose view includes _global rows.
    const wireOrder = idsInOrder.filter((id) => this.isWritable(byId.get(id) as BackendListItem))
    if (wireOrder.length === 0) return
    const snapshot = this.backends
    const expected: Record<string, number> = {}
    for (const b of snapshot) if (this.isWritable(b)) expected[b.id] = b.priority
    this.#begin(initiatorId)
    // Optimistic mirror of the server's assignment: (n-i)*10 over the
    // writable wire order ONLY — non-writable rows keep their true
    // priorities and interleave exactly as the reconcile will render them.
    const optimistic = new Map(wireOrder.map((id, i) => [id, (wireOrder.length - i) * 10]))
    this.backends = this.backends.map((b) =>
      optimistic.has(b.id) ? { ...b, priority: optimistic.get(b.id) as number } : b,
    )
    try {
      await this.#api.reorder(wireOrder, expected)
      await this.load()
    } catch (err) {
      this.backends = snapshot
      const e = toApiError(err)
      if (e.status === 409) {
        this.actionError = 'priority order changed elsewhere — list reloaded'
        await this.load()
      } else {
        this.actionError = e.message
      }
    } finally {
      this.#end()
    }
  }

  /**
   * Keyboard fallback (▲▼ buttons): swap with the neighbor in the sorted view
   * and hand the FULL order to reorder() — one atomic write instead of two
   * racing per-row patches (the old swap could interleave with a concurrent
   * edit and leave duplicate priorities). No-op at the ends.
   */
  async reprioritize(id: string, dir: 'up' | 'down'): Promise<void> {
    const sorted = this.sorted
    // Neighbor search runs over the WRITABLE subsequence (T37): the swap
    // partner must be a row this caller may move — swapping with a read-only
    // _global neighbor would leave the wire order unchanged (silent no-op).
    const writable = sorted.filter((b) => this.isWritable(b))
    const wi = writable.findIndex((b) => b.id === id)
    const wj = dir === 'up' ? wi - 1 : wi + 1
    if (wi < 0 || wj < 0 || wj >= writable.length) return
    const ids = sorted.map((b) => b.id)
    const i = ids.indexOf(writable[wi].id)
    const j = ids.indexOf(writable[wj].id)
    ;[ids[i], ids[j]] = [ids[j], ids[i]]
    await this.reorder(ids, id)
  }

  /**
   * Reachability/chat probe — returns the verdict for the caller to render.
   * Read-only, so it does NOT take the mutation guard; the table tracks its
   * own per-row running state (a probe must not freeze unrelated mutations).
   */
  test(id: string, probeChat: boolean): Promise<BackendTestResult> {
    return this.#api.test(id, probeChat)
  }

  async #tablePatch(id: string, spec: BackendSpec): Promise<void> {
    if (this.mutating) return
    this.#begin(id)
    try {
      await this.#api.update(id, spec)
      await this.load()
    } catch (err) {
      this.actionError = toApiError(err).message
      // The optimistic control (checkbox/prio input) must snap back to the
      // live truth — without the reload it keeps showing the rejected value.
      await this.load()
    } finally {
      this.#end()
    }
  }

  #begin(id: string | null): void {
    this.mutating = true
    this.busyId = id
    this.actionError = null
  }

  #end(): void {
    this.mutating = false
    this.busyId = null
  }
}
