// Consumer detail state (RC-D C6, wave W05-09, design/05 §4.6 store table:
// `ConsumerDetailModel` — consumer, audit, calls; reload after mutation).
// Plain $state class with an injectable api so vitest covers it in node.
//
// Rules this model owns:
//   - load(id) reads model-consumer-get (consumer + key chain + audit) and,
//     in parallel, the consumer's last calls (GET /api/llmlog?consumer_id=).
//     A failing calls read never hides the consumer (callsError only).
//   - The No-Oracle refusal of a foreign/unknown id (404 "model consumer not
//     found") is the page's notFound state — no consumer, no calls (even if
//     the calls read answered), never a hint which case it was.
//   - Stale answers are dropped (generation counter), like ConsumersModel.
//   - The model keeps no body of any call (the list is body-free; the detail
//     route is server-admin only and not fetched here).

import { toApiError, type ApiError } from '../../lib/api'
import { getConsumer, listConsumerCalls } from '../../lib/api/gatewaypanels'
import type { ConsumerAuditRow, ConsumerCallEntry, ConsumerCallsResponse, ConsumerGetResponse, ConsumerView } from '../../lib/api/types'
import type { ResourceStatus } from '../../lib/resource.svelte'

interface ConsumerDetailApi {
  get: typeof getConsumer
  calls: typeof listConsumerCalls
}

const DEFAULT_API: ConsumerDetailApi = { get: getConsumer, calls: listConsumerCalls }

export class ConsumerDetailModel {
  consumer = $state<ConsumerView | null>(null)
  audit = $state<ConsumerAuditRow[]>([])
  keysTruncated = $state(false)
  calls = $state<ConsumerCallEntry[]>([])
  status = $state<ResourceStatus>('idle')
  loadError = $state<ApiError | null>(null)
  /** true = the server answered the No-Oracle "model consumer not found" (404). */
  notFound = $state(false)
  callsStatus = $state<ResourceStatus>('idle')
  callsError = $state<ApiError | null>(null)

  readonly #api: ConsumerDetailApi
  #gen = 0
  #id: string | null = null

  constructor(api: Partial<ConsumerDetailApi> = {}) {
    this.#api = { ...DEFAULT_API, ...api }
  }

  /** Load the consumer, its chain + audit, and its last calls. */
  async load(id: string): Promise<void> {
    const gen = ++this.#gen
    this.#id = id
    this.status = 'loading'
    this.callsStatus = 'loading'
    this.loadError = null
    this.callsError = null
    this.notFound = false
    const [got, calls] = await Promise.allSettled([this.#api.get(id), this.#api.calls(id, undefined)])
    if (gen !== this.#gen) return
    if (got.status === 'rejected') {
      const err = toApiError(got.reason)
      this.consumer = null
      this.audit = []
      this.keysTruncated = false
      this.calls = []
      this.notFound = err.status === 404
      this.loadError = err
      this.status = 'error'
      this.callsStatus = 'idle'
      return
    }
    const res: ConsumerGetResponse = got.value
    this.consumer = res.consumer
    this.audit = res.audit
    this.keysTruncated = res.keys_truncated
    this.status = 'ready'
    if (calls.status === 'fulfilled') {
      const c: ConsumerCallsResponse = calls.value
      this.calls = c.entries
      this.callsStatus = 'ready'
    } else {
      this.calls = []
      this.callsError = toApiError(calls.reason)
      this.callsStatus = 'error'
    }
  }

  /** Re-read the loaded consumer (after a mutation). */
  async reload(): Promise<void> {
    if (this.#id !== null) await this.load(this.#id)
  }
}
