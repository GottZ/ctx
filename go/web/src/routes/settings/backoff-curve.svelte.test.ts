// Back-off curve model tests: the Go-mirror of effectiveCooldownHours
// (values pinned against dream.BackoffConfig semantics), the hours
// parse/render round-trip, the drag-time factor solver, and the save
// watcher's one-restamp-per-save contract.

import { describe, expect, it, vi } from 'vitest'
import type { DreamBackoffRestampResponse, DreamStatsResponse } from '../../lib/api/types'
import {
  BackoffSaveWatcher,
  bezierPath,
  cooldownHours,
  fmtHoursDraft,
  parseHours,
  policyFromDrafts,
  solveFactor,
  xAxisMax,
  type BackoffPolicy,
} from './backoff-curve.svelte'

const DEFAULTS: BackoffPolicy = { mode: 'exp', factor: 1.6, grace: 0, minHours: 12, capHours: 45 * 24, inertOffset: 7 }

describe('cooldownHours — Go mirror', () => {
  it('exp: min * factor^n, floored and capped', () => {
    expect(cooldownHours(DEFAULTS, 0, false)).toBeCloseTo(12)
    expect(cooldownHours(DEFAULTS, 1, false)).toBeCloseTo(19.2)
    expect(cooldownHours(DEFAULTS, 5, false)).toBeCloseTo(12 * 1.6 ** 5)
    expect(cooldownHours(DEFAULTS, 40, false)).toBe(45 * 24)
  })

  it('inert shifts up the same curve by the offset', () => {
    expect(cooldownHours(DEFAULTS, 2, true)).toBeCloseTo(12 * 1.6 ** 9)
  })

  it('grace zeroes the exponent below the plateau', () => {
    const p = { ...DEFAULTS, grace: 3 }
    expect(cooldownHours(p, 0, false)).toBeCloseTo(12)
    expect(cooldownHours(p, 3, false)).toBeCloseTo(12)
    expect(cooldownHours(p, 4, false)).toBeCloseTo(19.2)
  })

  it('log and linear shapes', () => {
    const log = { ...DEFAULTS, mode: 'log' as const, factor: 3 }
    expect(cooldownHours(log, 1, false)).toBeCloseTo(12 * (1 + 3 * Math.log(2)))
    const lin = { ...DEFAULTS, mode: 'linear' as const, factor: 2 }
    expect(cooldownHours(lin, 2, false)).toBeCloseTo(12 + 2 * 24 * 2)
  })

  it('off uses the fixed active/inert days', () => {
    const off = { ...DEFAULTS, mode: 'off' as const }
    expect(cooldownHours(off, 9, false)).toBe(72)
    expect(cooldownHours(off, 9, true)).toBe(336)
  })

  it('floors at 1h', () => {
    const p = { ...DEFAULTS, minHours: 0.1 }
    expect(cooldownHours(p, 0, false)).toBe(1)
  })
})

describe('parseHours / fmtHoursDraft — config mirror', () => {
  it('parses suffixes and bare hours', () => {
    expect(parseHours('12h')).toBe(12)
    expect(parseHours('45d')).toBe(45 * 24)
    expect(parseHours('1w')).toBe(168)
    expect(parseHours('36')).toBe(36)
    expect(parseHours('1.5h')).toBe(1.5)
  })

  it('rejects malformed and negative values', () => {
    expect(parseHours('')).toBeNull()
    expect(parseHours('-3h')).toBeNull()
    expect(parseHours('abc')).toBeNull()
  })

  it('renders whole-day multiples as days, else hours', () => {
    expect(fmtHoursDraft(45 * 24)).toBe('45d')
    expect(fmtHoursDraft(12)).toBe('12h')
    expect(fmtHoursDraft(13.46)).toBe('13.5h')
  })

  it('round-trips through the parser', () => {
    for (const h of [1, 12, 36, 240, 45 * 24]) {
      expect(parseHours(fmtHoursDraft(h))).toBeCloseTo(h)
    }
  })
})

describe('policyFromDrafts', () => {
  const drafts: Record<string, string> = {
    'dream.backoff_mode': 'exp',
    'dream.backoff_factor': '1.6',
    'dream.backoff_grace': '0',
    'dream.backoff_cap': '45d',
    'dream.backoff_min': '12h',
    'dream.backoff_inert_offset': '7',
  }

  it('builds the policy from valid drafts', () => {
    const { policy, invalid } = policyFromDrafts((k) => drafts[k])
    expect(invalid).toEqual([])
    expect(policy).toEqual(DEFAULTS)
  })

  it('names invalid drafts and yields no policy', () => {
    const { policy, invalid } = policyFromDrafts((k) => (k === 'dream.backoff_factor' ? 'junk' : drafts[k]))
    expect(policy).toBeNull()
    expect(invalid).toEqual(['dream.backoff_factor'])
  })
})

describe('solveFactor — drag inverse', () => {
  it('inverts exp through a curve point', () => {
    const f = solveFactor(DEFAULTS, 5, 12 * 2 ** 5)
    expect(f).toBeCloseTo(2)
    expect(cooldownHours({ ...DEFAULTS, factor: f as number }, 5, false)).toBeCloseTo(12 * 2 ** 5)
  })

  it('inverts linear', () => {
    const p = { ...DEFAULTS, mode: 'linear' as const }
    expect(solveFactor(p, 2, 12 + 24 * 2 * 1.5)).toBeCloseTo(1.5)
  })

  it('refuses the grace plateau, off mode and sub-min targets', () => {
    expect(solveFactor({ ...DEFAULTS, grace: 5 }, 4, 100)).toBeNull()
    expect(solveFactor({ ...DEFAULTS, mode: 'off' }, 5, 100)).toBeNull()
    expect(solveFactor(DEFAULTS, 5, 6)).toBeNull()
  })
})

describe('axis + path helpers', () => {
  it('extends the axis past the cap knee and the corpus max', () => {
    expect(xAxisMax(DEFAULTS, 0)).toBeGreaterThanOrEqual(12)
    expect(xAxisMax(DEFAULTS, 30)).toBeGreaterThanOrEqual(34)
    expect(xAxisMax(DEFAULTS, 500)).toBe(80)
  })

  it('builds a bezier path through the points', () => {
    const d = bezierPath([
      { x: 0, y: 10 },
      { x: 10, y: 5 },
      { x: 20, y: 1 },
    ])
    expect(d.startsWith('M 0')).toBe(true)
    expect(d.match(/C /g)).toHaveLength(2)
  })
})

describe('BackoffSaveWatcher', () => {
  const statsResponse = { success: true, backoff: { levels: [], max_eval_count: 0 } } as unknown as DreamStatsResponse
  const restampResponse: DreamBackoffRestampResponse = {
    success: true,
    action: 'dream-backoff-restamp',
    restamped: 42,
    skipped_transient: 1,
  }

  function makeWatcher() {
    const restamp = vi.fn(async () => restampResponse)
    const fetchStats = vi.fn(async () => statsResponse)
    const watcher = new BackoffSaveWatcher({ restamp, fetchStats }, 10)
    return { watcher, restamp, fetchStats }
  }

  it('loads stats initially without restamping', async () => {
    const { watcher, restamp, fetchStats } = makeWatcher()
    await watcher.loadStats()
    expect(fetchStats).toHaveBeenCalledTimes(1)
    expect(restamp).not.toHaveBeenCalled()
    expect(watcher.stats).toBe(statsResponse)
  })

  it('arms on the first fingerprint and restamps once on a change', async () => {
    vi.useFakeTimers()
    const { watcher, restamp } = makeWatcher()
    watcher.sync('a|b|c')
    watcher.sync('a|b|c')
    await vi.runAllTimersAsync()
    expect(restamp).not.toHaveBeenCalled()

    watcher.sync('a|b|CHANGED')
    await vi.runAllTimersAsync()
    expect(restamp).toHaveBeenCalledTimes(1)
    expect(watcher.phase).toBe('done')
    expect(watcher.restamped?.restamped).toBe(42)
    vi.useRealTimers()
  })

  it('debounces the sequential PUTs of one save into one restamp', async () => {
    vi.useFakeTimers()
    const { watcher, restamp } = makeWatcher()
    watcher.sync('v1')
    watcher.sync('v2')
    watcher.sync('v3')
    watcher.sync('v4')
    await vi.runAllTimersAsync()
    expect(restamp).toHaveBeenCalledTimes(1)
    vi.useRealTimers()
  })

  it('surfaces a restamp failure', async () => {
    vi.useFakeTimers()
    const restamp = vi.fn(async () => {
      throw new Error('boom')
    })
    const fetchStats = vi.fn(async () => statsResponse)
    const watcher = new BackoffSaveWatcher({ restamp, fetchStats }, 10)
    watcher.sync('a')
    watcher.sync('b')
    await vi.runAllTimersAsync()
    expect(watcher.phase).toBe('error')
    expect(watcher.errorMessage).toContain('boom')
    vi.useRealTimers()
  })
})
