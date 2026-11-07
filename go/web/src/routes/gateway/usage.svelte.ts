// Usage panel state (RC-D C6, wave W05-10, design/05 §4.6 store table:
// `UsageModel` — window, series, complete, hist; §4.7 cadence: manual + 60 s,
// only while the tab is visible).
//
// Rules this model owns:
//   - setWindow(w) re-reads GET /api/usage for the consumer with the window's
//     bucket (24h/7d hourly, 30d/90d daily); stale answers are dropped.
//   - `complete` and `hist` are the SERVER's flags, carried unchanged — the
//     model never fills a missing value (null stays null, never 0).
//   - Poll every USAGE_POLL_MS only while visible (VisibilitySource of
//     ConsumersModel); a tick in a hidden phase is caught up once on the
//     next visible change; dispose() stops it and drops in-flight answers.

import { toApiError, type ApiError } from '../../lib/api'
import { getUsage } from '../../lib/api/gatewaypanels'
import type { UsageSeries, UsageWindow } from '../../lib/api/types'
import type { ResourceStatus } from '../../lib/resource.svelte'
import { documentVisibility, type VisibilitySource } from './consumers.svelte'

/** Usage panel cadence — design/05 §4.7 row "Nutzungs-Panel": manual + 60 s, only while visible. */
export const USAGE_POLL_MS = 60_000

interface UsageApi {
  usage: (params: { consumer: string; window: UsageWindow }) => ReturnType<typeof getUsage>
}

export class UsageModel {
  window = $state<UsageWindow>('24h')
  series = $state<UsageSeries[]>([])
  /** The server's complete flag of the last answer; null = nothing loaded. */
  complete = $state<boolean | null>(null)
  /** The server's hist flag of the last answer; null = nothing loaded. */
  hist = $state<boolean | null>(null)
  status = $state<ResourceStatus>('idle')
  loadError = $state<ApiError | null>(null)

  readonly #consumerId: string
  readonly #api: UsageApi
  readonly #visibility: VisibilitySource
  #gen = 0
  #timer: ReturnType<typeof setInterval> | null = null
  #unsubscribe: (() => void) | null = null
  #missed = false

  constructor(consumerId: string, api: Partial<UsageApi> = {}, visibility: VisibilitySource = documentVisibility) {
    this.#consumerId = consumerId
    this.#api = { usage: (p) => getUsage(p), ...api }
    this.#visibility = visibility
  }

  /** Read the current window. */
  async load(): Promise<void> {
    const gen = ++this.#gen
    if (this.status !== 'ready') this.status = 'loading'
    try {
      const res = await this.#api.usage({ consumer: this.#consumerId, window: this.window })
      if (gen !== this.#gen) return
      this.series = res.series
      this.complete = res.complete
      this.hist = res.hist
      this.loadError = null
      this.status = 'ready'
    } catch (err) {
      if (gen !== this.#gen) return
      this.loadError = toApiError(err)
      this.status = 'error'
    }
  }

  /** Switch the window and re-read. */
  async setWindow(w: UsageWindow): Promise<void> {
    this.window = w
    await this.load()
  }

  /** Arm the visible-only poll and read once if visible. */
  start(): void {
    if (this.#timer !== null) return
    this.#timer = setInterval(() => this.#tick(), USAGE_POLL_MS)
    this.#unsubscribe = this.#visibility.subscribe(() => {
      if (this.#visibility.visible() && this.#missed) {
        this.#missed = false
        void this.load()
      }
    })
    if (this.#visibility.visible()) void this.load()
    else this.#missed = true
  }

  #tick(): void {
    if (this.#visibility.visible()) void this.load()
    else this.#missed = true
  }

  /** Stop the poll and drop in-flight answers. */
  dispose(): void {
    if (this.#timer !== null) clearInterval(this.#timer)
    this.#timer = null
    this.#unsubscribe?.()
    this.#unsubscribe = null
    this.#gen++
  }
}
