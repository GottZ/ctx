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
  // Forge webhook receiver (design 04 §4.1.1, W13-Vorgriff; Achse 03 §5.3/§9.3):
  // the ingress lives OUTSIDE /api, so the SPA must never claim it as a client
  // route. Pinned now (prefix only) — the handler lands with the W13 sync wave.
  '/webhooks',
  // External-login consumer flow (OAuth L5, design 04-oauth §4.2/§4.3):
  // /auth/login/{provider} + /auth/callback/{provider} are server routes —
  // the SPA must never claim the prefix or a deep link would shadow the flow.
  '/auth',
  // OpenAI-compatible gateway (design 02 §4.2, design 05 K-05-12, W02-02):
  // /v1/* and /v2/rerank answer in the OpenAI error envelope once mounted,
  // so no SPA route may claim either prefix.
  '/v1',
  '/v2',
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
  // Kategorie-Farb-Overrides (AM-2, U02-W6): eigene Lazy-Sub-Route = eigener
  // Chunk (das OKLCH-Wheel trägt weder der /settings- noch der Default-Chunk,
  // design 02a §A4-W6 PLc). Kein Tier-Guard auf /settings/*; die Fläche self-
  // gated auf admin/tenant-admin (HueWheelPage). areaMode unmapped → 'reading'.
  '/settings/hues': () => import('./settings/hues/HueWheelPage.svelte'),
  '/status': () => import('./status/StatusPage.svelte'),
  '/graph': () => import('./graph/GraphPage.svelte'),
  '/chat': () => import('./chat/ChatPage.svelte'),
  '/blocks': () => import('./blocks/BlocksPage.svelte'),
  // Member landing (design 06-role-nav.md §3, D2, Welle N6): the dedicated
  // capability screen. Registered in the SAME commit that flips
  // landingFor(member) → /home (guard.ts), so `/` never canonicalizes to an
  // unregistered route (§94, no NotFound window). areaMode('/home') is unmapped
  // → 'reading' (modes.ts default), so it self-limits on the shell reading cap.
  '/home': () => import('./MemberHome.svelte'),
  // Role-gated areas (design 06-role-nav.md §3, Welle N4/N5): the route table is
  // additive, the TIER guard lives in guardArea (router.ts beforeLoad). Pages
  // are owned by the server-admin (A2) / tenant-admin (TK3) axes; registered
  // here so the guard has a real route to land on. /tenant/backends is NOT
  // registered yet — its page does not exist (the /tenant prefix-guard still
  // covers it once it lands).
  '/admin': () => import('./admin/AdminPage.svelte'),
  // Tenant-detail + per-scope quota (design 04 §3, Welle A4). Registered in the
  // SAME wave that adds TenantDetail.svelte (NOT in A2) so Vite can resolve the
  // lazy chunk. areaMode('/admin/tenants/:id') is unmapped → 'reading'; the N5
  // /admin prefix-guard already covers this sub-route (no extra guard needed).
  '/admin/tenants/:id': () => import('./admin/TenantDetail.svelte'),
  // Type-registry admin (design 04 §4.7/§5.5, wave U10): the first web surface
  // that makes classification policy writable (retrieval/guard/dream/parent).
  // Server-admin-only via the /admin prefix-guard (guard.ts TIER_GATED, U04 pin)
  // — no new guard entry. Registered in the SAME wave as TypesAdminPage.svelte so
  // Vite resolves the lazy chunk; areaMode('/admin/types') is unmapped → 'reading'.
  '/admin/types': () => import('./admin/types/TypesAdminPage.svelte'),
  '/tenant': () => import('./tenant/TenantPage.svelte'),
  // Tenant-scoped backend pool (design 04 §4 E04-4 / §4.9, wave U11): the E04-4
  // rot-fix. The /tenant/backends nav item shipped in U04 but the route did not,
  // so the deep link hit NotFound. Registered here, it reuses the settings
  // BackendsPage in its tenant-scoped variant (pool only). Inherits the /tenant
  // prefix-guard (guard.ts TIER_GATED → manageTenantKeys, tenant-admin-or-up);
  // areaMode('/tenant/backends') is unmapped → 'reading'.
  '/tenant/backends': () => import('./tenant/TenantBackendsPage.svelte'),
  // Workflow surface (design 04 §4.1, wave U04): additive lazy routes for the
  // issue list, issue detail and board. DARK-LAUNCHED — the nav section + home
  // tile are gated on caps.viewWorkflow (false until whoami carries the flag,
  // §4.1.3), but the routes are registered unconditionally so a deep link
  // renders the page's EmptyState, never NotFound and never a redirect loop
  // (the pages are ungated member surfaces; authorization stays server-side).
  // /issues is 'split' (list+detail column, like /blocks), /issues/:id inherits
  // it via prefix-match, /board is the new 'board' mode (modes.ts). U04 ships
  // empty scaffolds only — the data layer lands in U05/U06/U07.
  '/issues': () => import('./issues/IssuesPage.svelte'),
  '/issues/:id': () => import('./issues/IssueDetailPage.svelte'),
  '/board': () => import('./board/BoardPage.svelte'),
  // Guard review queue (needs_review pipeline W4): registered unconditionally
  // (the U04 dark-launch line — a deep link renders the page, authorization
  // stays server-side); the nav item gates on caps.viewOpsSurfaces.
  // areaMode('/guard') is unmapped → 'reading'.
  '/guard': () => import('./guard/GuardReviewPage.svelte'),
  // Model API area (design/05 §4.6, wave W05-05): the gateway consumer list.
  // Registered flag-independently (the dark-launch line above: a deep link
  // reaches the guard, never NotFound); guardArea gates it on caps.viewModelApi
  // (server-admin always, tenant-admin only with model_api.enabled, member
  // never). Its sub-routes (/gateway/consumers/:id W05-09, /gateway/models
  // W05-13) inherit the prefix-guard; the peer page stays /admin/peers so the
  // guard prefixes remain disjoint. Outside the reserved /v1 namespace.
  // areaMode('/gateway') is unmapped → 'reading'.
  '/gateway': () => import('./gateway/GatewayPage.svelte'),
  // Alias catalog (design/05 §4.6 ModelsPage, wave W05-13 — RC-D C5): inherits
  // the /gateway prefix-guard (caps.viewModelApi); its own lazy chunk.
  // areaMode('/gateway/models') is unmapped → 'reading'.
  '/gateway/models': () => import('./gateway/ModelsPage.svelte'),
  // Consumer detail (RC-D C6, wave W05-09; design/05 §4.6): inherits the
  // /gateway prefix guard (viewModelApi). Head, policy, key chain, usage
  // panel (W05-10), limits (W05-12), last calls, audit trail.
  '/gateway/consumers/:id': () => import('./gateway/ConsumerDetail.svelte'),
  '*': () => import('./NotFound.svelte'),
} satisfies Routes

/**
 * Landing redirect (design 06-role-nav.md §4, Welle N4/N6): `/` is no area — it
 * canonicalizes to the caps-adaptive landing (member → /home, higher tiers /
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
