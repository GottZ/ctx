// computeWindow / isNearBottom pins (design 04 §4.3/§5.5, wave U05). The DOM-cap
// gate (rendered rows < 200 at 10k items) is a runtime Playwright assert; these
// vitests pin the ARITHMETIC that makes it hold — a bounded rendered count that
// does NOT grow with `total`, correct spacer heights, and the near-bottom
// loadMore trigger.

import { describe, expect, it } from 'vitest'
import { computeWindow, isNearBottom } from './virtual-window'

describe('computeWindow', () => {
  it('renders O(viewport) rows regardless of total — the DOM-cap proof', () => {
    const rowHeight = 44
    const viewportHeight = 700 // ~16 visible rows
    const overscan = 8
    // The rendered count is bounded independent of `total`: 100 vs 10k vs 1M.
    const counts = [100, 10_000, 1_000_000].map((total) => {
      const w = computeWindow({ scrollTop: 0, viewportHeight, rowHeight, total, overscan })
      return w.end - w.start
    })
    // ceil(700/44)+1 + 2*8 = 17 + 16 = 33 (top overscan clamps to 0 at scrollTop 0).
    // From the top the upper overscan is clipped, so it is ≤ that bound.
    for (const n of counts) {
      expect(n).toBeGreaterThan(0)
      expect(n).toBeLessThan(200) // the §5.5 cap the runtime gate asserts
    }
    // Identical at 10k and 1M — the count is viewport-driven, not data-driven.
    expect(counts[1]).toBe(counts[2])
  })

  it('adds overscan above once scrolled past the top', () => {
    const w = computeWindow({ scrollTop: 44 * 100, viewportHeight: 700, rowHeight: 44, total: 10_000, overscan: 8 })
    const firstVisible = 100
    expect(w.start).toBe(firstVisible - 8)
    // padTop reconstructs the exact offset of the first rendered row.
    expect(w.padTop).toBe(w.start * 44)
    // padTop + rendered height + padBottom == the full virtual height.
    const renderedHeight = (w.end - w.start) * 44
    expect(w.padTop + renderedHeight + w.padBottom).toBe(10_000 * 44)
  })

  it('clamps the window to the data bounds at the very bottom', () => {
    const total = 10_000
    const rowHeight = 44
    const w = computeWindow({ scrollTop: rowHeight * total, viewportHeight: 700, rowHeight, total, overscan: 8 })
    expect(w.end).toBe(total)
    expect(w.padBottom).toBe(0)
    expect(w.start).toBeLessThan(total)
  })

  it('renders the overscan band even when the viewport is unmeasured (pre-mount)', () => {
    const w = computeWindow({ scrollTop: 0, viewportHeight: 0, rowHeight: 44, total: 10_000, overscan: 8 })
    // ceil(0/44)+1 + 8 = 9 rows — never a blank first paint.
    expect(w.end - w.start).toBeGreaterThan(0)
    expect(w.start).toBe(0)
  })

  it('returns an empty window for an empty list or a zero row height', () => {
    expect(computeWindow({ scrollTop: 0, viewportHeight: 700, rowHeight: 44, total: 0, overscan: 8 })).toEqual({
      start: 0,
      end: 0,
      padTop: 0,
      padBottom: 0,
    })
    expect(computeWindow({ scrollTop: 0, viewportHeight: 700, rowHeight: 0, total: 10, overscan: 8 }).end).toBe(0)
  })
})

describe('isNearBottom', () => {
  it('is true within the threshold of the scrollable bottom', () => {
    // scrollHeight 10000, viewport 700 → max scrollTop 9300.
    expect(isNearBottom(9000, 700, 10_000, 400)).toBe(true) // 300 left ≤ 400
    expect(isNearBottom(8000, 700, 10_000, 400)).toBe(false) // 1300 left > 400
  })

  it('is false for an unmeasured scroller', () => {
    expect(isNearBottom(0, 0, 0, 400)).toBe(false)
  })
})
