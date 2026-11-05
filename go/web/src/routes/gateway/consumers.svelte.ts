// Model API consumer list state (design/05 §4.6 store table, wave W05-05) —
// the read half of the /gateway area: the keyset-paged consumer list, its
// filter (status set, label prefix, server-admin tenant), the 30 s poll and
// the tenant options of the server-admin filter. Plain $state class with an
// injectable api + visibility source so vitest covers it in node without a DOM
// (precedent routes/tenant/keys.svelte.ts).
//
// Rules this model owns:
//   - Keyset (design/05 §3.2): next_cursor is OPAQUE — handed to the next
//     loadMore() byte-unchanged, never parsed; null = last page. Every filter
//     change drops the cursor and loads page 1. The client sends no `limit`,
//     so the page size is the server's gateway.consumer_list_max_limit.
//   - Status is the server's verdict (B-11): the model never derives a status
//     from expires_at and never reads the client clock.
//   - Polling (design/05 §4.7): every CONSUMER_POLL_MS, and ONLY while the tab
//     is visible; a tick that falls into a hidden phase is caught up once when
//     the tab turns visible again. A poll re-walks the loaded pages from the
//     start (keyset, each page's cursor from the fresh previous page), so a
//     consumer that moved between pages is neither lost nor shown twice.
//   - Stale answers are dropped: every load/loadMore bumps a generation, and a
//     response of an older generation never overwrites a newer state.
//   - dispose() stops the timer, removes the visibility listener and drops
//     whatever is still in flight.
//   - Mutations (wave W05-06): create() hands the reveal-once answer — WITH
//     the plaintext api_key — to the caller and keeps NOTHING of it (no
//     field, no cache, no error text; design/05 §5 B-6, precedent
//     routes/tenant/keys.svelte.ts create()). deactivate / reactivate /
//     revokeKey guard the row with busyId, put a refusal on actionError AND
//     rethrow (the ConfirmDialog stays open with the error), and hand the
//     server's answer (cancelled_inflight) back for the toast. Every success
//     reloads page 1 — the list shows what the server now says.

import { toApiError, type ApiError } from '../../lib/api'
import {
  CONSUMER_STATUSES,
  DEFAULT_CONSUMER_STATUSES,
  createConsumer,
  deleteConsumer,
  listConsumers,
  reactivateConsumer,
  revokeModelKey,
  type ConsumerCreateSpec,
  type ConsumerListParams,
} from '../../lib/api/modelapi'
import { listTenants } from '../../lib/api/tenants'
import type {
  ConsumerCreateResult,
  ConsumerRevokeResult,
  ConsumerStatus,
  ConsumerUpdateResult,
  ConsumerView,
  Tenant,
} from '../../lib/api/types'
import type { ResourceStatus } from '../../lib/resource.svelte'

/**
 * Poll cadence of the consumer list — design/05 §4.7 (Polling-Kadenz, row
 * „Konsumenten-Liste": 30 s, only while document.visibilityState ===
 * 'visible'). A UX cadence fixed by the design, not server policy.
 */
export const CONSUMER_POLL_MS = 30_000

/** Where the model learns whether the tab is visible (injectable for vitest). */
export interface VisibilitySource {
  visible(): boolean
  /** Subscribe to visibility changes; returns the unsubscribe. */
  subscribe(onChange: () => void): () => void
}

/** The browser source: document.visibilityState (precedent StatusPage.svelte poll). No document ⇒ never visible. */
export const documentVisibility: VisibilitySource = {
  visible: () => typeof document !== 'undefined' && document.visibilityState === 'visible',
  subscribe(onChange) {
    if (typeof document === 'undefined') return () => {}
    document.addEventListener('visibilitychange', onChange)
    return () => document.removeEventListener('visibilitychange', onChange)
  },
}

interface ConsumersApi {
  list: typeof listConsumers
  tenants: typeof listTenants
  create: (spec: ConsumerCreateSpec) => Promise<ConsumerCreateResult>
  del: typeof deleteConsumer
  reactivate: typeof reactivateConsumer
  revoke: typeof revokeModelKey
}

const DEFAULT_API: ConsumersApi = {
  list: listConsumers,
  tenants: listTenants,
  create: (spec) => createConsumer(spec),
  del: deleteConsumer,
  reactivate: reactivateConsumer,
  revoke: revokeModelKey,
}

/** The list filter: a non-empty status set, a label prefix ('' = none), a tenant (server-admin; null = all). */
export interface ConsumerFilter {
  statuses: ConsumerStatus[]
  q: string
  tenantId: string | null
}

/** A status set in the server's vocabulary order (stable wire, stable checkbox order). */
function canonical(statuses: readonly ConsumerStatus[]): ConsumerStatus[] {
  return CONSUMER_STATUSES.filter((s) => statuses.includes(s))
}

export class ConsumersModel {
  consumers = $state<ConsumerView[]>([])
  status = $state<ResourceStatus>('idle')
  /** Failure of the page-1 load (filter change, first load, Retry). */
  loadError = $state<ApiError | null>(null)
  /** Opaque keyset token of the next page; null = everything is loaded. */
  nextCursor = $state<string | null>(null)
  loadingMore = $state(false)
  /** Failure of the last loadMore(); the loaded rows stay. */
  moreError = $state<string | null>(null)
  /** Failure of the last poll; the shown rows stay until a poll succeeds. */
  refreshError = $state<string | null>(null)
  filter = $state<ConsumerFilter>({ statuses: [...DEFAULT_CONSUMER_STATUSES], q: '', tenantId: null })
  /** Options of the server-admin tenant filter (tenant-list). */
  tenants = $state<Tenant[]>([])
  tenantsError = $state<string | null>(null)
  /** The consumer or key id a deactivate / reactivate / revoke is in flight for. */
  busyId = $state<string | null>(null)
  /** Failure of the last row mutation (also rethrown to the confirm dialog). */
  actionError = $state<string | null>(null)

  #api: ConsumersApi
  #visibility: VisibilitySource
  #timer: ReturnType<typeof setInterval> | null = null
  #unsubscribe: (() => void) | null = null
  /** Generation of the list state: bumped by load, loadMore and dispose. */
  #seq = 0
  /** Pages the shown list is made of — what a poll re-walks. */
  #pages = 0
  #polling = false
  /** A tick fell into a hidden phase — catch it up once on the next visible. */
  #missed = false

  constructor(api: Partial<ConsumersApi> = {}, visibility: VisibilitySource = documentVisibility) {
    this.#api = { ...DEFAULT_API, ...api }
    this.#visibility = visibility
  }

  /** True while all five statuses are selected (sent as ["all"]). */
  get allStatuses(): boolean {
    return CONSUMER_STATUSES.every((s) => this.filter.statuses.includes(s))
  }

  /** Initial load + the visibility-gated poll. Idempotent while running. */
  start(): void {
    if (this.#timer !== null) return
    this.#timer = setInterval(() => this.#tick(), CONSUMER_POLL_MS)
    this.#unsubscribe = this.#visibility.subscribe(() => this.#onVisibility())
    void this.load()
  }

  /** Stop polling, remove the listener, drop every answer still in flight. */
  dispose(): void {
    if (this.#timer !== null) clearInterval(this.#timer)
    this.#timer = null
    this.#unsubscribe?.()
    this.#unsubscribe = null
    this.#missed = false
    this.#seq++
  }

  #params(cursor?: string): ConsumerListParams {
    const p: ConsumerListParams = { status: this.allStatuses ? ['all'] : canonical(this.filter.statuses) }
    if (this.filter.q !== '') p.q = this.filter.q
    if (this.filter.tenantId) p.tenant_id = this.filter.tenantId
    if (cursor !== undefined) p.cursor = cursor
    return p
  }

  /** Page 1 for the current filter (drops any cursor and every loaded page). */
  async load(): Promise<void> {
    const seq = ++this.#seq
    this.status = 'loading'
    this.loadError = null
    this.moreError = null
    this.refreshError = null
    this.loadingMore = false
    try {
      const res = await this.#api.list(this.#params())
      if (seq !== this.#seq) return
      this.consumers = res.consumers
      this.nextCursor = res.next_cursor
      this.#pages = 1
      this.status = 'ready'
    } catch (err) {
      if (seq !== this.#seq) return
      this.loadError = toApiError(err)
      this.status = 'error'
    }
  }

  reload = (): Promise<void> => this.load()

  /** Append the next keyset page; no-op on the last page or while one is loading. */
  async loadMore(): Promise<void> {
    const cursor = this.nextCursor
    if (cursor === null || this.loadingMore || this.status !== 'ready') return
    const seq = ++this.#seq
    this.loadingMore = true
    this.moreError = null
    try {
      const res = await this.#api.list(this.#params(cursor))
      if (seq !== this.#seq) return
      this.consumers = [...this.consumers, ...res.consumers]
      this.nextCursor = res.next_cursor
      this.#pages++
    } catch (err) {
      if (seq !== this.#seq) return
      this.moreError = toApiError(err).message
    } finally {
      if (seq === this.#seq) this.loadingMore = false
    }
  }

  /**
   * Refresh the shown list in place: re-walk the loaded pages from page 1.
   * Skipped unless the list is ready and idle; dropped if a load or loadMore
   * happened meanwhile. A failure keeps the rows and sets refreshError.
   */
  async poll(): Promise<void> {
    if (this.status !== 'ready' || this.loadingMore || this.#polling) return
    const seq = this.#seq
    this.#polling = true
    try {
      const rows: ConsumerView[] = []
      let cursor: string | undefined
      let next: string | null = null
      let walked = 0
      while (walked < Math.max(1, this.#pages)) {
        const res = await this.#api.list(this.#params(cursor))
        if (seq !== this.#seq) return
        rows.push(...res.consumers)
        next = res.next_cursor
        walked++
        if (next === null) break
        cursor = next
      }
      this.consumers = rows
      this.nextCursor = next
      this.#pages = walked
      this.refreshError = null
    } catch (err) {
      if (seq === this.#seq) this.refreshError = toApiError(err).message
    } finally {
      this.#polling = false
    }
  }

  #tick(): void {
    if (!this.#visibility.visible()) {
      this.#missed = true
      return
    }
    void this.poll()
  }

  #onVisibility(): void {
    if (!this.#missed || !this.#visibility.visible()) return
    this.#missed = false
    void this.poll()
  }

  /** Change the filter and load page 1. An empty status set is refused (not a filter the server knows). */
  async setFilter(patch: Partial<ConsumerFilter>): Promise<void> {
    const statuses = canonical(patch.statuses ?? this.filter.statuses)
    if (statuses.length === 0) return
    this.filter = {
      statuses,
      q: patch.q ?? this.filter.q,
      tenantId: patch.tenantId !== undefined ? patch.tenantId : this.filter.tenantId,
    }
    await this.load()
  }

  /** Add or remove one status; the last selected status cannot be removed. */
  async toggleStatus(s: ConsumerStatus): Promise<void> {
    const cur = this.filter.statuses
    const next = cur.includes(s) ? cur.filter((x) => x !== s) : [...cur, s]
    if (next.length === 0) return
    await this.setFilter({ statuses: next })
  }

  /** "all" on = the five statuses; off = back to the server default set. */
  async setAllStatuses(on: boolean): Promise<void> {
    await this.setFilter({ statuses: on ? [...CONSUMER_STATUSES] : [...DEFAULT_CONSUMER_STATUSES] })
  }

  /**
   * Create a consumer with its first key. Returns the reveal-once answer —
   * carrying the plaintext `api_key` — for the caller to show once and drop;
   * the model never keeps it (B-6). Throws the ApiError of a refusal (400
   * field, 403 tier, 409 label, 429 quota) untouched — the dialog renders it.
   * On success page 1 reloads (load() never throws), so the new consumer
   * arrives through the list, as the server lists it (hint only).
   */
  async create(spec: ConsumerCreateSpec): Promise<ConsumerCreateResult> {
    const res = await this.#api.create(spec)
    await this.load()
    return res
  }

  /** Deactivate (model-consumer-delete) — always allowed; returns cancelled_inflight for the toast. */
  async deactivate(id: string): Promise<ConsumerRevokeResult> {
    return this.#mutate(id, () => this.#api.del(id))
  }

  /** Reactivate (model-consumer-update {active:true}) — granting, the server gates it. */
  async reactivate(id: string): Promise<ConsumerUpdateResult> {
    return this.#mutate(id, () => this.#api.reactivate(id))
  }

  /** Revoke one key by its KEY id (model-key-revoke); returns cancelled_inflight for the toast. */
  async revokeKey(keyId: string): Promise<ConsumerRevokeResult> {
    return this.#mutate(keyId, () => this.#api.revoke(keyId))
  }

  async #mutate<T>(id: string, run: () => Promise<T>): Promise<T> {
    this.busyId = id
    this.actionError = null
    try {
      const res = await run()
      await this.load()
      return res
    } catch (err) {
      this.actionError = toApiError(err).message
      throw err
    } finally {
      this.busyId = null
    }
  }

  /** Load the server-admin tenant filter options (tenant-list is server-admin only). */
  async loadTenants(): Promise<void> {
    this.tenantsError = null
    try {
      const res = await this.#api.tenants()
      this.tenants = res.tenants
    } catch (err) {
      this.tenantsError = toApiError(err).message
    }
  }
}
