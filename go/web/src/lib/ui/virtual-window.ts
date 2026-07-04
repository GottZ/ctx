// Fixed-height row windowing (design 04 §4.3, wave U05). virtua is NOT in the
// repo (bun.lock carries no virtua/@tanstack — the U05 briefing's "virtua ist
// etabliert" premise is stale, verified 2026-07-04) and the U05 gate forbids a
// new dependency (frozen-lockfile clean). So the /issues list virtualises with
// a hand-rolled fixed-height window OVER the Q10 Table primitive instead of a
// library VList: the same DOM-cap guarantee (design 04 §5.5: rendered rows <
// 200 at 10k items), no lockfile drift.
//
// The design already MANDATES fixed row heights ("Zeilen fester Höhe … die
// flake-ärmste Virtualisierungs-Basis", §4.3), which is exactly what makes a
// library unnecessary: with a constant row height the visible slice is pure
// arithmetic. This module is that arithmetic — DOM-free, unit-tested — and the
// list component renders a top/bottom spacer <tr> plus the windowed rows into
// the Table's `children` snippet (the consumer owns its iteration, Table.svelte
// header). No eviction (§4.2 holds ALL loaded rows), so there is no index shift
// and the scroll geometry stays stable.

export interface VirtualWindow {
  /** First rendered row index (inclusive), clamped to [0, total]. */
  start: number
  /** One-past-last rendered row index (exclusive), clamped to [start, total]. */
  end: number
  /** Spacer height (px) standing in for the rows before `start`. */
  padTop: number
  /** Spacer height (px) standing in for the rows after `end`. */
  padBottom: number
}

export interface VirtualWindowInput {
  /** Scroll offset of the viewport in px (scroller.scrollTop). */
  scrollTop: number
  /** Visible viewport height in px (scroller.clientHeight). */
  viewportHeight: number
  /** Constant per-row height in px (must be > 0). */
  rowHeight: number
  /** Total number of rows in the backing data (NOT the DOM). */
  total: number
  /** Extra rows rendered above AND below the viewport (scroll-jank buffer). */
  overscan: number
}

/**
 * The rows that must be in the DOM for the current scroll offset, plus the two
 * spacer heights that keep the scrollbar geometry honest. The rendered count is
 * `end - start ≤ ceil(viewportHeight / rowHeight) + 2·overscan + 1` — a bound
 * INDEPENDENT of `total`, which is the whole point: 10k rows render O(viewport)
 * DOM nodes. A zero/negative viewport (unmeasured, pre-mount) still renders the
 * overscan band so the first paint is never blank.
 */
export function computeWindow(input: VirtualWindowInput): VirtualWindow {
  const { scrollTop, viewportHeight, rowHeight, total, overscan } = input
  if (rowHeight <= 0 || total <= 0) {
    return { start: 0, end: 0, padTop: 0, padBottom: 0 }
  }
  const safeScroll = Math.max(0, scrollTop)
  const safeViewport = Math.max(0, viewportHeight)
  const firstVisible = Math.floor(safeScroll / rowHeight)
  const visibleCount = Math.ceil(safeViewport / rowHeight) + 1 // +1: partial row at the bottom edge
  const start = Math.max(0, firstVisible - overscan)
  const end = Math.min(total, firstVisible + visibleCount + overscan)
  return {
    start,
    end: Math.max(start, end),
    padTop: start * rowHeight,
    padBottom: Math.max(0, (total - end) * rowHeight),
  }
}

/**
 * True when the viewport is within `thresholdPx` of the scrollable bottom — the
 * signal the list uses to fire the keyset `loadMore()` before the user hits the
 * hard end (design 04 §6.1: scroll-end reaches the cursor nachladung). Guards
 * an unmeasured scroller (scrollHeight 0) as "not near".
 */
export function isNearBottom(
  scrollTop: number,
  viewportHeight: number,
  scrollHeight: number,
  thresholdPx: number,
): boolean {
  if (scrollHeight <= 0) return false
  return scrollHeight - (scrollTop + viewportHeight) <= thresholdPx
}
