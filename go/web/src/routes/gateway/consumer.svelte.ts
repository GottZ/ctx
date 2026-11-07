// Consumer detail state (RC-D C6, wave W05-09, design/05 §4.6 store table:
// `ConsumerDetailModel` — consumer, audit, calls; reload after mutation).
// Plain $state class with an injectable api so vitest covers it in node.
//
// Rules this model owns:
//   - load(id) reads model-consumer-get (consumer + key chain + audit) and,
//     independently, the consumer's last calls (GET /api/llmlog?consumer_id=).
//     A failing calls read never hides the consumer (callsError only).
//   - The No-Oracle refusal of a foreign/unknown id (ApiError "consumer not
//     found", 404) is the page's notFound state — the model never retries with a
//     different id and never reveals which case it was.
//   - Stale answers are dropped (generation counter), like ConsumersModel.
//   - The model keeps no body of any call (the list is body-free; the detail
//     route is server-admin only and not fetched here).
//
// RC-D S1: contract stub — load() leaves the model in 'error' with a 503
// ApiError and loads nothing.

import { ApiError } from '../../lib/api'
import { getConsumer, listConsumerCalls } from '../../lib/api/gatewaypanels'
import type { ConsumerAuditRow, ConsumerCallEntry, ConsumerView } from '../../lib/api/types'
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
  /** true = the server answered the No-Oracle "consumer not found". */
  notFound = $state(false)
  callsStatus = $state<ResourceStatus>('idle')
  callsError = $state<ApiError | null>(null)

  readonly #api: ConsumerDetailApi

  constructor(api: Partial<ConsumerDetailApi> = {}) {
    this.#api = { ...DEFAULT_API, ...api }
  }

  /** Load the consumer, its chain + audit, and its last calls. */
  async load(id: string): Promise<void> {
    void id
    void this.#api
    this.status = 'error'
    this.loadError = new ApiError(503, 'service_unavailable', 'consumer detail: not implemented (RC-D S1 stub)')
  }

  /** Re-read the loaded consumer (after a mutation). */
  async reload(): Promise<void> {
    if (this.consumer !== null) await this.load(this.consumer.id)
  }
}
