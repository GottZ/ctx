// Pool editing state (design 04-§3.5). Holds the backend-list, its load status
// and table-level action errors; mutations call the manage actions and reload
// the list so the live status fields (effective_state, cooldown) stay fresh.
// Mutation methods THROW ApiError on failure — the dialog/table caller surfaces
// it (a 400 trust-elevation is how the confirm flow re-drives). Plain $state
// class with an injectable api so vitest covers the flow without a DOM.

import { toApiError, type ApiError } from '../../../lib/api'
import {
  createBackend,
  deleteBackend,
  listBackends,
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
}

export class PoolModel {
  backends = $state<BackendListItem[]>([])
  status = $state<ResourceStatus>('idle')
  loadError = $state<ApiError | null>(null)
  /** Row id with a table action (priority/enabled/delete/test) in flight. */
  busyId = $state<string | null>(null)
  /** Last table-action failure (priority/enabled/delete), shown above the table. */
  actionError = $state<string | null>(null)

  readonly sorted = $derived(sortBackends(this.backends))

  #api: PoolApi

  constructor(
    api: PoolApi = {
      list: listBackends,
      create: createBackend,
      update: updateBackend,
      del: deleteBackend,
      test: testBackend,
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
    this.busyId = id
    this.actionError = null
    try {
      await this.#api.del(id)
      await this.load()
    } catch (err) {
      this.actionError = toApiError(err).message
      throw err
    } finally {
      this.busyId = null
    }
  }

  /** Toggle enabled via a single-field patch (table switch). */
  async setEnabled(id: string, enabled: boolean): Promise<void> {
    await this.#tablePatch(id, { enabled })
  }

  /**
   * Move a backend up/down in the priority order. Swaps the two priorities;
   * on a tie it nudges past the neighbor so the order actually changes. No-op
   * at the ends. Reloads once after the (one or two) patches.
   */
  async reprioritize(id: string, dir: 'up' | 'down'): Promise<void> {
    const sorted = this.sorted
    const i = sorted.findIndex((b) => b.id === id)
    const j = dir === 'up' ? i - 1 : i + 1
    if (i < 0 || j < 0 || j >= sorted.length) return
    const me = sorted[i]
    const other = sorted[j]
    this.busyId = id
    this.actionError = null
    try {
      if (me.priority === other.priority) {
        await this.#api.update(id, { priority: dir === 'up' ? other.priority + 1 : other.priority - 1 })
      } else {
        await this.#api.update(id, { priority: other.priority })
        await this.#api.update(other.id, { priority: me.priority })
      }
      await this.load()
    } catch (err) {
      this.actionError = toApiError(err).message
    } finally {
      this.busyId = null
    }
  }

  /** Reachability/chat probe — returns the verdict for the caller to render. */
  async test(id: string, probeChat: boolean): Promise<BackendTestResult> {
    this.busyId = id
    try {
      return await this.#api.test(id, probeChat)
    } finally {
      this.busyId = null
    }
  }

  async #tablePatch(id: string, spec: BackendSpec): Promise<void> {
    this.busyId = id
    this.actionError = null
    try {
      await this.#api.update(id, spec)
      await this.load()
    } catch (err) {
      this.actionError = toApiError(err).message
    } finally {
      this.busyId = null
    }
  }
}
