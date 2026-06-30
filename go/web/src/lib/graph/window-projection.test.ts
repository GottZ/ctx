// Window-projection gates (design 07 Wave G5a). Proves the geometry layer is
// GENUINELY renderer-agnostic (lu, not px) and not "accidentally correct because
// 1:1": (1) DomProjector is identity@scale1; (2) the D4 FALSIFICATION — an
// anisotropic ScaleProjector(sx≠sy) round-trips through toLogicalDelta AND the
// pure geometry (spawnRect/clampSize/clampPos) yields IDENTICAL lu regardless of
// which projector renders (they take no projector); (3) clampSize min; (4)
// spawnRect cascade + full clamp incl. the sub-min branch; (5) clampPos move-clamp
// (grabbable-invariant, partial off-canvas allowed — NOT full containment).

import { describe, expect, it } from 'vitest'
import {
  clampPos,
  clampSize,
  DomProjector,
  MIN_H_LU,
  MIN_VISIBLE_LU,
  MIN_W_LU,
  SPAWN_ORIGIN_X,
  SPAWN_ORIGIN_Y,
  SPAWN_STEP,
  spawnRect,
  TITLEBAR_H_LU,
  type LogicalRect,
  type RenderRect,
  type SurfaceMetrics,
  type WindowProjector,
} from './window-projection'

const SURFACE: SurfaceMetrics = { wLu: 1280, hLu: 720 }

// NICHT-Identity-Mock-Renderer: skaliert x und y UNTERSCHIEDLICH (anisotrop).
// sx≠sy fängt einen Achsen-Swap — ein toLogicalDelta, das versehentlich durch
// die falsche Achse teilt (dx/sy statt dx/sx), würde den Round-Trip brechen.
function ScaleProjector(sx: number, sy: number): WindowProjector<RenderRect> {
  return {
    toRender: (r) => ({ left: r.x * sx, top: r.y * sy, width: r.w * sx, height: r.h * sy }),
    toLogicalDelta: (dx, dy) => ({ dx: dx / sx, dy: dy / sy }),
  }
}

describe('DomProjector (the only built renderer, identity @ scale 1)', () => {
  it('toRender maps lu 1:1 onto px (1 lu == 1 CSS-px)', () => {
    const r: LogicalRect = { x: 12, y: 34, w: 300, h: 200 }
    expect(DomProjector.toRender(r, SURFACE)).toEqual({ left: 12, top: 34, width: 300, height: 200 })
  })

  it('toLogicalDelta is identity (px delta == lu delta @ scale 1)', () => {
    expect(DomProjector.toLogicalDelta(17, -23, SURFACE)).toEqual({ dx: 17, dy: -23 })
  })
})

describe('D4 falsification — genuinely agnostic, not accidentally 1:1', () => {
  it('an anisotropic ScaleProjector(sx≠sy) round-trips: toLogicalDelta(sx·dx, sy·dy) === {dx,dy}', () => {
    const sx = 3
    const sy = 7 // sx≠sy on purpose — catches an axis-swap in the inverse
    const p = ScaleProjector(sx, sy)
    const d = { dx: 10, dy: 4 }
    // a drag of d.dx,d.dy lu renders as sx·dx, sy·dy in concrete space; the
    // inverse MUST recover the original lu delta (else drag runs away @ scale≠1)
    expect(p.toLogicalDelta(sx * d.dx, sy * d.dy, SURFACE)).toEqual(d)
    // an axis-swapped inverse (dx/sy, dy/sx) would yield {3.33…, 12} ≠ d — proven
    // distinct precisely because sx≠sy:
    expect(p.toLogicalDelta(sx * d.dx, sy * d.dy, SURFACE)).not.toEqual({ dx: (sx * d.dx) / sy, dy: (sy * d.dy) / sx })
  })

  it('spawnRect/clampSize/clampPos give IDENTICAL lu regardless of which projector renders', () => {
    // The geometry functions take NO projector → the lu they produce is one
    // value; only the projection of that value into render-space differs. That
    // is what makes the layer agnostic rather than coincidentally 1:1.
    const rect = spawnRect(0, SURFACE)
    const dom = DomProjector.toRender(rect, SURFACE)
    const aniso = ScaleProjector(2, 3).toRender(rect, SURFACE)
    // SAME lu rect in, DIFFERENT px out under an anisotropic projector:
    expect(dom).toEqual({ left: rect.x, top: rect.y, width: rect.w, height: rect.h })
    expect(aniso).toEqual({ left: rect.x * 2, top: rect.y * 3, width: rect.w * 2, height: rect.h * 3 })
    expect(aniso).not.toEqual(dom) // the projector demonstrably matters for px…
    // …yet the upstream lu math is projector-free: identical lu inputs (here the
    // very rect both projected) prove no hidden 1lu==1px fudge sits inside it.
    expect(clampSize(10, 10)).toEqual({ w: MIN_W_LU, h: MIN_H_LU })
    expect(clampPos({ x: 9_999, y: 9_999, w: 300, h: 200 }, SURFACE)).toEqual({
      x: SURFACE.wLu - MIN_VISIBLE_LU,
      y: SURFACE.hLu - TITLEBAR_H_LU,
      w: 300,
      h: 200,
    })
  })
})

describe('clampSize — minimum window size (MIN_W_LU=255 / MIN_H_LU=135)', () => {
  it('floors sub-minimum dims to the minimum', () => {
    expect(MIN_W_LU).toBe(255)
    expect(MIN_H_LU).toBe(135)
    expect(clampSize(10, 10)).toEqual({ w: 255, h: 135 })
  })

  it('passes through dims already at or above the minimum', () => {
    expect(clampSize(500, 400)).toEqual({ w: 500, h: 400 })
  })
})

describe('spawnRect — cascade off=SPAWN_STEP*(i%6) from origin, full clamp', () => {
  it('places window 0 at the origin with the default size', () => {
    const r = spawnRect(0, SURFACE)
    // size: min(28rem,0.9·wLu) × min(22rem,0.8·hLu) = 420 × 330 on this surface
    expect(r).toEqual({ x: SPAWN_ORIGIN_X, y: SPAWN_ORIGIN_Y, w: 420, h: 330 })
  })

  it('cascades by SPAWN_STEP per window and wraps modulo 6', () => {
    expect(spawnRect(1, SURFACE).x).toBe(SPAWN_ORIGIN_X + SPAWN_STEP)
    expect(spawnRect(1, SURFACE).y).toBe(SPAWN_ORIGIN_Y + SPAWN_STEP)
    // i=6 → 6%6=0 → back to the origin (no offset)
    expect(spawnRect(6, SURFACE)).toEqual(spawnRect(0, SURFACE))
  })

  it('fully clamps the position into [0, wLu-w] × [0, hLu-h]', () => {
    // a narrow-but-valid surface pushes the origin back to the right/bottom edge
    const surface: SurfaceMetrics = { wLu: 700, hLu: 500 }
    const r = spawnRect(0, surface)
    expect(r.x).toBe(surface.wLu - r.w) // origin 465 > wLu-w → clamped to the edge
    expect(r.x).toBeGreaterThanOrEqual(0)
    expect(r.y).toBeGreaterThanOrEqual(0)
    expect(r.x + r.w).toBeLessThanOrEqual(surface.wLu)
    expect(r.y + r.h).toBeLessThanOrEqual(surface.hLu)
  })

  it('sub-min surface: clampSize forces MIN and the window overflows BY DESIGN', () => {
    const surface: SurfaceMetrics = { wLu: 100, hLu: 80 }
    const r = spawnRect(0, surface)
    // a sub-min (unreadable) window is worse than overflow → MIN wins, x/y clamp to 0
    expect(r).toEqual({ x: 0, y: 0, w: MIN_W_LU, h: MIN_H_LU })
    expect(r.w).toBeGreaterThan(surface.wLu) // overflow accepted
  })
})

describe('clampPos — move-clamp (grabbable-invariant, partial off-canvas allowed)', () => {
  it('keeps MIN_VISIBLE on-surface at the LEFT edge (window shoved far left)', () => {
    const r = clampPos({ x: -9_999, y: -9_999, w: 300, h: 200 }, SURFACE)
    // x floored at MIN_VISIBLE - w → right edge sits at MIN_VISIBLE → still grabbable
    expect(r.x).toBe(MIN_VISIBLE_LU - 300)
    expect(r.x + 300).toBe(MIN_VISIBLE_LU)
    // y floored at 0 → titlebar never above the top edge
    expect(r.y).toBe(0)
  })

  it('keeps MIN_VISIBLE on-surface at the RIGHT edge + titlebar above the bottom', () => {
    const r = clampPos({ x: 99_999, y: 99_999, w: 300, h: 200 }, SURFACE)
    expect(r.x).toBe(SURFACE.wLu - MIN_VISIBLE_LU) // left edge at wLu-MIN_VISIBLE
    expect(r.y).toBe(SURFACE.hLu - TITLEBAR_H_LU) // titlebar height stays on-surface
  })

  it('leaves a partially-off-canvas window UNCHANGED (move ≠ full containment)', () => {
    // contrast with spawnRect (full clamp): a window partly off-screen but within
    // the grabbable bounds is NOT pulled back — that is the n8n/Maps feel.
    const partial: LogicalRect = { x: -100, y: 10, w: 300, h: 200 }
    expect(clampPos(partial, SURFACE)).toEqual(partial)
  })
})
