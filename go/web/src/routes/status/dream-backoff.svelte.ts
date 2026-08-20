// Dream back-off model — holds the dream-stats answer (policy + per-eval-count
// maturity distribution) that the DreamTile renders as the histogram the CLI
// shows under `ctx dream stats`. Split out of the tile as a plain $state class
// (the StatusStore/ProfilesModel pattern) so the refetch throttle is
// unit-testable DOM-free via the injectable fetcher + clock.
//
// Refetch contract: the server side of dream-stats is an O(n) GROUP BY over
// context_blocks (R12 names the 1M+ follow-up), so the model never polls on its
// own. The tile feeds every status merge into sync(last_cycle_at); the model
// fetches (a) once on first sync, (b) again when last_cycle_at moved — each
// completed dream cycle advances exactly one block's eval count, so that stamp
// is the precise invalidation signal — but at most once per minIntervalMs.
// A moved stamp inside the quiet window marks the model dirty; the next sync
// call after the window (frames keep arriving every tick) performs the fetch.
// A failed fetch also marks dirty, so the stream of frames retries it on the
// same cadence instead of leaving a dead section until the next cycle.

import { toApiError, type ApiError } from '../../lib/api'
import { fetchDreamStats } from '../../lib/api/status'
import type { ResourceStatus } from '../../lib/resource.svelte'
import type { DreamStatsResponse } from '../../lib/api/types'

export class DreamBackoffModel {
  data = $state<DreamStatsResponse | null>(null)
  status = $state<ResourceStatus>('idle')
  error = $state<ApiError | null>(null)

  #fetch: () => Promise<DreamStatsResponse>
  #now: () => number
  #minIntervalMs: number
  // undefined = never synced (sentinel distinct from null: a corpus whose
  // dream loop never ran reports last_cycle_at null and must still load once).
  #lastStamp: string | null | undefined = undefined
  #dirty = false
  // null = never fetched (a plain 0 would collide with a clock that starts at 0)
  #lastFetchMs: number | null = null
  #seq = 0

  constructor(
    fetcher: () => Promise<DreamStatsResponse> = fetchDreamStats,
    opts: { minIntervalMs?: number; now?: () => number } = {},
  ) {
    this.#fetch = fetcher
    this.#minIntervalMs = opts.minIntervalMs ?? 30_000
    this.#now = opts.now ?? Date.now
  }

  /** Feed one status merge. Fetches on first call, then only when the dream
   *  cycle stamp moved AND the quiet window has passed (dirty carries a moved
   *  stamp across the window). */
  sync(lastCycleAt: string | null): void {
    if (this.#lastStamp === undefined) {
      this.#lastStamp = lastCycleAt
      this.#dirty = true
    } else if (lastCycleAt !== this.#lastStamp) {
      this.#lastStamp = lastCycleAt
      this.#dirty = true
    }
    if (!this.#dirty || this.status === 'loading') return
    if (this.#lastFetchMs !== null && this.#now() - this.#lastFetchMs < this.#minIntervalMs) return
    void this.#load()
  }

  async #load(): Promise<void> {
    const seq = ++this.#seq
    this.#dirty = false
    this.#lastFetchMs = this.#now()
    this.status = 'loading'
    try {
      const data = await this.#fetch()
      if (seq !== this.#seq) return
      this.data = data
      this.error = null
      this.status = 'ready'
    } catch (err) {
      if (seq !== this.#seq) return
      this.error = toApiError(err)
      this.status = 'error'
      this.#dirty = true // the frame stream retries after the quiet window
    }
  }
}

/** Compact hours renderer, ported 1:1 from the CLI's fmtDuration
 *  (go/internal/cli/dream_render.go): <24h as "Nh", <10d as "N.Nd", else "Nd" —
 *  keeps the back-off column readable across the 12h→45d range. */
export function fmtHours(hours: number): string {
  if (hours < 24) return `${Math.round(hours)}h`
  const days = hours / 24
  if (days < 10) return `${days.toFixed(1)}d`
  return `${Math.round(days)}d`
}
