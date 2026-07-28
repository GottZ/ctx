// SSE client (design 04-§3.6 / inventory web-svelte.md §6). `.svelte.ts`
// enables runes in a module so `status` is reactive in consuming components.
//
// fetch + ReadableStream + eventsource-parser, NOT native EventSource: der
// Client entstand in der Bearer-Ära (EventSource kann keinen Authorization-
// Header senden; `?token=` würde in Request-Logs landen) und bleibt seit der
// Cookie-Session (OAuth R4) der etablierte Transport. This client streams named
// events (status | backends | llmcalls | hb | error) to an onEvent dispatcher
// and reconnects with exponential backoff + jitter (cap 30s) after any
// non-clean end, until close() is called. The status field drives the page's
// poll fallback: callers poll while it is not 'open'.
//
// Liveness watchdog (design 05-§4.3-c, wave S4): a live transport is not a
// live SYSTEM. Since S3 the server's keepalive is `event: hb` carrying a
// SUCCESS stamp (`last_good_at`), so a wedged collector behind a perfectly
// healthy stream is visible here — and it is the case a frame-arrival watchdog
// structurally cannot see, because `hb` itself keeps the frames arriving. Two
// independent conditions therefore feed the sixth status 'stale':
//
//   payload   — no evidence of a successful collector tick within staleAfterMs
//   transport — no named frame of any kind within staleAfterMs (a half-open
//               connection behind a proxy, whose reader has not ended yet)
//
// 'stale' is deliberately NOT 'error': 'error' means the transport is broken
// and backoff is running (it repairs itself), 'stale' means the transport
// stands and the data is old (it does not). Consumers need the distinction —
// and because 'stale' is ≠ 'open', every existing poll gate reactivates on it
// with no change at the call site.
//
// The watchdog is OPT-IN (`{ heartbeat: true }`), because both conditions
// measure absence and only a stream that PROMISES traffic can read absence as
// a fault. /api/events promises it since S3; the workflow domain stream
// (/api/project/events) does not — it keeps the connection alive with a bare
// ": ping" comment, which this parser never surfaces (project_events.go:206).
// Arming it there would read legitimate silence (§3.1) as staleness and put
// /issues and /board back on the poll they left behind.

import { createParser, type EventSourceMessage } from 'eventsource-parser'

export type SseStatus = 'idle' | 'connecting' | 'open' | 'stale' | 'closed' | 'error'

/** 2.5 × the server's 25s `events.ping_interval` — two missed heartbeats plus
 *  slack, so a single late tick never raises a false alarm. */
export const DEFAULT_STALE_AFTER_MS = 62_000

/** Watchdog granularity. Independent of the threshold: the check is an integer
 *  compare on two numbers, so a tight tick costs nothing and bounds how long a
 *  wedged system keeps claiming to be live. */
const WATCHDOG_TICK_MS = 5_000

/** RFC3339 → epoch ms, null for absent/empty/unparseable. A missing stamp is
 *  NOT an error: `hb` sends last_good_at: null while the collector has not had
 *  one good tick yet, and that must read as "no payload evidence". */
function parseStamp(v: unknown): number | null {
  if (typeof v !== 'string' || v === '') return null
  const ms = Date.parse(v)
  return Number.isNaN(ms) ? null : ms
}

/** hb.degraded is the count of sections that fell back this tick (design
 *  05-§4.3-b). Tolerates a bool for forward/backward compatibility. */
function isDegraded(v: unknown): boolean {
  return typeof v === 'number' ? v > 0 : v === true
}

export class SseClient {
  status = $state<SseStatus>('idle')

  /** Transport evidence: local timestamp of the last named frame, whatever it
   *  carried. Set for EVERY event — `hb` included, which is exactly why it can
   *  never be the only condition. */
  lastFrameAt = $state(0)

  /** Payload evidence: local timestamp at which the last PROOF OF A SUCCESSFUL
   *  collector tick arrived — an advancing `hb.last_good_at`, or a `status`
   *  frame (the collector emits one only when its snapshot changed).
   *
   *  Deliberately a LOCAL clock reading, not the server stamp itself: the
   *  threshold is 62s and browser clocks drift by minutes, so `Date.now() -
   *  serverStamp` would fire permanently on any tab running ahead of the
   *  server. Measuring "how long since the stamp last MOVED" is skew-free and
   *  catches the same wedged collector — a frozen stamp never moves. */
  lastGoodAt = $state(0)

  /** hb.degraded > 0: the collector answered, but with sections missing. A
   *  WORD on the display, never a colour alone (design 05-§4.3-c). */
  partial = $state(false)

  readonly staleAfterMs: number

  /** Whether this endpoint promises periodic frames. Off by default — see the
   *  file header: a watchdog on a stream that may legitimately stay silent is
   *  a false-alarm generator, not a safety net. */
  readonly #heartbeat: boolean

  #ctrl: AbortController | null = null
  #timer: ReturnType<typeof setTimeout> | null = null
  #watchdog: ReturnType<typeof setInterval> | null = null
  /** Newest server success stamp seen on this connection, epoch ms. */
  #lastStamp = 0
  #retryMs = 1_000
  #stopped = false
  /** True while the backoff owns the display (an errored stream retrying).
   *  A PLAIN field, deliberately not derived from `status`: connect() runs
   *  inside the pages' `$effect(() => { void sse.connect(); return () =>
   *  sse.close() })`, and a synchronous READ of the status rune there makes
   *  that effect depend on the very state its own cleanup writes ('closed') —
   *  an effect_update_depth_exceeded loop that kills every effect on the page
   *  (found by the e2e contract run; regression probe in sse.svelte.test.ts). */
  #inBackoff = false

  constructor(
    private readonly url: string,
    private readonly onEvent: (name: string, data: unknown) => void,
    private readonly getInit: () => RequestInit = () => ({}),
    opts: { heartbeat?: boolean; staleAfterMs?: number } = {},
  ) {
    this.#heartbeat = opts.heartbeat === true
    this.staleAfterMs = opts.staleAfterMs ?? DEFAULT_STALE_AFTER_MS
  }

  async connect(): Promise<void> {
    this.#stopped = false
    this.#ctrl?.abort()
    this.#ctrl = new AbortController()
    // A retry inside a running backoff cycle stays 'error' — the display word
    // for it is 'reconnecting', and flapping error → connecting → error every
    // backoff round would be noise, not information. 'connecting' is the FIRST
    // attempt (and the one after a clean server-side end). Decided over the
    // plain #inBackoff mirror, never by reading the status rune — see its doc.
    if (!this.#inBackoff) this.status = 'connecting'
    try {
      const init = this.getInit()
      const res = await fetch(this.url, {
        ...init,
        signal: this.#ctrl.signal,
        headers: { Accept: 'text/event-stream', ...init.headers },
      })
      // A 429 (connection cap) or 401/403 (revoked key) lands here as !ok →
      // 'error' → the page polls; a freed slot / re-login recovers on retry.
      if (!res.ok || !res.body) throw new Error(`sse http ${res.status}`)
      // A fresh connection starts with a clean slate: the server answers the
      // initial frame within a tick, and a restarted server may well report an
      // OLDER success stamp than the previous connection did. Carrying either
      // clock across would flip a healthy new stream straight to 'stale'.
      this.lastFrameAt = Date.now()
      this.lastGoodAt = this.lastFrameAt
      this.#lastStamp = 0
      this.partial = false
      this.status = 'open'
      this.#inBackoff = false
      this.#retryMs = 1_000 // reset backoff after a good connect
      this.#armWatchdog()

      const parser = createParser({
        onEvent: (ev: EventSourceMessage) => {
          let data: unknown = null
          try {
            data = ev.data ? JSON.parse(ev.data) : null
          } catch {
            return // a malformed frame is dropped, never crashes the stream
          }
          const name = ev.event ?? 'message'
          this.#noteEvidence(name, data)
          this.onEvent(name, data)
        },
      })
      const reader = res.body.pipeThrough(new TextDecoderStream()).getReader()
      for (;;) {
        const { done, value } = await reader.read()
        if (done) break
        parser.feed(value)
      }
      // Clean EOF: the server ended the stream (shutdown or re-auth close).
      // Reconnect so a restarted server re-establishes; a revoked key is torn
      // down by the 'error' event handler + the poll fallback's 401.
      this.#scheduleReconnect()
    } catch {
      if (this.#ctrl?.signal.aborted || this.#stopped) {
        this.status = 'closed'
        return
      }
      this.status = 'error'
      this.#inBackoff = true
      this.#scheduleReconnect()
    }
  }

  /** Records what a frame proves. Called before the consumer dispatch so the
   *  page always renders against an already-updated liveness state. */
  #noteEvidence(name: string, data: unknown): void {
    this.lastFrameAt = Date.now()
    if (name === 'hb') {
      const d = data as { last_good_at?: unknown; degraded?: unknown } | null
      this.partial = isDegraded(d?.degraded)
      const stamp = parseStamp(d?.last_good_at)
      // Only an ADVANCING stamp is payload evidence. A wedged collector keeps
      // sending hb with the same stamp — frames flow, the system stands.
      if (stamp !== null && stamp > this.#lastStamp) {
        this.#lastStamp = stamp
        this.lastGoodAt = this.lastFrameAt
      }
    } else if (name === 'status') {
      // A status frame is emitted only on a snapshot DIFF, so its arrival is
      // proof of a successful build in its own right. Its `as_of` is not used:
      // that field advances even when sections degrade (design 05-§4.3-a).
      this.lastGoodAt = this.lastFrameAt
    }
    this.#recheck()
  }

  #armWatchdog(): void {
    if (!this.#heartbeat || this.#watchdog !== null) return
    this.#watchdog = setInterval(() => this.#recheck(), WATCHDOG_TICK_MS)
  }

  /** The one place open ⇄ stale is decided — symmetric, so fresh evidence
   *  recovers a stale display without waiting for a reconnect. Never touches
   *  idle / connecting / error / closed: those are transport states and the
   *  backoff owns them. */
  #recheck(): void {
    if (!this.#heartbeat) return
    if (this.status !== 'open' && this.status !== 'stale') return
    const now = Date.now()
    const stale =
      now - this.lastGoodAt > this.staleAfterMs || now - this.lastFrameAt > this.staleAfterMs
    this.status = stale ? 'stale' : 'open'
  }

  #scheduleReconnect(): void {
    if (this.#stopped) {
      this.status = 'closed'
      return
    }
    const wait = this.#retryMs * (0.5 + Math.random())
    this.#retryMs = Math.min(this.#retryMs * 2, 30_000)
    this.#timer = setTimeout(() => {
      if (!this.#stopped) void this.connect()
    }, wait)
  }

  close(): void {
    this.#stopped = true
    this.#inBackoff = false
    if (this.#timer) {
      clearTimeout(this.#timer)
      this.#timer = null
    }
    if (this.#watchdog) {
      clearInterval(this.#watchdog)
      this.#watchdog = null
    }
    this.#ctrl?.abort()
    this.status = 'closed'
  }
}
