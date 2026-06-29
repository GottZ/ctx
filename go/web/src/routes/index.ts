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
 * The five areas (design 04-§2.1) — lazy per area so each area is its own
 * chunk; F5 (sigma+graphology) and F6 (markdown-it+DOMPurify) only load when
 * entered. /blocks (block-workbench) loads its read client lazily as well.
 */
export const areaRoutes = {
  '/settings': () => import('./settings/SettingsPage.svelte'),
  // Deep sub-route under /settings (its own lazy chunk); the topbar /settings
  // tab stays active via startsWith. Reaches no reserved server prefix.
  '/settings/backends': () => import('./settings/backends/BackendsPage.svelte'),
  '/status': () => import('./status/StatusPage.svelte'),
  '/graph': () => import('./graph/GraphPage.svelte'),
  '/chat': () => import('./chat/ChatPage.svelte'),
  '/blocks': () => import('./blocks/BlocksPage.svelte'),
  '*': () => import('./NotFound.svelte'),
} satisfies Routes

/** Landing redirect: `/` is no area — it canonicalizes to /status. */
export function entryRedirect(pathname: string): '/status' | null {
  return pathname === '/' ? '/status' : null
}

// areaMode (design 01-shell-layout §3, §4 S2) is the route->layout-mode map. It
// lives in lib/layout/modes but is re-exported here so the route-namespace test
// (index.test.ts) can pin "every areaRoutes key resolves to a LayoutMode" next
// to the table it guards — one shared pure test, no cross-dir import there.
export { areaMode } from '../lib/layout/modes'
export type { LayoutMode } from '../lib/layout/modes'
