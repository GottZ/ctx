// Pins areaMode (design 01-shell-layout §4 S2): a pure route->layout-mode map
// resolved synchronously from the pathname (no flash on lazy navigation). Every
// area's mode, the 'reading' default for unmapped/unknown paths, deep
// sub-route inheritance, and the cross-module invariant "areaMode covers every
// areaRoutes key" — so a newly registered route always resolves to a defined
// mode. Mirrors lib/blocks/filters.test.ts.

import { describe, expect, it } from 'vitest'
import { areaRoutes } from '../../routes/index'
import { areaMode } from './modes'
import type { LayoutMode } from './modes'

const LAYOUT_MODES: readonly LayoutMode[] = ['reading', 'canvas', 'split', 'thread']

describe('areaMode', () => {
  const cases: ReadonlyArray<readonly [string, LayoutMode]> = [
    ['/graph', 'canvas'],
    ['/blocks', 'split'],
    ['/chat', 'thread'],
    ['/settings', 'reading'],
    ['/status', 'reading'],
    ['/settings/backends', 'reading'],
  ]

  for (const [pathname, mode] of cases) {
    it(`maps ${pathname} -> ${mode}`, () => {
      expect(areaMode(pathname)).toBe(mode)
    })
  }

  it('falls back to reading for unknown paths and the catch-all', () => {
    for (const p of ['/', '/nope', '/admin/tenants', '', '*']) {
      expect(areaMode(p)).toBe('reading')
    }
  })

  it('inherits the area mode for deep sub-routes', () => {
    expect(areaMode('/graph/019d')).toBe('canvas')
    expect(areaMode('/blocks/abc')).toBe('split')
    expect(areaMode('/chat/123')).toBe('thread')
  })

  it('resolves every areaRoutes key to a valid LayoutMode', () => {
    for (const key of Object.keys(areaRoutes)) {
      expect(LAYOUT_MODES).toContain(areaMode(key))
    }
  })
})
