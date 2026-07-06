// Saturation state-machine tests (MW8b, DECISIONS B2): the pre-stream HTTP 429
// path through send() — countdown + jittered auto-retry when Retry-After is
// present, a generic manual-retry notice when it is absent, and cancel. The
// streamer and session API are mocked so no network runs; fake timers drive the
// countdown deterministically. The reducer-level queued/saturated cases live in
// store.svelte.test.ts (no mocks needed there).

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from '../api'

vi.mock('./stream', () => ({ streamTurn: vi.fn() }))
vi.mock('./api', () => ({
  listSessions: vi.fn().mockResolvedValue({ success: true, sessions: [] }),
  getSession: vi.fn(),
  deleteSession: vi.fn(),
}))

import { streamTurn } from './stream'
import { ChatStore } from './store.svelte'

const streamMock = vi.mocked(streamTurn)

function reject429(retryAfter: number | null): ApiError {
  return new ApiError(429, 'rate_limited', 'server busy — retry shortly', null, retryAfter !== null ? { retry_after: retryAfter } : null)
}

describe('ChatStore saturation (B2)', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    streamMock.mockReset()
  })
  afterEach(() => vi.useRealTimers())

  it('enters a countdown on a 429 with Retry-After and auto-retries on expiry', async () => {
    streamMock.mockRejectedValueOnce(reject429(2)) // first turn: rejected
    streamMock.mockResolvedValueOnce(undefined) //     auto-retry: clean

    const s = new ChatStore(() => 'k')
    await s.send('hello')

    expect(s.saturation).toEqual({ retryAfter: 2, secondsLeft: 2 })
    expect(s.streaming).toBe(false)
    expect(s.turnError).toBeNull()
    expect(streamMock).toHaveBeenCalledTimes(1)

    // Past the (≤1s-jittered) 2s deadline the store re-sends the same turn.
    await vi.advanceTimersByTimeAsync(3200)
    expect(streamMock).toHaveBeenCalledTimes(2)
    expect(s.saturation).toBeNull()
  })

  it('ticks the countdown down toward the retry', async () => {
    streamMock.mockRejectedValueOnce(reject429(5))
    streamMock.mockResolvedValue(undefined)
    const s = new ChatStore(() => 'k')
    await s.send('hi')

    expect(s.saturation?.secondsLeft).toBe(5)
    await vi.advanceTimersByTimeAsync(2000)
    expect(s.saturation?.secondsLeft).toBeLessThan(5) // ticked down
    expect(s.saturation?.secondsLeft).toBeGreaterThan(0) // not yet fired
  })

  it('shows a generic notice with no auto-retry when Retry-After is absent', async () => {
    streamMock.mockRejectedValueOnce(reject429(null))
    const s = new ChatStore(() => 'k')
    await s.send('hi')

    expect(s.saturation).toEqual({ retryAfter: null, secondsLeft: null })
    await vi.advanceTimersByTimeAsync(60_000)
    expect(streamMock).toHaveBeenCalledTimes(1) // never blind-hammers
  })

  it('cancelSaturation clears the notice, the timer and the pending turn', async () => {
    streamMock.mockRejectedValueOnce(reject429(5))
    const s = new ChatStore(() => 'k')
    await s.send('hi')
    expect(s.saturation).not.toBeNull()

    s.cancelSaturation()
    expect(s.saturation).toBeNull()
    await vi.advanceTimersByTimeAsync(10_000)
    expect(streamMock).toHaveBeenCalledTimes(1) // canceled auto-retry never fired
  })

  it('retryLast re-sends immediately (manual "Retry now")', async () => {
    streamMock.mockRejectedValueOnce(reject429(30)) // long wait — proves it is manual
    streamMock.mockResolvedValueOnce(undefined)
    const s = new ChatStore(() => 'k')
    await s.send('hi')
    expect(streamMock).toHaveBeenCalledTimes(1)

    await s.retryLast()
    expect(streamMock).toHaveBeenCalledTimes(2)
    expect(s.saturation).toBeNull()
  })

  it('drops the optimistic user message on a 429 (no dangling unsent bubble)', async () => {
    streamMock.mockRejectedValueOnce(reject429(3))
    const s = new ChatStore(() => 'k')
    await s.send('hi')
    expect(s.messages.filter((m) => m.seq === -1)).toHaveLength(0)
  })

  it('a non-429 pre-stream failure stays a hard turn error, not a saturation', async () => {
    streamMock.mockRejectedValueOnce(new ApiError(503, 'server', 'backend down'))
    const s = new ChatStore(() => 'k')
    await s.send('hi')
    expect(s.saturation).toBeNull()
    expect(s.turnError).toBe('backend down')
  })
})
