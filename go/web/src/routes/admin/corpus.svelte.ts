// Corpus-maintenance model (design 04 §3, Wave A7). Drives the two tenant-LESS
// background jobs — the G41 sensitivity audit + the G40 credentials classify —
// over the start/status manage actions (lib/api/corpus.ts). There is NO SSE for
// these, so start kicks the run and we POLL `*-status` while `run.running`, then
// STOP (no endless poll once the run finishes; §6.7a "poll only while running").
//
// Testability + cleanup hinge on an INJECTABLE timer (a setTimeout seam, not a
// real `setInterval`). The poll is recursive-schedule (one in-flight request, no
// overlap): each tick reads status, then reschedules ONLY when `running` is still
// true. dispose() clears the pending handle so an unmount leaves no live timer
// and no further fetch. vitest drives a manual timer to prove the loop stops at
// `!running` and on error — deterministic, no wall-clock. Math.random/argless Date
// are avoided; any timestamp the UI shows comes from the status payload, not here.

import { toApiError } from '../../lib/api'
import {
  blocksAuditStatus,
  blocksClassifyStatus,
  startBlocksAudit,
  startBlocksClassify,
} from '../../lib/api/corpus'
import type { AuditStatus, BlocksAuditStatusResponse, BlocksClassifyStatusResponse, ClassifyStatus } from '../../lib/api/types'

/** Injectable seam: exactly the four corpus actions this wave needs. */
export interface CorpusApi {
  startAudit: typeof startBlocksAudit
  auditStatus: typeof blocksAuditStatus
  startClassify: typeof startBlocksClassify
  classifyStatus: typeof blocksClassifyStatus
}

/** setTimeout seam — a real timer in production, a manual one under vitest so the
 * poll loop is driven tick-by-tick with no wall-clock. `set` returns an opaque
 * handle that `clear` cancels (dispose → no dangling timer). */
export interface Timer {
  set(cb: () => void, ms: number): unknown
  clear(handle: unknown): void
}

const defaultTimer: Timer = {
  set: (cb, ms) => setTimeout(cb, ms),
  clear: (h) => clearTimeout(h as ReturnType<typeof setTimeout>),
}

/** Fixed poll cadence (§6.7a). One in-flight request; no overlap. */
const POLL_MS = 2000

export interface CorpusOpts {
  timer?: Timer
  pollMs?: number
}

export class CorpusModel {
  /** Latest audit envelope (scope/pending/by_source/run) or null before a run. */
  audit = $state<BlocksAuditStatusResponse | null>(null)
  /** Latest classify envelope (scope/by_source/run) or null before a run. */
  classify = $state<BlocksClassifyStatusResponse | null>(null)
  /** Last start/poll failure for the audit job (string for the banner). */
  auditError = $state<string | null>(null)
  /** Last start/poll failure for the classify job. */
  classifyError = $state<string | null>(null)

  // Reactive private flags so the getters below re-render the running indicator
  // and disable the start buttons without exposing the internals.
  #auditPolling = $state(false)
  #classifyPolling = $state(false)
  #auditStarting = $state(false)
  #classifyStarting = $state(false)

  #api: CorpusApi
  #timer: Timer
  #pollMs: number
  #disposed = false
  #auditHandle: unknown = null
  #classifyHandle: unknown = null

  constructor(
    api: CorpusApi = {
      startAudit: startBlocksAudit,
      auditStatus: blocksAuditStatus,
      startClassify: startBlocksClassify,
      classifyStatus: blocksClassifyStatus,
    },
    opts: CorpusOpts = {},
  ) {
    this.#api = api
    this.#timer = opts.timer ?? defaultTimer
    this.#pollMs = opts.pollMs ?? POLL_MS
  }

  /** True while a status poll is live for the audit job (drives the indicator). */
  get auditPolling(): boolean {
    return this.#auditPolling
  }
  get classifyPolling(): boolean {
    return this.#classifyPolling
  }
  /** Disable the start button while a start request OR a poll is active. */
  get auditBusy(): boolean {
    return this.#auditStarting || this.#auditPolling
  }
  get classifyBusy(): boolean {
    return this.#classifyStarting || this.#classifyPolling
  }
  /** Convenience views for the template (counts/state live on `run`). */
  get auditRun(): AuditStatus | null {
    return this.audit?.run ?? null
  }
  get classifyRun(): ClassifyStatus | null {
    return this.classify?.run ?? null
  }

  /** Trigger the sensitivity audit (dry_run is the no-write default, decided by
   * the UI). Sets the initial status from the start response and, when the run is
   * live, begins the poll. A second call while busy is a no-op. */
  async startAudit(dryRun: boolean): Promise<void> {
    if (this.auditBusy) return
    this.#auditStarting = true
    this.auditError = null
    try {
      const res = await this.#api.startAudit({ dry_run: dryRun })
      this.audit = res
      if (res.run.running && !this.#disposed) {
        this.#auditPolling = true
        this.#scheduleAudit()
      }
    } catch (err) {
      this.auditError = toApiError(err).message
    } finally {
      this.#auditStarting = false
    }
  }

  /** Trigger the credentials classify (same dry-run-default policy). */
  async startClassify(dryRun: boolean): Promise<void> {
    if (this.classifyBusy) return
    this.#classifyStarting = true
    this.classifyError = null
    try {
      const res = await this.#api.startClassify({ dry_run: dryRun })
      this.classify = res
      if (res.run.running && !this.#disposed) {
        this.#classifyPolling = true
        this.#scheduleClassify()
      }
    } catch (err) {
      this.classifyError = toApiError(err).message
    } finally {
      this.#classifyStarting = false
    }
  }

  /** Stop both polls and clear pending timers — call on unmount (no leak). */
  dispose(): void {
    this.#disposed = true
    if (this.#auditHandle != null) {
      this.#timer.clear(this.#auditHandle)
      this.#auditHandle = null
    }
    if (this.#classifyHandle != null) {
      this.#timer.clear(this.#classifyHandle)
      this.#classifyHandle = null
    }
    this.#auditPolling = false
    this.#classifyPolling = false
  }

  #scheduleAudit(): void {
    this.#auditHandle = this.#timer.set(() => {
      this.#auditHandle = null
      void this.#tickAudit()
    }, this.#pollMs)
  }

  async #tickAudit(): Promise<void> {
    if (this.#disposed) {
      this.#auditPolling = false
      return
    }
    try {
      const res = await this.#api.auditStatus()
      this.audit = res
      // STOP condition: reschedule only while the backend reports running.
      if (res.run.running && !this.#disposed) {
        this.#scheduleAudit()
      } else {
        this.#auditPolling = false
      }
    } catch (err) {
      this.auditError = toApiError(err).message
      this.#auditPolling = false
    }
  }

  #scheduleClassify(): void {
    this.#classifyHandle = this.#timer.set(() => {
      this.#classifyHandle = null
      void this.#tickClassify()
    }, this.#pollMs)
  }

  async #tickClassify(): Promise<void> {
    if (this.#disposed) {
      this.#classifyPolling = false
      return
    }
    try {
      const res = await this.#api.classifyStatus()
      this.classify = res
      if (res.run.running && !this.#disposed) {
        this.#scheduleClassify()
      } else {
        this.#classifyPolling = false
      }
    } catch (err) {
      this.classifyError = toApiError(err).message
      this.#classifyPolling = false
    }
  }
}
