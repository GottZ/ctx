// Chat store (design 06 §3.9). A runes class that owns the session list, the
// open session's messages, and the transient live-turn state. `.svelte.ts` so
// $state is reactive in the consuming components.
//
// Turn lifecycle: send() appends an optimistic user message, opens the stream,
// and reduces each SSE event into live state (applyEvent). When the stream ends
// cleanly it reloads the persisted truth (server seq/sensitivity/tool_calls are
// authoritative) and clears the live state; on an `error` event it keeps the
// partial + a retry affordance. abort() cancels the in-flight turn — the server
// persists the partial with a canceled marker and frees the llama.cpp slot.

import { toApiError, type ApiError } from '../api'
import { deleteSession, getSession, listSessions } from './api'
import { streamTurn } from './stream'
import type {
  BackendEvent,
  ChatMessage,
  ChatSessionListItem,
  DoneEvent,
  StreamRequest,
  ToolCallEvent,
  ToolResultEvent,
} from './types'

/** One tool invocation assembled live from tool_call + tool_result events. */
export interface LiveTool {
  iteration: number
  id: string
  name: string
  arguments?: Record<string, unknown>
  result?: ToolResultEvent
}

/** Saturation state (B2): a capacity block awaiting (auto-)retry. */
export interface Saturation {
  /** Seconds from the 429 Retry-After header; null = no concrete hint. */
  retryAfter: number | null
  /** Live countdown to the scheduled auto-retry; null when none is scheduled. */
  secondsLeft: number | null
}

const OPTIMISTIC_SEQ = -1

export class ChatStore {
  sessions = $state<ChatSessionListItem[]>([])
  currentId = $state<string | null>(null)
  messages = $state<ChatMessage[]>([])

  loadingList = $state(false)
  loadingSession = $state(false)
  streaming = $state(false)

  // Live turn state — rendered below the persisted messages while streaming.
  liveAssistant = $state('')
  liveTools = $state<LiveTool[]>([])
  liveBackend = $state<BackendEvent | null>(null)
  done = $state<DoneEvent | null>(null)

  /**
   * The turn is waiting in the dispatcher queue (MW8 `queued` SSE keepalive):
   * a foreign stream holds the 1-slot GPU target while we wait for admission.
   * Distinct from "the model is thinking" — the queued indicator, not a cursor,
   * is shown; cleared by the first post-admission event (backend/delta/…).
   */
  liveQueued = $state(false)

  /**
   * Capacity signal (MW8, DECISIONS B2): a pre-stream HTTP 429 (dispatcher
   * rejection / per-scope turn cap) OR a mid-stream `saturated` error event —
   * both retryable, neither a fault. `retryAfter` = seconds from the 429
   * Retry-After header when present (null = honest absence: generic notice,
   * manual retry only). `secondsLeft` drives the live countdown while an
   * auto-retry is scheduled (null when there is none). See SaturationNotice.
   */
  saturation = $state<Saturation | null>(null)

  /** A failure to surface in the composer: pre-stream JSON error or `error` event. */
  turnError = $state<string | null>(null)
  /** A failure loading the list / a session (separate from turn errors). */
  loadError = $state<ApiError | null>(null)

  #ctrl: AbortController | null = null
  #key: () => string | null
  // Retry plumbing for the saturation state (B2): the last turn to re-send and
  // the countdown ticker driving the jittered auto-retry.
  #satTimer: ReturnType<typeof setInterval> | null = null
  #lastTurn: { text: string; opts: Partial<StreamRequest> } | null = null

  constructor(getKey: () => string | null) {
    this.#key = getKey
  }

  /** GET the session list (metadata only). */
  async loadSessions(): Promise<void> {
    this.loadingList = true
    try {
      this.sessions = (await listSessions()).sessions
      this.loadError = null
    } catch (err) {
      this.loadError = toApiError(err)
    } finally {
      this.loadingList = false
    }
  }

  /** Open a session: load its full message history. A 404 clears the selection. */
  async selectSession(id: string): Promise<void> {
    if (this.streaming) return
    this.loadingSession = true
    this.turnError = null
    this.cancelSaturation()
    this.#clearLive()
    try {
      const detail = await getSession(id)
      this.currentId = id
      this.messages = detail.messages
      this.loadError = null
    } catch (err) {
      const e = toApiError(err)
      if (e.code === 'not_found') {
        this.newSession()
        await this.loadSessions()
      } else {
        this.loadError = e
      }
    } finally {
      this.loadingSession = false
    }
  }

  /** Start a fresh, unsaved session (the next send creates it server-side). */
  newSession(): void {
    if (this.streaming) return
    this.currentId = null
    this.messages = []
    this.turnError = null
    this.cancelSaturation()
    this.#clearLive()
  }

  /** Delete a session; if it was open, fall back to a fresh one. */
  async deleteSession(id: string): Promise<void> {
    try {
      await deleteSession(id)
      this.sessions = this.sessions.filter((s) => s.id !== id)
      if (this.currentId === id) this.newSession()
    } catch (err) {
      this.loadError = toApiError(err)
    }
  }

  /**
   * Send one turn. Appends an optimistic user message, streams the reply, then
   * reloads the persisted session (or keeps the partial on an error event).
   */
  async send(text: string, opts: Partial<StreamRequest> = {}): Promise<void> {
    const message = text.trim()
    if (message === '' || this.streaming) return

    // A fresh send supersedes any pending saturation retry; remember this turn
    // so retryLast()/auto-retry can re-send the exact same request.
    this.#clearSatTimer()
    this.saturation = null
    this.#lastTurn = { text: message, opts }

    const req: StreamRequest = { message, ...opts }
    if (this.currentId) req.session_id = this.currentId

    // Drop any lingering optimistic message (e.g. a prior saturated turn we kept
    // in view) before appending this one — idempotent, avoids a duplicate bubble.
    const base = this.messages.filter((m) => m.seq !== OPTIMISTIC_SEQ)
    this.messages = [
      ...base,
      { seq: OPTIMISTIC_SEQ, role: 'user', content: message, sensitivity: 'personal', created_at: new Date().toISOString() },
    ]
    this.streaming = true
    this.turnError = null
    this.liveQueued = false
    this.#clearLive()
    this.#ctrl = new AbortController()

    try {
      await streamTurn(req, this.#key(), (n, d) => this.applyEvent(n, d), this.#ctrl.signal)
    } catch (err) {
      // Pre-stream failure: drop the optimistic message.
      this.messages = this.messages.filter((m) => m.seq !== OPTIMISTIC_SEQ)
      this.streaming = false
      this.liveQueued = false
      const e = toApiError(err)
      if (e.status === 429) {
        // Capacity, not fault (§3.3): enter the saturation state with the
        // Retry-After hint (when present) instead of a hard turn error.
        const ra = typeof e.details?.retry_after === 'number' ? (e.details.retry_after as number) : null
        this.#enterSaturation(ra)
      } else {
        this.turnError = e.message
      }
      return
    }

    this.liveQueued = false
    // Stream ended. Keep the partial in view on an `error` event (turnError) or
    // a mid-stream `saturated` event (saturation); otherwise reload the
    // persisted truth (server seq/sensitivity/tool_calls are authoritative).
    if (this.turnError === null && this.saturation === null && this.currentId) {
      await this.#reloadCurrent()
      this.#clearLive()
    }
    this.streaming = false
    await this.loadSessions() // refresh titles / message_count / ordering
  }

  /** Re-send the last turn now (manual "Retry" or the scheduled auto-retry). */
  async retryLast(): Promise<void> {
    this.#clearSatTimer()
    const last = this.#lastTurn
    this.saturation = null
    if (last) await this.send(last.text, last.opts)
  }

  /** Give up on a saturated turn: clear the notice, timer and pending message. */
  cancelSaturation(): void {
    this.#clearSatTimer()
    this.saturation = null
    this.#lastTurn = null
    this.messages = this.messages.filter((m) => m.seq !== OPTIMISTIC_SEQ)
  }

  /**
   * Enter the saturation state. With a concrete Retry-After we start a jittered
   * countdown that auto-retries on expiry (the jitter spreads many saturated
   * clients so they don't fire in lockstep — no thundering herd). Without one
   * (honest absence / mid-stream `saturated`) we show a generic notice and wait
   * for a manual retry — never blind-hammer an unknown backend.
   */
  #enterSaturation(retryAfter: number | null): void {
    this.#clearSatTimer()
    if (retryAfter !== null && retryAfter > 0) {
      const jitterMs = Math.floor(Math.random() * 1000)
      const deadline = Date.now() + retryAfter * 1000 + jitterMs
      this.saturation = { retryAfter, secondsLeft: retryAfter }
      this.#satTimer = setInterval(() => {
        const left = Math.max(0, Math.ceil((deadline - Date.now()) / 1000))
        this.saturation = { retryAfter, secondsLeft: left }
        if (Date.now() >= deadline) void this.retryLast()
      }, 250)
    } else {
      this.saturation = { retryAfter: null, secondsLeft: null }
    }
  }

  #clearSatTimer(): void {
    if (this.#satTimer) {
      clearInterval(this.#satTimer)
      this.#satTimer = null
    }
  }

  /** Abort the in-flight turn (final — no resume). */
  abort(): void {
    this.#ctrl?.abort()
  }

  /**
   * Reduce one SSE event into live state. Public so the reducer is unit-testable
   * without a fetch mock; send() is the only production caller.
   */
  applyEvent(name: string, data: unknown): void {
    // Any real event means admission is over: clear the queued indicator. The
    // `queued` keepalive itself (re-fired every ~20s during the wait) sets it.
    if (name !== 'queued') this.liveQueued = false
    switch (name) {
      case 'session':
        this.currentId = (data as { session_id: string }).session_id
        break
      case 'queued':
        // MW8 dispatcher-queue keepalive (design/03 §4.4): the acquire is
        // blocked behind a foreign stream lease. Show a "waiting in queue"
        // indicator, not a thinking cursor — the wait can be long (up to the
        // GPU target's lease timeout) and admission is automatic.
        this.liveQueued = true
        break
      case 'backend':
        this.liveBackend = data as BackendEvent
        break
      case 'delta':
        this.liveAssistant += (data as { text: string }).text
        break
      case 'tool_call': {
        const e = data as ToolCallEvent
        this.liveTools = [...this.liveTools, { iteration: e.iteration, id: e.id, name: e.name, arguments: e.arguments }]
        break
      }
      case 'tool_result': {
        const e = data as ToolResultEvent
        this.liveTools = this.liveTools.map((t) => (t.id === e.id ? { ...t, result: e } : t))
        break
      }
      case 'done':
        this.done = data as DoneEvent
        break
      case 'error': {
        const e = data as { code: string }
        if (e.code === 'saturated') {
          // Mid-stream dispatcher rejection (engine finishUnserved, MW8): a
          // capacity signal, retryable — a subtle saturation notice, not a hard
          // fault. No Retry-After exists mid-stream ⇒ manual retry (retryAfter
          // null). The optimistic user message is kept so a retry re-sends it.
          this.#enterSaturation(null)
        } else {
          this.turnError = e.code
        }
        break
      }
      // tool_call_start / usage / unknown: ignored (forward-compat, §3.5).
    }
  }

  async #reloadCurrent(): Promise<void> {
    if (!this.currentId) return
    try {
      this.messages = (await getSession(this.currentId)).messages
    } catch (err) {
      this.loadError = toApiError(err)
    }
  }

  #clearLive(): void {
    this.liveAssistant = ''
    this.liveTools = []
    this.liveBackend = null
    this.liveQueued = false
    this.done = null
  }
}
