// UsageModel gates (RC-D C6 S1, W05-10): the window drives the request,
// complete/hist are the server's flags carried unchanged (null stays null),
// polling only while visible (design/05 §4.7: manual + 60 s). Red against
// the S1 stub. S2: the fixture follows C2's llmusage.Result (group string,
// series without label/op, queue_/ttft_ percentile names, rejected list|null).

import { afterEach, describe, expect, it, vi } from 'vitest'
import type { UsageResponse } from '../../lib/api/types'
import type { VisibilitySource } from './consumers.svelte'
import { USAGE_POLL_MS, UsageModel } from './usage.svelte'

function answer(over: Partial<UsageResponse> = {}): UsageResponse {
  return {
    success: true,
    from: '2026-06-28T12:00:00Z',
    to: '2026-06-29T12:00:00Z',
    bucket: 'hour',
    group: 'consumer',
    complete: false,
    hist: false,
    series: [
      {
        key: 'c1',
        points: [
          {
            bucket_start: '2026-06-29T11:00:00Z',
            complete: false,
            calls: null,
            errors: null,
            estimated_calls: null,
            prompt_tokens: null,
            completion_tokens: null,
            gpu_ms: null,
            avg_ms: null,
            p50_ms: null,
            p95_ms: null,
            queue_p50_ms: null,
            queue_p95_ms: null,
            ttft_p50_ms: null,
            ttft_p95_ms: null,
            cost_usd: null,
            rejected: null,
          },
        ],
      },
    ],
    ...over,
  }
}

function visibility(initial: boolean): { source: VisibilitySource; set: (v: boolean) => void } {
  let state = initial
  let listener: (() => void) | null = null
  return {
    source: {
      visible: () => state,
      subscribe(on) {
        listener = on
        return () => (listener = null)
      },
    },
    set(v) {
      state = v
      listener?.()
    },
  }
}

afterEach(() => vi.useRealTimers())

describe('UsageModel (W05-10)', () => {
  it('reads the consumer + window and carries complete/hist unchanged, nulls stay null', async () => {
    const usage = vi.fn().mockResolvedValue(answer())
    const m = new UsageModel('c1', { usage })
    await m.load()
    expect(usage.mock.calls[0]?.[0], 'gate W05-10: consumer + window on the request').toEqual({ consumer: 'c1', window: '24h' })
    expect(m.status).toBe('ready')
    expect(m.complete, 'gate W05-10: complete:false is the server’s word').toBe(false)
    expect(m.hist, 'gate W05-10: hist:false carried').toBe(false)
    expect(m.series[0]?.points[0]?.calls, 'gate W05-10: null stays null (never 0)').toBeNull()
  })

  it('setWindow re-reads with the new window', async () => {
    const usage = vi.fn().mockResolvedValue(answer())
    const m = new UsageModel('c1', { usage })
    await m.setWindow('90d')
    expect(usage.mock.calls.at(-1)?.[0], 'gate W05-10: window switch').toEqual({ consumer: 'c1', window: '90d' })
    expect(m.window).toBe('90d')
    expect(m.status).toBe('ready')
  })

  it('polls every 60 s only while visible; dispose stops it', async () => {
    vi.useFakeTimers()
    const usage = vi.fn().mockResolvedValue(answer())
    const vis = visibility(false)
    const m = new UsageModel('c1', { usage }, vis.source)
    m.start()
    await vi.advanceTimersByTimeAsync(3 * USAGE_POLL_MS)
    const hidden = usage.mock.calls.length
    vis.set(true)
    await vi.advanceTimersByTimeAsync(2 * USAGE_POLL_MS)
    expect(usage.mock.calls.length - hidden, 'gate W05-10: visible ⇒ polls at 60 s').toBeGreaterThanOrEqual(2)
    expect(hidden, 'gate: hidden tab ⇒ at most the initial read').toBeLessThanOrEqual(1)
    m.dispose()
    const after = usage.mock.calls.length
    await vi.advanceTimersByTimeAsync(3 * USAGE_POLL_MS)
    expect(usage.mock.calls.length, 'dispose stops the poll').toBe(after)
  })
})
