// Route table as a pure module: sv-router's createRouter touches DOM
// observers at call time, so the table and the entry-redirect rule live here
// where vitest can assert them in a plain node environment (router.ts does
// the instantiation).

import type { Routes } from 'sv-router'

/**
 * Path prefixes chi registers on the server — the SPA must never claim them
 * or deep links would shadow real endpoints (design 04-§2.3, forbidden SPA
 * routes). Pinned by the route-namespace test.
 */
export const RESERVED_SERVER_PREFIXES = [
  '/api',
  '/mcp',
  '/health',
  '/authorize',
  '/token',
  '/.well-known',
] as const

/**
 * The four areas (design 04-§2.1) — lazy per area so Settings/Status/Graph/
 * Chat become separate chunks; F5 (sigma+graphology) and F6 only load when
 * entered. Settings is the W4 target UI; Status/Graph/Chat ship in W6/F5/F6,
 * their placeholders keep structure and navigation real.
 */
export const areaRoutes = {
  '/settings': () => import('./settings/SettingsPage.svelte'),
  '/status': () => import('./status/Placeholder.svelte'),
  '/graph': () => import('./graph/GraphPage.svelte'),
  '/chat': () => import('./chat/Placeholder.svelte'),
  '*': () => import('./NotFound.svelte'),
} satisfies Routes

/** Landing redirect: `/` is no area — it canonicalizes to /status. */
export function entryRedirect(pathname: string): '/status' | null {
  return pathname === '/' ? '/status' : null
}
