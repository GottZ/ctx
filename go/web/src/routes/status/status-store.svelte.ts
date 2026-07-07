// Status dashboard store (U01-W7, design/01 §4.5). Holds the live status object,
// its load state, AND the as_of-floor that makes every merge order-safe:
//
//  - SSE `status` frames and GET /api/status poll answers are DISCARDED when
//    their as_of is older than the floor (a late in-flight answer can no longer
//    overwrite a just-toggled profile — the pre-W7 StatusPage applied every
//    frame VORBEHALTLOS, which is exactly the stale-path-3 bug §2.3).
//  - A mutation answer (profile toggle, dream-mode) is the fresh truth: it is
//    spliced/merged in and RAISES the floor to its own as_of, so any older frame
//    still in flight is then rejected.
//
// Split out of StatusPage.svelte as a plain $state class (mirrors ProfilesModel)
// so the ordering logic is unit-testable DOM-free — the injectable fetcher lets
// the out-of-order gate drive it without a browser.

import { toApiError, type ApiError } from '../../lib/api'
import { fetchStatus, type DreamModeResponse } from '../../lib/api/status'
import type { DisableProfileMeta } from '../../lib/api/profiles'
import type { ResourceStatus } from '../../lib/resource.svelte'
import type { BackendStatus, StatusEvent, StatusResponse } from '../../lib/api/types'

/** Milliseconds of an RFC3339 timestamp; a malformed/empty stamp maps to 0 (it
 *  can never gate a real answer out — the floor only rises on parseable stamps). */
export function asOfMs(iso: string | undefined): number {
  if (!iso) return 0
  const t = Date.parse(iso)
  return Number.isNaN(t) ? 0 : t
}

/** Whether an answer stamped `asOf` may be applied given the current floor. An
 *  unparseable stamp (0) is admitted — freshness is unknown, never assume stale. */
export function acceptAsOf(asOf: string | undefined, floorMs: number): boolean {
  const ms = asOfMs(asOf)
  if (ms === 0) return true
  return ms >= floorMs
}

export class StatusStore {
  data = $state<StatusResponse | null>(null)
  status = $state<ResourceStatus>('idle')
  error = $state<ApiError | null>(null)
  /** Highest as_of seen (ms). Every merge below this is rejected as stale. */
  floorMs = $state(0)

  #fetch: () => Promise<StatusResponse>
  #seq = 0
  // A backends event can arrive before the first status frame on connect; buffer
  // it so it is never dropped (mirrors the pre-W7 pendingBackends).
  #pendingBackends: BackendStatus[] | null = null

  constructor(fetcher: () => Promise<StatusResponse> = fetchStatus) {
    this.#fetch = fetcher
  }

  /** Load (or poll-reload). Stale in-flight loads are superseded (seq), and an
   *  answer older than the floor is discarded (a late poll cannot revert a
   *  just-applied mutation) — but the FIRST load always lands. */
  async load(): Promise<void> {
    const seq = ++this.#seq
    this.status = 'loading'
    this.error = null
    try {
      const data = await this.#fetch()
      if (seq !== this.#seq) return
      if (this.data && !acceptAsOf(data.as_of, this.floorMs)) {
        this.status = 'ready' // keep the fresher held state
        return
      }
      this.data = data
      this.#bumpFloor(data.as_of)
      this.status = 'ready'
    } catch (err) {
      if (seq !== this.#seq) return
      this.error = toApiError(err)
      this.status = 'error'
    }
  }

  reload = (): Promise<void> => this.load()

  /** Apply an SSE `status` frame, floor-guarded. An out-of-order (older) frame
   *  is dropped so it cannot overwrite a just-toggled profile. */
  applyStatusFrame(e: StatusEvent): void {
    if (this.data && !acceptAsOf(e.as_of, this.floorMs)) return
    this.data = { success: true, ...e, backends: this.#pendingBackends ?? this.data?.backends ?? [] }
    this.#pendingBackends = null
    this.#bumpFloor(e.as_of)
    this.status = 'ready'
    this.error = null
  }

  /** Apply a `backends` frame (its own SSE event); buffer it if no status yet. */
  applyBackends(be: BackendStatus[]): void {
    if (this.data) this.data = { ...this.data, backends: be }
    else this.#pendingBackends = be
  }

  /** Splice ONE toggled profile into the held profiles array by (name, scope) —
   *  per-profile replace, no refetch — and raise the floor to the mutation's
   *  as_of so a late older frame is then rejected (§4.5-1/-6). */
  spliceProfile(p: DisableProfileMeta, asOf: string): void {
    if (!this.data) return
    const profiles = (this.data.profiles ?? []).map((row) =>
      row.name === p.name && row.scope === p.scope ? { ...row, active: p.active, label: p.label } : row,
    )
    this.data = { ...this.data, profiles }
    this.#bumpFloor(asOf)
  }

  /** Merge a dream-mode mutation answer (mode + throttle) into the held dream
   *  block and raise the floor (§4.5-4) — replaces the stale reload path. */
  applyDream(r: DreamModeResponse): void {
    if (!this.data) return
    this.data = {
      ...this.data,
      dream: { ...this.data.dream, mode: r.mode, throttle_interval_s: r.interval },
    }
    this.#bumpFloor(r.as_of)
  }

  #bumpFloor(asOf: string | undefined): void {
    this.floorMs = Math.max(this.floorMs, asOfMs(asOf))
  }
}
