// Route table as a pure module: sv-router's createRouter touches DOM
// observers at call time, so the table and the entry-redirect rule live here
// where vitest can assert them in a plain node environment (router.ts does
// the instantiation).

import type { Routes } from 'sv-router'
import type { Capabilities } from '../lib/auth/capabilities'
import { landingFor, type LandingPath } from '../lib/nav/guard'

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
  // Role-gated areas (design 06-role-nav.md §3, Welle N4/N5): the route table is
  // additive, the TIER guard lives in guardArea (router.ts beforeLoad). Pages
  // are owned by the server-admin (A2) / tenant-admin (TK3) axes; registered
  // here so the guard has a real route to land on. /tenant/backends is NOT
  // registered yet — its page does not exist (the /tenant prefix-guard still
  // covers it once it lands).
  '/admin': () => import('./admin/AdminPage.svelte'),
  '/tenant': () => import('./tenant/TenantPage.svelte'),
  '*': () => import('./NotFound.svelte'),
} satisfies Routes

/**
 * Landing redirect (design 06-role-nav.md §4, Welle N4): `/` is no area — it
 * canonicalizes to the caps-adaptive landing (member → /blocks, higher tiers /
 * loading → /status, via landingFor). Returns the target for `/`, else null.
 * router.ts passes `session.caps`; kept caps-as-param so this stays a pure,
 * node-testable module (the session read lives only in router.ts).
 */
export function entryRedirect(pathname: string, caps: Capabilities): LandingPath | null {
  return pathname === '/' ? landingFor(caps) : null
}

// areaMode (design 01-shell-layout §3, §4 S2) is the route->layout-mode map. It
// lives in lib/layout/modes but is re-exported here so the route-namespace test
// (index.test.ts) can pin "every areaRoutes key resolves to a LayoutMode" next
// to the table it guards — one shared pure test, no cross-dir import there.
export { areaMode } from '../lib/layout/modes'
export type { LayoutMode } from '../lib/layout/modes'
