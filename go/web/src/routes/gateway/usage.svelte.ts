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
//     ConsumersModel); dispose() stops it.
//
// RC-D S1: contract stub — load() ends in 'error' (503), start() arms no
// timer.

import { ApiError } from '../../lib/api'
import { getUsage } from '../../lib/api/gatewaypanels'
import type { UsageSeries, UsageWindow } from '../../lib/api/types'
import type { ResourceStatus } from '../../lib/resource.svelte'
import { documentVisibility, type VisibilitySource } from './consumers.svelte'

/** Usage panel cadence — design/05 §4.7 row "Nutzungs-Panel": manual + 60 s, only while visible. */
export const USAGE_POLL_MS = 60_000

interface UsageApi {
  usage: typeof getUsage
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

  constructor(consumerId: string, api: Partial<UsageApi> = {}, visibility: VisibilitySource = documentVisibility) {
    this.#consumerId = consumerId
    this.#api = { usage: getUsage, ...api }
    this.#visibility = visibility
  }

  /** Read the current window. */
  async load(): Promise<void> {
    void this.#consumerId
    void this.#api
    this.status = 'error'
    this.loadError = new ApiError(503, 'service_unavailable', 'usage panel: not implemented (RC-D S1 stub)')
  }

  /** Switch the window and re-read. */
  async setWindow(w: UsageWindow): Promise<void> {
    this.window = w
    await this.load()
  }

  /** Arm the visible-only poll. */
  start(): void {
    void this.#visibility
  }

  /** Stop the poll and drop in-flight answers. */
  dispose(): void {}
}
