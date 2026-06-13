// Route-table gates (design 04-§2.3): the four areas exist, stay lazy, and
// never collide with paths chi registers on the server — a mistyped SPA
// route must not shadow /api, /mcp, OAuth or health endpoints.

import { describe, expect, it } from 'vitest'
import { RESERVED_SERVER_PREFIXES, areaRoutes, entryRedirect } from './index'

describe('areaRoutes', () => {
  it('contains the four areas, the backends sub-route and the catch-all', () => {
    expect(Object.keys(areaRoutes).sort()).toEqual([
      '*',
      '/chat',
      '/graph',
      '/settings',
      '/settings/backends',
      '/status',
    ])
  })

  it('keeps every area lazy (separate chunk per area)', () => {
    for (const loader of Object.values(areaRoutes)) {
      expect(typeof loader).toBe('function')
    }
  })

  it('claims no path inside the reserved server namespace', () => {
    const paths = Object.keys(areaRoutes).filter((p) => p.startsWith('/'))
    for (const path of paths) {
      for (const reserved of RESERVED_SERVER_PREFIXES) {
        expect(path === reserved || path.startsWith(`${reserved}/`)).toBe(false)
      }
    }
  })
})

describe('entryRedirect', () => {
  it('canonicalizes / to /status', () => {
    expect(entryRedirect('/')).toBe('/status')
  })

  it('leaves every real route alone', () => {
    for (const path of ['/settings', '/status', '/graph', '/chat', '/nope', '']) {
      expect(entryRedirect(path)).toBeNull()
    }
  })
})
