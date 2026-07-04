// LiveSource gates (design 04 §7-U13 / §3.1 / Achse-03-W9). The SSE domain-event
// stream drives a DEBOUNCED, COALESCED refetch: a project frame yields a targeted
// batch, an issues-bulk frame yields exactly ONE full batch (never count-many),
// a burst of frames in one window collapses to ONE batch, and an SSE outage lets
// the poll fallback drive full refetches. All DOM-free via an injected connection
// + fake timers (the vitest env is node — no document, no live EventSource).
//
// Wire truth (go/internal/events/project_hub.go:69-73 + handler/project_events.go):
//   - SSE event name is always 'project' for domain frames, 'error' on revoke.
//   - JSON kind is the block TYPE (default 'issue') for an id-list frame,
//     'issues-bulk' for the coalesced burst frame (count, NO block_ids),
//     'resync' after a listener-reconnect gap. The briefing's {kind:'project'}
//     conflated the SSE event NAME with the frame kind — the gates below pin the
//     ACTUAL wire.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { LiveSource, type LiveBatch, type LiveConnection } from './live'
import type { SseStatus } from '../sse.svelte'

/** A controllable fake connection: tests push frames via emit() and flip status. */
class FakeConn implements LiveConnection {
  status: SseStatus = 'idle'
  connected = 0
  closed = 0
  #onEvent: (name: string, data: unknown) => void
  constructor(onEvent: (name: string, data: unknown) => void) {
    this.#onEvent = onEvent
  }
  connect(): void {
    this.connected += 1
    this.status = 'open'
  }
  close(): void {
    this.closed += 1
    this.status = 'closed'
  }
  /** Simulate the server pushing one SSE frame to the client. */
  emit(name: string, data: unknown): void {
    this.#onEvent(name, data)
  }
}

/** Build a LiveSource wired to a FakeConn the test keeps a handle on. */
function harness(opts: { pollMs?: number; debounceMs?: number; isVisible?: () => boolean } = {}) {
  const batches: LiveBatch[] = []
  let conn!: FakeConn
  const live = new LiveSource({
    onBatch: (b) => batches.push(b),
    connect: (onEvent) => {
      conn = new FakeConn(onEvent)
      return conn
    },
    pollMs: opts.pollMs ?? 10_000,
    debounceMs: opts.debounceMs ?? 500,
    isVisible: opts.isVisible,
  })
  live.start()
  return { live, batches, conn: () => conn }
}

beforeEach(() => {
  vi.useFakeTimers()
})
afterEach(() => {
  vi.useRealTimers()
})

describe('LiveSource — targeted refetch (gate: project frame -> gezielter Refetch)', () => {
  it('an id-list project frame yields a targeted batch (ids, not full)', () => {
    const h = harness()
    // Wire truth: event name 'project', kind = block type ('issue'), block_ids set.
    h.conn().emit('project', { kind: 'issue', project_id: 'p1', op: 'update', block_ids: ['id-x'] })
    vi.advanceTimersByTime(500)

    expect(h.batches).toHaveLength(1)
    expect(h.batches[0]).toEqual({ full: false, ids: ['id-x'] })
    h.live.stop()
  })

  it('distinct affected ids across frames in one window are unioned, deduped', () => {
    const h = harness()
    h.conn().emit('project', { kind: 'issue', op: 'update', block_ids: ['a', 'b'] })
    h.conn().emit('project', { kind: 'comment', op: 'insert', block_ids: ['b', 'c'] })
    vi.advanceTimersByTime(500)

    expect(h.batches).toHaveLength(1)
    expect(h.batches[0].full).toBe(false)
    expect([...h.batches[0].ids].sort()).toEqual(['a', 'b', 'c'])
    h.live.stop()
  })
})

describe('LiveSource — bulk coalescing (gate: issues-bulk -> genau EIN Refetch)', () => {
  it('a single issues-bulk frame (count=25) yields exactly ONE full batch, not count-many', () => {
    const h = harness()
    // The coalesced burst frame carries a COUNT and NO block_ids (project_hub.go:363).
    h.conn().emit('project', { kind: 'issues-bulk', project_id: 'p1', count: 25 })
    vi.advanceTimersByTime(500)

    expect(h.batches).toHaveLength(1) // RED under naive per-count handling -> 25
    expect(h.batches[0]).toEqual({ full: true, ids: [] })
    h.live.stop()
  })

  it('a resync frame yields one full batch', () => {
    const h = harness()
    h.conn().emit('project', { kind: 'resync' })
    vi.advanceTimersByTime(500)

    expect(h.batches).toEqual([{ full: true, ids: [] }])
    h.live.stop()
  })
})

describe('LiveSource — debounce (gate: 5 frames im Fenster -> 1 Refetch)', () => {
  it('five frames inside the debounce window collapse to ONE batch', () => {
    const h = harness({ debounceMs: 500 })
    for (let i = 0; i < 5; i++) {
      h.conn().emit('project', { kind: 'issue', op: 'update', block_ids: [`id-${i}`] })
      vi.advanceTimersByTime(50) // all within the 500ms window
    }
    expect(h.batches).toHaveLength(0) // nothing fired yet — still coalescing
    vi.advanceTimersByTime(500)

    expect(h.batches).toHaveLength(1) // RED under naive per-frame handling -> 5
    expect([...h.batches[0].ids].sort()).toEqual(['id-0', 'id-1', 'id-2', 'id-3', 'id-4'])
    h.live.stop()
  })

  it('frames in SEPARATE windows fire separately (the window resets after a flush)', () => {
    const h = harness({ debounceMs: 500 })
    h.conn().emit('project', { kind: 'issue', op: 'update', block_ids: ['a'] })
    vi.advanceTimersByTime(500)
    h.conn().emit('project', { kind: 'issue', op: 'update', block_ids: ['b'] })
    vi.advanceTimersByTime(500)

    expect(h.batches).toEqual([
      { full: false, ids: ['a'] },
      { full: false, ids: ['b'] },
    ])
    h.live.stop()
  })
})

describe('LiveSource — poll fallback (gate: SSE-Abort -> Poll-Pfad weiter gruen)', () => {
  it('polls a full refetch while the stream is NOT open', () => {
    const h = harness({ pollMs: 10_000 })
    h.conn().status = 'error' // the SSE aborted / 429 cap / revoked
    vi.advanceTimersByTime(10_000)

    expect(h.batches).toEqual([{ full: true, ids: [] }])
    h.live.stop()
  })

  it('does NOT poll while the stream is open (live frames drive it instead)', () => {
    const h = harness({ pollMs: 10_000 })
    h.conn().status = 'open'
    vi.advanceTimersByTime(30_000)

    expect(h.batches).toHaveLength(0)
    h.live.stop()
  })

  it('does NOT poll while the tab is hidden (visibility guard)', () => {
    const h = harness({ pollMs: 10_000, isVisible: () => false })
    h.conn().status = 'error'
    vi.advanceTimersByTime(30_000)

    expect(h.batches).toHaveLength(0)
    h.live.stop()
  })

  it('the error SSE frame is ignored (revocation is handled by poll + the 401 path)', () => {
    const h = harness()
    h.conn().emit('error', { error: 'session revoked' })
    vi.advanceTimersByTime(500)

    expect(h.batches).toHaveLength(0)
    h.live.stop()
  })
})

describe('LiveSource — lifecycle', () => {
  it('start connects once, stop closes and halts the poll', () => {
    const h = harness({ pollMs: 10_000 })
    expect(h.conn().connected).toBe(1)
    h.live.stop()
    expect(h.conn().closed).toBe(1)
    vi.advanceTimersByTime(60_000)
    expect(h.batches).toHaveLength(0) // no poll after stop
  })
})
