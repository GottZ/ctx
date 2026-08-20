// DreamBackoffModel gates. The load-bearing one is the refetch throttle: the
// dream-stats source is an O(n) GROUP BY server-side, so the model must fetch
// exactly once per moved last_cycle_at stamp outside the quiet window — never
// per frame (frames arrive every tick), and a stamp that moves INSIDE the
// window must still be picked up by a later frame (dirty carry-over).

import { describe, expect, it } from 'vitest'
import type { DreamStatsResponse } from '../../lib/api/types'
import { DreamBackoffModel, fmtHours } from './dream-backoff.svelte'

function stats(): DreamStatsResponse {
  return {
    success: true,
    action: 'dream-stats',
    total_blocks: 100,
    dream_checked: 80,
    dream_links: 40,
    coverage_pct: 80,
    unchecked: 20,
    pending_recheck: 3,
    backoff: {
      mode: 'exp',
      factor: 1.6,
      grace: 1,
      min_hours: 12,
      cap_hours: 1080,
      inert_offset: 2,
      max_eval_count: 4,
      truncated: false,
      levels: [
        { eval_count: 0, blocks: 20, cooldown_hours: 12 },
        { eval_count: 4, blocks: 5, cooldown_hours: 49 },
      ],
    },
  }
}

/** Model with a counting fetcher and a manual clock. */
function build(opts: { fail?: () => boolean } = {}) {
  let nowMs = 0
  let calls = 0
  const model = new DreamBackoffModel(
    () => {
      calls++
      return opts.fail?.() ? Promise.reject(new Error('boom')) : Promise.resolve(stats())
    },
    { minIntervalMs: 30_000, now: () => nowMs },
  )
  return {
    model,
    calls: () => calls,
    tick: (ms: number) => {
      nowMs += ms
    },
  }
}

const flush = () => Promise.resolve()

describe('DreamBackoffModel refetch throttle', () => {
  it('fetches once on first sync, even with a null stamp (dream never ran)', async () => {
    const { model, calls } = build()
    model.sync(null)
    await flush()
    expect(calls()).toBe(1)
    expect(model.data?.backoff.levels).toHaveLength(2)

    // identical frames keep arriving — no further fetch
    model.sync(null)
    model.sync(null)
    await flush()
    expect(calls()).toBe(1)
  })

  it('refetches when the stamp moves outside the quiet window, not inside it', async () => {
    const { model, calls, tick } = build()
    model.sync('t0')
    await flush()
    expect(calls()).toBe(1)

    // stamp moves 5s later — inside the window: no fetch yet, but dirty
    tick(5_000)
    model.sync('t1')
    await flush()
    expect(calls()).toBe(1)

    // next frame after the window carries the dirty flag into a fetch,
    // WITHOUT the stamp moving again
    tick(30_000)
    model.sync('t1')
    await flush()
    expect(calls()).toBe(2)

    // and a clean stamp after that stays quiet
    tick(60_000)
    model.sync('t1')
    await flush()
    expect(calls()).toBe(2)
  })

  it('a failed fetch keeps old data, reports the error, and retries via the frame stream', async () => {
    let failing = false
    const { model, calls, tick } = build({ fail: () => failing })
    model.sync('t0')
    await flush()
    expect(model.status).toBe('ready')

    failing = true
    tick(60_000)
    model.sync('t1')
    await flush()
    await flush()
    expect(calls()).toBe(2)
    expect(model.status).toBe('error')
    expect(model.data?.total_blocks).toBe(100) // held, not cleared

    // recovery: same stamp, next frame after the window retries (dirty set on error)
    failing = false
    tick(60_000)
    model.sync('t1')
    await flush()
    await flush()
    expect(calls()).toBe(3)
    expect(model.status).toBe('ready')
    expect(model.error).toBeNull()
  })
})

describe('fmtHours (CLI fmtDuration parity)', () => {
  it('renders h below a day, tenths of days below 10d, whole days above', () => {
    expect(fmtHours(12)).toBe('12h')
    expect(fmtHours(23.6)).toBe('24h')
    expect(fmtHours(24)).toBe('1.0d')
    expect(fmtHours(49)).toBe('2.0d')
    expect(fmtHours(1080)).toBe('45d')
  })
})
