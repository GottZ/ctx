// SseClient gates (design 04-§3.6 / web-svelte.md §6): named-event dispatch off
// a fetch stream, and the HTTP-error → 'error' status that drives the page's
// poll fallback. fetch + ReadableStream are stubbed; pure node.

import { afterEach, describe, expect, it, vi } from 'vitest'
// Source text of the consuming page, as a string (vite ?raw). The poll-gate
// probe below asserts against the REAL page condition instead of a copy of it —
// node:fs is not available to this tsconfig, ?raw is.
import statusPageSource from '../routes/status/StatusPage.svelte?raw'
import sseSource from './sse.svelte.ts?raw'
import { DEFAULT_STALE_AFTER_MS, SseClient } from './sse.svelte'

// sseResponse builds a 200 text/event-stream Response whose body is the given
// SSE frames as one chunk (an explicit ReadableStream so .body is never null in
// the test env).
function sseResponse(...frames: string[]): Response {
  const bytes = new TextEncoder().encode(frames.join(''))
  const stream = new ReadableStream<Uint8Array>({
    start(c) {
      c.enqueue(bytes)
      c.close()
    },
  })
  return new Response(stream, { status: 200, headers: { 'Content-Type': 'text/event-stream' } })
}

// openStream builds a 200 text/event-stream Response that stays OPEN: frames are
// pushed one by one over (fake) time. The watchdog gates below need a stream that
// keeps delivering `hb` while the payload stamp stands still — a one-shot body
// would EOF straight into the reconnect path instead.
function openStream(): { res: Response; push: (frame: string) => void; end: () => void } {
  const enc = new TextEncoder()
  let ctrl: ReadableStreamDefaultController<Uint8Array>
  const stream = new ReadableStream<Uint8Array>({
    start(c) {
      ctrl = c
    },
  })
  return {
    res: new Response(stream, { status: 200, headers: { 'Content-Type': 'text/event-stream' } }),
    push: (frame: string) => ctrl.enqueue(enc.encode(frame)),
    end: () => ctrl.close(),
  }
}

function hbFrame(lastGoodAt: string | null, degraded = 0, health = 'ok'): string {
  return `event: hb\ndata: ${JSON.stringify({ last_good_at: lastGoodAt, degraded, health })}\n\n`
}

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

describe('SseClient', () => {
  it('dispatches named events with parsed JSON data', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValueOnce(
        sseResponse(
          'event: status\ndata: {"health":{"status":"ok"}}\n\n',
          ': ping\n\n',
          'event: backends\ndata: [{"name":"herbert-chat"}]\n\n',
        ),
      ),
    )
    const got: Array<[string, unknown]> = []
    const sse = new SseClient('/api/events', (name, data) => got.push([name, data]))
    await sse.connect()
    sse.close() // stop the post-EOF reconnect timer

    expect(got).toEqual([
      ['status', { health: { status: 'ok' } }],
      ['backends', [{ name: 'herbert-chat' }]],
    ])
  })

  it('sends the Authorization header from getInit and Accept: text/event-stream', async () => {
    const key = ['cafe', 'f00d'].join('') // doc-value, assembled at runtime (repo rule: no secret-shaped literals)
    const fetchMock = vi.fn().mockResolvedValueOnce(sseResponse('event: status\ndata: {}\n\n'))
    vi.stubGlobal('fetch', fetchMock)
    const sse = new SseClient(
      '/api/events',
      () => {},
      () => ({ headers: { Authorization: `Bearer ${key}` } }),
    )
    await sse.connect()
    sse.close()

    const init = fetchMock.mock.calls[0][1] as RequestInit
    const headers = init.headers as Record<string, string>
    expect(headers.Authorization).toBe(`Bearer ${key}`)
    expect(headers.Accept).toBe('text/event-stream')
  })

  it('goes to error on a non-ok status (429 connection cap → poll fallback)', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(new Response(null, { status: 429 })))
    const sse = new SseClient('/api/events', () => {})
    await sse.connect()
    expect(sse.status).toBe('error')
    sse.close() // stop the backoff reconnect timer
  })

  it('drops a malformed data frame without crashing the stream', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValueOnce(
        sseResponse('event: status\ndata: {bad json\n\n', 'event: backends\ndata: []\n\n'),
      ),
    )
    const got: Array<[string, unknown]> = []
    const sse = new SseClient('/api/events', (name, data) => got.push([name, data]))
    await sse.connect()
    sse.close()

    // The malformed status frame is skipped; the valid backends frame survives.
    expect(got).toEqual([['backends', []]])
  })

  // Dead-branch gate (RC-1 wave S3): a keepalive framed as an SSE COMMENT is
  // invisible to this client — eventsource-parser drops comment lines natively,
  // so no amount of ": ping" traffic ever reaches onEvent. That is why the
  // server's telemetry keepalive is the named `hb` event: only the second
  // framing below gives the page anything to act on.
  it('never fires on a ": ping" comment, always fires on an "hb" event', async () => {
    const hbData = { last_good_at: '2026-07-28T10:00:00Z', degraded: false, health: 'ok' }
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValueOnce(
        sseResponse(
          ': ping\n\n',
          ': ping\n\n',
          `event: hb\ndata: ${JSON.stringify(hbData)}\n\n`,
        ),
      ),
    )
    const got: Array<[string, unknown]> = []
    const sse = new SseClient('/api/events', (name, data) => got.push([name, data]))
    await sse.connect()
    sse.close()

    // Two comments + one event ⇒ exactly one dispatch, carrying the stamp.
    expect(got).toEqual([['hb', hbData]])
  })

  // Backward compatibility of the new frame (S3; the StatusPage watchdog is S4):
  // this client dispatches EVERY named event verbatim and knows nothing about
  // the name set, so `hb` rides an unchanged parser and the consumer's unknown-
  // name branch drops it silently (llmcall-feed.svelte.test.ts pins that half).
  it('dispatches an unknown event name verbatim (hb needs no client change)', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValueOnce(
        sseResponse(
          'event: hb\ndata: {"last_good_at":null,"degraded":true,"health":"unknown"}\n\n',
          'event: backends\ndata: []\n\n',
        ),
      ),
    )
    const got: Array<[string, unknown]> = []
    const sse = new SseClient('/api/events', (name, data) => got.push([name, data]))
    await sse.connect()
    sse.close()

    expect(got).toEqual([
      ['hb', { last_good_at: null, degraded: true, health: 'unknown' }],
      ['backends', []],
    ])
  })
})

// --- Liveness watchdog (RC-1 wave S4, design 05-§4.3-c) ----------------------
//
// Two INDEPENDENT staleness conditions, both measured against the same
// threshold: payload evidence (the `hb` success stamp / a `status` frame) and
// transport evidence (any named frame at all). The first catches a wedged
// collector behind a healthy stream — the case a frame-arrival watchdog cannot
// see, because `hb` itself keeps the frames arriving. The second catches a
// half-open connection whose reader has not ended yet.
describe('SseClient — liveness watchdog', () => {
  // Drives a live /api/events stream under fake timers — the endpoint WITH the
  // heartbeat contract, so the watchdog is armed. Then hands back the pushers.
  async function openClient(opts?: { staleAfterMs?: number }) {
    const s = openStream()
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(s.res))
    const got: Array<[string, unknown]> = []
    const sse = new SseClient('/api/events', (name, data) => got.push([name, data]), undefined, {
      heartbeat: true,
      ...opts,
    })
    void sse.connect()
    await vi.advanceTimersByTimeAsync(1) // let fetch + the reader loop settle
    return { sse, got, push: s.push, end: s.end }
  }

  it('defaults the threshold to 2.5 × the 25s server ping interval', () => {
    expect(DEFAULT_STALE_AFTER_MS).toBe(62_000)
  })

  // (a) PAYLOAD watchdog — the wave's reason to exist. `hb` keeps arriving on
  // the server's 25s cadence, so transport evidence is perfect; only the
  // success stamp inside it stands still. RED against a client that resets on
  // FRAME ARRIVAL (the Revision-1 specification): it stays 'open' forever.
  it('goes stale while hb keeps arriving with a FROZEN last_good_at', async () => {
    vi.useFakeTimers({ now: new Date('2026-07-28T10:00:00Z') })
    const { sse, push } = await openClient()
    expect(sse.status).toBe('open')

    const frozen = '2026-07-28T09:59:55Z' // the collector wedged 5s before connect
    for (let i = 0; i < 4; i++) {
      await vi.advanceTimersByTimeAsync(25_000) // the server's ping_interval
      push(hbFrame(frozen))
      await vi.advanceTimersByTimeAsync(1)
    }

    // 100s of perfect transport, 100s without a single successful collector
    // tick ⇒ the payload condition fires (5s watchdog granularity).
    expect(sse.status).toBe('stale')
    sse.close()
  })

  // (a, second half) 'stale' is ≠ 'open', so the page's UNCHANGED poll gate
  // reopens by itself. The gate is pinned against the page source: this test
  // is worthless if StatusPage stops asking `!== 'open'`.
  it("reopens the StatusPage poll gate, whose condition is still `sse.status === 'open'`", async () => {
    expect(statusPageSource).toContain("if (!session.admin || sse.status === 'open') return")

    vi.useFakeTimers({ now: new Date('2026-07-28T10:00:00Z') })
    const { sse, push } = await openClient()
    // While the stream delivers, the poll stays parked.
    expect(sse.status === 'open').toBe(true)

    const frozen = '2026-07-28T09:59:55Z'
    for (let i = 0; i < 4; i++) {
      await vi.advanceTimersByTimeAsync(25_000)
      push(hbFrame(frozen))
      await vi.advanceTimersByTimeAsync(1)
    }

    // The exact expression StatusPage's $effect evaluates — now false, so the
    // effect re-runs its body and the 5s poll interval is armed again.
    expect(sse.status === 'open').toBe(false)
    sse.close()
  })

  // (b) NO FALSE ALARM — the gate that kills the literally-copied hermes
  // watchdog. hermes waits for domain frames; in ctx silence is legitimate
  // (§3.1), so a client that only accepts `status` frames as evidence would
  // flip a perfectly healthy stream to 'stale' after 62s.
  it('never goes stale while hb carries an ADVANCING last_good_at and no status frame arrives', async () => {
    vi.useFakeTimers({ now: new Date('2026-07-28T10:00:00Z') })
    const { sse, got, push } = await openClient()

    let t = Date.parse('2026-07-28T10:00:00Z')
    for (let i = 0; i < 24; i++) {
      // 10 minutes = 24 ticks, four times the threshold.
      await vi.advanceTimersByTimeAsync(25_000)
      t += 25_000
      push(hbFrame(new Date(t).toISOString()))
      await vi.advanceTimersByTimeAsync(1)
      expect(sse.status).toBe('open')
    }

    expect(sse.status).toBe('open')
    // Not one status frame in the whole run — silence is not staleness.
    expect(got.some(([name]) => name === 'status')).toBe(false)
    sse.close()
  })

  // (c) TRANSPORT watchdog — the second, independent condition. No frame of any
  // kind (a half-open connection behind a proxy: the reader never ends).
  it('goes stale when NO frame of any kind arrives for longer than the threshold', async () => {
    vi.useFakeTimers({ now: new Date('2026-07-28T10:00:00Z') })
    const { sse } = await openClient()
    expect(sse.status).toBe('open')

    await vi.advanceTimersByTimeAsync(60_000)
    expect(sse.status).toBe('open') // still inside the threshold
    await vi.advanceTimersByTimeAsync(10_000)
    expect(sse.status).toBe('stale')
    sse.close()
  })

  // A `status` frame is payload evidence in its own right: the collector only
  // emits one when its snapshot actually changed. It clears a stale state.
  it('recovers from stale on the next frame carrying fresh evidence', async () => {
    vi.useFakeTimers({ now: new Date('2026-07-28T10:00:00Z') })
    const { sse, push } = await openClient()
    await vi.advanceTimersByTimeAsync(70_000)
    expect(sse.status).toBe('stale')

    push('event: status\ndata: {"as_of":"2026-07-28T10:01:10Z"}\n\n')
    await vi.advanceTimersByTimeAsync(1)
    expect(sse.status).toBe('open')
    sse.close()
  })

  // degraded > 0 raises the partial flag (a WORD on the display, never a colour
  // alone) — the ctx stand-in for hermes' never-rendered db_error.
  it('mirrors hb.degraded into the partial flag, in both directions', async () => {
    vi.useFakeTimers({ now: new Date('2026-07-28T10:00:00Z') })
    const { sse, push } = await openClient()
    expect(sse.partial).toBe(false)

    push(hbFrame('2026-07-28T10:00:00Z', 2, 'degraded'))
    await vi.advanceTimersByTimeAsync(1)
    expect(sse.partial).toBe(true)

    push(hbFrame('2026-07-28T10:00:25Z', 0, 'ok'))
    await vi.advanceTimersByTimeAsync(1)
    expect(sse.partial).toBe(false)
    sse.close()
  })

  // An `hb` WITHOUT a success stamp (last_good_at: null — the collector has not
  // had one good tick since start) is transport evidence only. A client that
  // took the frame itself as payload evidence would render 'live' forever on a
  // collector that never succeeded.
  it('does not accept an hb with last_good_at: null as payload evidence', async () => {
    vi.useFakeTimers({ now: new Date('2026-07-28T10:00:00Z') })
    const { sse, push } = await openClient()

    for (let i = 0; i < 4; i++) {
      await vi.advanceTimersByTimeAsync(25_000)
      push(hbFrame(null))
      await vi.advanceTimersByTimeAsync(1)
    }
    expect(sse.status).toBe('stale')
    sse.close()
  })

  // A reconnect must not inherit the old evidence clocks: after a 10-minute
  // outage the fresh stream is live, not instantly stale.
  it('resets the evidence clocks on a successful (re)connect', async () => {
    vi.useFakeTimers({ now: new Date('2026-07-28T10:00:00Z') })
    const { sse, end } = await openClient()
    await vi.advanceTimersByTimeAsync(70_000)
    expect(sse.status).toBe('stale')

    // Server ends the stream; the backoff reconnect lands on a fresh body.
    const next = openStream()
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(next.res))
    end()
    await vi.advanceTimersByTimeAsync(5_000) // ≥ the 1s backoff + jitter
    expect(sse.status).toBe('open')
    sse.close()
  })

  // BLAST-RADIUS GATE. This client also carries the workflow domain stream
  // (live.ts → GET /api/project/events), and THAT endpoint keeps its
  // connection alive with a bare ": ping" COMMENT (project_events.go:206),
  // which eventsource-parser never surfaces as an event. A watchdog armed on
  // every stream would therefore find zero frames on a quiet project, declare
  // it stale after 62s and re-arm the poll fallback on /issues and /board —
  // silently undoing the transport those pages were built for. The watchdog is
  // opt-in on the heartbeat contract, and only /api/events has one (S3).
  it('never goes stale on a stream that was not declared to heartbeat', async () => {
    vi.useFakeTimers({ now: new Date('2026-07-28T10:00:00Z') })
    const s = openStream()
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(s.res))
    const sse = new SseClient('/api/project/events', () => {})
    void sse.connect()
    await vi.advanceTimersByTimeAsync(1)
    expect(sse.status).toBe('open')

    await vi.advanceTimersByTimeAsync(600_000) // ten minutes of legitimate silence
    expect(sse.status).toBe('open')
    sse.close()
  })

  it('is armed at the /api/events call site, where the server sends hb', () => {
    // The opt-in only helps if the ONE stream with a heartbeat asks for it.
    expect(statusPageSource).toMatch(/new SseClient\('\/api\/events'[\s\S]{0,120}heartbeat: true/)
  })

  it('stops the watchdog on close (no stale flip after teardown)', async () => {
    vi.useFakeTimers({ now: new Date('2026-07-28T10:00:00Z') })
    const { sse } = await openClient()
    sse.close()
    await vi.advanceTimersByTimeAsync(120_000)
    expect(sse.status).toBe('closed')
  })

  // REGRESSION (Lead-Fix während der S4-Abnahme). Die Seite ruft connect() in
  // einem $effect mit Cleanup close(). Liest connect() dabei SYNCHRON die
  // status-Rune (die Erstfassung tat es für die 'error'-Weiche), hängt der
  // Effect an genau dem Zustand, den sein eigener Cleanup schreibt ('closed'):
  // Cleanup → 'closed' → Body liest 'closed', schreibt 'connecting' →
  // Invalidierung → Endlosschleife → effect_update_depth_exceeded, und mit dem
  // abgebrochenen Flush sterben ALLE Effects der Seite (e2e-Symptom: /status
  // dauerhaft "loading status…", Anzeige eingefroren auf 'connecting'; alle
  // sieben status-e2e-Tests rot).
  //
  // In DIESER Umgebung ist der Effect-Kontext nicht nachstellbar ($effect ist
  // im node-only Server-Build ein No-op — dieselbe Grenze wie bei mount(), s.
  // ConnState.svelte.test.ts Kopf). Das VERHALTENS-Gate ist deshalb der
  // e2e-Contract-Lauf (contract:status @flow), der den Defekt gefangen hat.
  // Hier wird die tragende Zeile gepinnt, mit demselben ?raw-Source-Muster wie
  // der Poll-Gate-Pin oben: die 'error'-Weiche entscheidet über das nicht-
  // reaktive #inBackoff-Spiegelfeld, nie über einen Read der status-Rune.
  it('decides the connecting/error fork over #inBackoff, never by reading the status rune', () => {
    expect(sseSource).toContain("if (!this.#inBackoff) this.status = 'connecting'")
    // The broken variant, verbatim — it must never come back.
    expect(sseSource).not.toContain("if (this.status !== 'error')")
  })

  // Die Display-Semantik der Weiche, wertbasiert: ein Retry im laufenden
  // Backoff bleibt 'error' (Anzeigewort 'reconnecting'), kein connecting-Flap
  // je Backoff-Runde — und erst ein GELUNGENER Connect verlässt den Backoff.
  it('keeps the error display through backoff retries and recovers to open on success', async () => {
    vi.useFakeTimers({ now: new Date('2026-07-28T10:00:00Z') })
    const fetchMock = vi.fn().mockRejectedValue(new Error('net::ERR_FAILED'))
    vi.stubGlobal('fetch', fetchMock)
    const sse = new SseClient('/api/events', () => {}, undefined, { heartbeat: true })

    void sse.connect()
    expect(sse.status).toBe('connecting') // first attempt announces itself
    // Settle the rejected fetch WITHOUT moving the clock: the first reconnect
    // timer is armed during this flush, so any advance here would shift the
    // band measurement below by exactly that amount (a 0 ms tick still drains
    // the microtask queue — it yields to a real macrotask once).
    await vi.advanceTimersByTimeAsync(0)
    expect(sse.status).toBe('error')

    // Two backoff rounds: exactly ONE retry per round, and the display never
    // flaps back to 'connecting'. A FIXED window over a JITTERED exponential
    // backoff (sse.svelte.ts:235 — retryMs * (0.5 + Math.random()), doubling to
    // a 30s cap) is a coin flip, and it flaked in CI both ways: a round whose
    // retries all landed early leaves the next delay outgrowing the 10s window
    // and asserts on zero calls (#31, run 32234106583: "expected 5 to be
    // greater than 5"), and the same drift pushes the pending wait past the 30s
    // recovery advance below (run 32608414735: "expected 'error' to be 'open'").
    // advanceTimersToNextTimerAsync runs the pending reconnect timer itself —
    // the only timer alive in this phase, since the watchdog is armed on the
    // success path (sse.svelte.ts:153) and this phase never connects.
    for (let i = 0; i < 2; i++) {
      const before = fetchMock.mock.calls.length
      const at = Date.now()
      await vi.advanceTimersToNextTimerAsync()
      const waited = Date.now() - at
      // Round i's pending wait was scheduled with retryMs = 1000 · 2^i, so it
      // sits in that step's jitter band — the behavioural pin on the schedule.
      expect(waited).toBeGreaterThanOrEqual(500 * 2 ** i)
      expect(waited).toBeLessThan(1_500 * 2 ** i)
      expect(fetchMock.mock.calls.length).toBe(before + 1)
      expect(sse.status).toBe('error')
    }

    // A succeeding connect leaves the backoff: 'open', and a LATER fresh
    // connect (after close) announces 'connecting' again. The 30s window is
    // deterministic once the loop above is: the pending wait was scheduled with
    // retryMs = 4_000 (which is why #retryMs reads 8_000 here), so it is
    // < 6_000 ms and always fires inside the advance.
    const s = openStream()
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(s.res))
    await vi.advanceTimersByTimeAsync(30_000)
    expect(sse.status).toBe('open')
    sse.close()
    expect(sse.status).toBe('closed')
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('down')))
    void sse.connect()
    expect(sse.status).toBe('connecting')
    sse.close()
  })
})
