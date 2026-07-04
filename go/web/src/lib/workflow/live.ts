// LiveSource — the workflow live-update datenschicht (design 04 §7-U13 / §3.1,
// Achse-03-W9). It wraps the Achse-03 member SSE domain-event stream
// (GET /api/project/events, scope-tagged projectHub) and turns raw frames into a
// DEBOUNCED, COALESCED refetch signal the pages consume, while a poll keeps
// running as the permanent fallback (E04-7: SSE is a swap over the same
// interface, the poll never leaves).
//
// TRANSPORT = SseClient, NOT native EventSource. ctx auth is Bearer and native
// EventSource can send neither an Authorization header nor anything but GET
// (sse.svelte.ts:1-14 / design §3.6). The briefing note "EventSource ist Browser-
// nativ" does not apply here — the sanctioned Bearer-capable client is SseClient,
// which is exactly what this reuses. NO new dependency (eventsource-parser ships
// already, used by the SseClient).
//
// WIRE TRUTH (go/internal/events/project_hub.go:69-73 + handler/project_events.go:
// 195-201). Every domain frame arrives under the SSE event NAME 'project'
// (sw.event("project", ...)); the JSON kind is the discriminator:
//   - id-list frame:  {kind: <block type, default "issue">, project_id, op, block_ids}
//   - coalesced burst: {kind: "issues-bulk", project_id, count}   (NO block_ids)
//   - gap recovery:   {kind: "resync"}
//   - revoke:         SSE event name 'error' (stream ends; the 401 path + poll cover it)
// The briefing's "{kind:'project'}" conflated the SSE event NAME with the frame
// kind — this module keys on the frame SHAPE (block_ids present vs kind), so it is
// correct regardless of which block type a normal frame carries.
//
// COALESCING is two layers of ONE burst contract (design §3.1): the server hub
// collapses a sync burst into a single issues-bulk frame (O(ticks), not O(writes)),
// AND this client debounces ALL frames in a window into ONE onBatch — so even a
// hub that leaks single frames cannot make the UI refetch per-second.

import { SseClient, type SseStatus } from '../sse.svelte'
import { ISSUES_BASE } from '../api/issues'

/** The member SSE domain-event endpoint. Built from ISSUES_BASE so the single
 * path prefix (U03) governs it too — no second hand-written '/api/project'. */
export const PROJECT_EVENTS_PATH = `${ISSUES_BASE}/events`

/** Default poll cadence (E04-7: 10s interim, kept as the SSE fallback). */
export const DEFAULT_POLL_MS = 10_000
/** Client refetch-debounce window (design §3.1: 500ms over all frame types). */
export const DEFAULT_DEBOUNCE_MS = 500

/**
 * The coalesced refetch signal for ONE debounce window. `full` means a whole-
 * surface refetch is due (an issues-bulk / resync / unknown-shape frame, or a
 * poll tick); `ids` are the distinct issue block ids named by id-list frames in
 * the window. The CONSUMER chooses granularity: the list reloads its head on any
 * signal, the detail refetches only when its id is named, the board refetches the
 * affected columns or (on `full`) the whole board.
 */
export interface LiveBatch {
  full: boolean
  ids: string[]
}

/** The transport seam LiveSource drives. SseClient satisfies it; tests inject a
 * fake that pushes frames + flips status without a network. */
export interface LiveConnection {
  readonly status: SseStatus
  connect(): void | Promise<void>
  close(): void
}

/** Builds a connection bound to the LiveSource's frame dispatcher. */
export type LiveConnectionFactory = (onEvent: (name: string, data: unknown) => void) => LiveConnection

export interface LiveOptions {
  /** Called once per debounce window (or per poll tick) with the coalesced batch. */
  onBatch: (batch: LiveBatch) => void
  /** Transport factory (default: an SseClient on PROJECT_EVENTS_PATH + projectId). */
  connect?: LiveConnectionFactory
  /** Pins the stream to one project's scope (?project_id=), used by the default
   * factory only. */
  projectId?: string
  /** Auth/init for the default SseClient (Bearer header), used by the default
   * factory only. */
  getInit?: () => RequestInit
  pollMs?: number
  debounceMs?: number
  /** Tab-visibility guard for the poll (default: document.visibilityState, or
   * always-visible when there is no document — the node test env). */
  isVisible?: () => boolean
}

function defaultVisible(): boolean {
  return typeof document === 'undefined' || document.visibilityState === 'visible'
}

/** A frame body as it arrives off the wire (all fields optional / defensive). */
interface WireFrame {
  kind?: unknown
  block_ids?: unknown
}

function defaultFactory(projectId: string | undefined, getInit: () => RequestInit): LiveConnectionFactory {
  const q = projectId ? `?project_id=${encodeURIComponent(projectId)}` : ''
  const url = `${PROJECT_EVENTS_PATH}${q}`
  return (onEvent) => new SseClient(url, onEvent, getInit)
}

export class LiveSource {
  readonly #onBatch: (batch: LiveBatch) => void
  readonly #factory: LiveConnectionFactory
  readonly #pollMs: number
  readonly #debounceMs: number
  readonly #isVisible: () => boolean

  #conn: LiveConnection | null = null
  #pollTimer: ReturnType<typeof setInterval> | null = null
  #debounceTimer: ReturnType<typeof setTimeout> | null = null
  #pendingFull = false
  #pendingIds = new Set<string>()
  #started = false

  constructor(opts: LiveOptions) {
    this.#onBatch = opts.onBatch
    this.#pollMs = opts.pollMs ?? DEFAULT_POLL_MS
    this.#debounceMs = opts.debounceMs ?? DEFAULT_DEBOUNCE_MS
    this.#isVisible = opts.isVisible ?? defaultVisible
    this.#factory = opts.connect ?? defaultFactory(opts.projectId, opts.getInit ?? (() => ({})))
  }

  /** Current transport status (drives the poll suppression). */
  get status(): SseStatus {
    return this.#conn?.status ?? 'idle'
  }

  /** Open the stream and arm the poll fallback. Idempotent. */
  start(): void {
    if (this.#started) return
    this.#started = true
    this.#conn = this.#factory((name, data) => this.#onEvent(name, data))
    void this.#conn.connect()
    this.#pollTimer = setInterval(() => this.#tick(), this.#pollMs)
  }

  /** Close the stream and halt every timer. Idempotent. */
  stop(): void {
    this.#started = false
    this.#conn?.close()
    this.#conn = null
    if (this.#pollTimer !== null) {
      clearInterval(this.#pollTimer)
      this.#pollTimer = null
    }
    if (this.#debounceTimer !== null) {
      clearTimeout(this.#debounceTimer)
      this.#debounceTimer = null
    }
    this.#pendingFull = false
    this.#pendingIds.clear()
  }

  /** Fold one SSE frame into the pending window (never refetches synchronously —
   * the debounce owns the fire). */
  #onEvent(name: string, data: unknown): void {
    // 'error' ends the stream (revoke / tenant change). Nothing to refetch here:
    // the poll fallback resumes and the api client's 401 interceptor tears the
    // session down if the key is truly gone.
    if (name === 'error') return
    if (data === null || typeof data !== 'object') return
    const frame = data as WireFrame

    if (frame.kind === 'issues-bulk' || frame.kind === 'resync') {
      // Coalesced burst / gap recovery: ONE full refetch regardless of count.
      this.#pendingFull = true
    } else if (Array.isArray(frame.block_ids)) {
      // Id-list frame: collect the distinct affected ids (deduped by the Set).
      for (const id of frame.block_ids) if (typeof id === 'string') this.#pendingIds.add(id)
    } else {
      // Unknown / malformed shape: fail safe with a full refetch.
      this.#pendingFull = true
    }
    this.#schedule()
  }

  /** Arm the debounce once per window — a frame arriving while a timer is pending
   * only accumulates (never restarts the clock), so a steady stream still flushes
   * every debounceMs rather than never. */
  #schedule(): void {
    if (this.#debounceTimer !== null) return
    this.#debounceTimer = setTimeout(() => this.#flush(), this.#debounceMs)
  }

  #flush(): void {
    this.#debounceTimer = null
    const batch: LiveBatch = { full: this.#pendingFull, ids: [...this.#pendingIds] }
    this.#pendingFull = false
    this.#pendingIds.clear()
    if (batch.full || batch.ids.length > 0) this.#onBatch(batch)
  }

  /** Poll tick: while the stream is NOT delivering and the tab is visible, drive a
   * full refetch — the StatusPage fallback pattern. When SSE is open the live
   * frames drive refetches instead, so the poll stays quiet. */
  #tick(): void {
    if (this.status === 'open') return
    if (!this.#isVisible()) return
    this.#onBatch({ full: true, ids: [] })
  }
}
