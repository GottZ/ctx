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

  /** A failure to surface in the composer: pre-stream JSON error or `error` event. */
  turnError = $state<string | null>(null)
  /** A failure loading the list / a session (separate from turn errors). */
  loadError = $state<ApiError | null>(null)

  #ctrl: AbortController | null = null
  #key: () => string | null

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

    const req: StreamRequest = { message, ...opts }
    if (this.currentId) req.session_id = this.currentId

    this.messages = [
      ...this.messages,
      { seq: OPTIMISTIC_SEQ, role: 'user', content: message, sensitivity: 'personal', created_at: new Date().toISOString() },
    ]
    this.streaming = true
    this.turnError = null
    this.#clearLive()
    this.#ctrl = new AbortController()

    try {
      await streamTurn(req, this.#key(), (n, d) => this.applyEvent(n, d), this.#ctrl.signal)
    } catch (err) {
      // Pre-stream failure: drop the optimistic message, surface the error.
      this.messages = this.messages.filter((m) => m.seq !== OPTIMISTIC_SEQ)
      this.turnError = toApiError(err).message
      this.streaming = false
      return
    }

    // Stream ended. Reload the persisted truth unless an error event already
    // gave us a reason to keep the partial in view.
    if (this.turnError === null && this.currentId) {
      await this.#reloadCurrent()
      this.#clearLive()
    }
    this.streaming = false
    await this.loadSessions() // refresh titles / message_count / ordering
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
    switch (name) {
      case 'session':
        this.currentId = (data as { session_id: string }).session_id
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
      case 'error':
        this.turnError = (data as { code: string }).code
        break
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
    this.done = null
  }
}
