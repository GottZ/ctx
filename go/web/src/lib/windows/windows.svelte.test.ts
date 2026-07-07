// WindowStore gates (design 07 Wave G5a): open → dedup by UUID, z-order on
// focus, openIds-Set, minimize/restore, close → list removal + focus-return on
// triggerEl bzw. fallbackFocusEl, closeAll → empty, setSurface → open places in
// the set surface, move → clampPos (titlebar stays grabbable, NOT full
// containment), resize → clampSize. Pure runes store, no DOM: a focus-tracking
// stub stands in for the HTMLElement focus targets (vitest env = node).

import { describe, expect, it } from 'vitest'
import { MIN_H_LU, MIN_VISIBLE_LU, MIN_W_LU, type SurfaceMetrics } from './window-projection'
import { WindowStore } from './windows.svelte'

/** Minimal focus-tracking stand-in for an HTMLElement (no DOM in node env). */
function focusTarget() {
  const t = { focused: false, focus() { t.focused = true } }
  return t as { focused: boolean; focus(): void } & HTMLElement
}

const A = '550e8400-e29b-41d4-a716-446655440001'
const B = '550e8400-e29b-41d4-a716-446655440002'
const C = '550e8400-e29b-41d4-a716-446655440003'

describe('WindowStore open / dedup', () => {
  it('open adds a window and surfaces it in openIds', () => {
    const s = new WindowStore()
    s.open(A)
    expect(s.wins).toHaveLength(1)
    expect(s.openIds.has(A)).toBe(true)
    expect(s.wins[0]).toMatchObject({ id: A, minimized: false })
  })

  it('dedups by UUID — opening the same id twice keeps ONE window', () => {
    const s = new WindowStore()
    s.open(A)
    s.open(B)
    s.open(A) // duplicate
    expect(s.wins).toHaveLength(2)
    expect([...s.openIds].sort()).toEqual([A, B].sort())
  })

  it('re-opening a minimized window restores AND focuses it (dedup → restore)', () => {
    const s = new WindowStore()
    s.open(A)
    s.open(B)
    s.minimize(A)
    expect(s.topId).toBe(B)
    s.open(A) // dedup hits restore: un-minimize + raise
    expect(s.wins.find((w) => w.id === A)?.minimized).toBe(false)
    expect(s.topId).toBe(A)
  })
})

describe('WindowStore z-order / topId', () => {
  it('the most recently opened window is on top', () => {
    const s = new WindowStore()
    s.open(A)
    s.open(B)
    expect(s.topId).toBe(B)
  })

  it('focus raises a window above the others', () => {
    const s = new WindowStore()
    s.open(A)
    s.open(B)
    s.open(C)
    expect(s.topId).toBe(C)
    s.focus(A)
    expect(s.topId).toBe(A)
  })
})

describe('WindowStore identity + z-raise-no-op (U04-W2, §7-W2)', () => {
  // Gate-Grenze (bewusst, §7-W2 / §6-Client-Render): Dieses Gate belegt die
  // Store-OBJEKT-Identität (den Mechanismus der In-Place-Mutation), NICHT die
  // Repaint-/Snippet-Re-Invocation-Reduktion. vitest läuft DOM-frei
  // (vite.config.ts:60-64) — der Server-Last-Nutzen ist W1-getragen und dort
  // e2e-gegated (Δ /api/manage == 0). W2s Client-Render-Nutzen ist analytisch
  // (Svelte-5-Deep-Proxy: Lesen des Bezeichners `win` erzeugt keine Abhängigkeit
  // auf `win.rect`), hier bewusst nicht render-gegated.
  it('WinState-Identität überlebt focus/move/resize/minimize/restore', () => {
    const s = new WindowStore()
    s.open(A)
    const ref = s.wins[0]
    s.focus(A)
    s.move(A, 10, 10)
    s.resize(A, 5, 5)
    s.minimize(A)
    s.restore(A)
    expect(s.wins[0]).toBe(ref) // dasselbe (Proxy-)Objekt — keine .map()-Ersetzung
  })

  it('focus auf ein bereits oben liegendes Fenster lässt z unverändert (z-Raise-No-op)', () => {
    const s = new WindowStore()
    s.open(A)
    s.open(B) // B ist jetzt top
    const zBefore = s.wins.find((w) => w.id === B)?.z
    s.focus(B) // B ist bereits top → kein z-Schreiben (kein unbegrenztes z-Wachstum)
    const zAfter = s.wins.find((w) => w.id === B)?.z
    expect(zAfter).toBe(zBefore)
  })
})

describe('WindowStore minimize / restore', () => {
  it('minimized windows drop out of topId but stay in openIds (highlight)', () => {
    const s = new WindowStore()
    s.open(A)
    s.open(B)
    s.minimize(B)
    expect(s.topId).toBe(A) // B no longer eligible for top
    expect(s.openIds.has(B)).toBe(true) // but still an open block → still highlighted
  })

  it('restore un-minimizes and brings to front', () => {
    const s = new WindowStore()
    s.open(A)
    s.open(B)
    s.minimize(A)
    s.restore(A)
    expect(s.wins.find((w) => w.id === A)?.minimized).toBe(false)
    expect(s.topId).toBe(A)
  })

  it('topId is null when no window is open (and when all are minimized)', () => {
    const s = new WindowStore()
    expect(s.topId).toBeNull()
    s.open(A)
    s.minimize(A)
    expect(s.topId).toBeNull()
  })
})

describe('WindowStore close — list removal + focus return (a11y)', () => {
  it('returns focus to the window triggerEl', () => {
    const s = new WindowStore()
    const trigger = focusTarget()
    s.open(A, trigger)
    s.close(A)
    expect(s.wins).toHaveLength(0)
    expect(trigger.focused).toBe(true)
  })

  it('falls back to fallbackFocusEl when the window had no triggerEl (node-click)', () => {
    const s = new WindowStore()
    const fallback = focusTarget()
    s.fallbackFocusEl = fallback
    s.open(A, null) // node-click window: no DOM target
    s.close(A)
    expect(fallback.focused).toBe(true) // graph region, NOT <body>
  })

  it('does not throw when neither triggerEl nor fallbackFocusEl exists', () => {
    const s = new WindowStore()
    s.open(A, null)
    expect(() => s.close(A)).not.toThrow()
    expect(s.wins).toHaveLength(0)
  })
})

describe('WindowStore closeAll', () => {
  it('empties the store (Map-Wechsel) without per-window focus return', () => {
    const s = new WindowStore()
    const trigger = focusTarget()
    s.open(A, trigger)
    s.open(B)
    s.closeAll()
    expect(s.wins).toHaveLength(0)
    expect(s.openIds.size).toBe(0)
    expect(trigger.focused).toBe(false) // closeAll does NOT focus-return
  })
})

describe('WindowStore setSurface — open places within the set surface', () => {
  it('spawnRect uses the pushed surface (window fully contained on a big surface)', () => {
    const s = new WindowStore()
    const surface: SurfaceMetrics = { wLu: 2000, hLu: 1200 }
    s.setSurface(surface)
    s.open(A)
    const r = s.wins[0].rect
    expect(r.x).toBeGreaterThanOrEqual(0)
    expect(r.y).toBeGreaterThanOrEqual(0)
    expect(r.x + r.w).toBeLessThanOrEqual(surface.wLu)
    expect(r.y + r.h).toBeLessThanOrEqual(surface.hLu)
  })

  it('a tiny surface forces the MIN window size (sub-min branch)', () => {
    const s = new WindowStore()
    s.setSurface({ wLu: 120, hLu: 90 })
    s.open(A)
    expect(s.wins[0].rect).toMatchObject({ w: MIN_W_LU, h: MIN_H_LU })
  })
})

describe('WindowStore move — clampPos (grabbable, not full containment)', () => {
  it('applies the logical delta and clamps so the titlebar stays grabbable', () => {
    const s = new WindowStore()
    s.setSurface({ wLu: 1280, hLu: 720 })
    s.open(A) // origin ~ (465,15), size 600×570
    const before = { ...s.wins[0].rect }
    s.move(A, -100_000, -100_000) // shove far off the top-left
    const r = s.wins[0].rect
    // x floored at MIN_VISIBLE - w → at least MIN_VISIBLE stays grabbable
    expect(r.x).toBe(MIN_VISIBLE_LU - before.w)
    expect(r.x + before.w).toBe(MIN_VISIBLE_LU)
    expect(r.y).toBe(0) // titlebar never above the top edge
    expect(r.w).toBe(before.w) // move does not resize
  })

  it('a small in-bounds move is applied verbatim (partial off-canvas allowed)', () => {
    const s = new WindowStore()
    s.setSurface({ wLu: 1280, hLu: 720 })
    s.open(A)
    const before = { ...s.wins[0].rect }
    s.move(A, 10, 20)
    expect(s.wins[0].rect).toMatchObject({ x: before.x + 10, y: before.y + 20 })
  })
})

describe('WindowStore resize — clampSize', () => {
  it('applies the logical delta and floors at the minimum size', () => {
    const s = new WindowStore()
    s.open(A)
    const before = { ...s.wins[0].rect }
    s.resize(A, 50, 40)
    expect(s.wins[0].rect).toMatchObject({ w: before.w + 50, h: before.h + 40 })
  })

  it('cannot shrink a window below MIN_W_LU / MIN_H_LU', () => {
    const s = new WindowStore()
    s.open(A)
    s.resize(A, -100_000, -100_000)
    expect(s.wins[0].rect).toMatchObject({ w: MIN_W_LU, h: MIN_H_LU })
  })
})
