// Tier route-guard + landing resolver (design 06-role-nav.md §3/§4, Wellen
// N4+N5). PURE functions of `caps` — no session import, so vitest asserts them
// in a plain node environment (mirrors lib/nav/items.ts N3). router.ts owns the
// `session.caps` read and wires both into the central `beforeLoad` hook.
//
// landingFor (N4): where `/` canonicalizes to, per tier. Member → /blocks (the
// corpus-browse entry that exists today; the dedicated /home capability screen
// arrives with N6). tenant-admin & server-admin → /status (ops focus, D3).
// `loading` → /status (safe default before the session resolves — superseded
// once whoami lands and the tier resolves).
//
// guardArea (N5): a tier-forbidden area returns its redirect target (= the
// caps-adaptive landing), else null. /admin (+/admin/*) needs server-admin
// (viewTenants); /tenant (+/tenant/*) needs tenant-admin or higher
// (manageTenantKeys). `loading` → ALWAYS null: no redirect during the
// session-restore race (R6), else a deep link would kick the user out before
// whoami resolves. Visibility is UX only — the real authorization stays
// server-side (RequireAdmin / RequireAdminOrTenantAdmin, R2); this redirect is
// comfort, not a gate.

import type { Capabilities } from '../auth/capabilities'

/**
 * The landing targets — both are registered areaRoutes keys, so the literal
 * union (not bare `string`) is what sv-router's `navigate` accepts at the
 * router.ts call sites.
 */
export type LandingPath = '/blocks' | '/status'

/**
 * The caps-adaptive landing path for `/`. Member → /blocks; every higher tier
 * and the loading floor → /status (safe default). Pure function of the tier.
 */
export function landingFor(caps: Capabilities): LandingPath {
  return caps.tier === 'member' ? '/blocks' : '/status'
}

/**
 * Tier-gated area prefixes: each prefix paired with the capability it requires.
 * Matched prefix-style (`p === prefix || p.startsWith(`${prefix}/`)`, same as
 * areaMode / the reserved namespace) so deep sub-routes (/admin/tenants,
 * /tenant/backends) inherit their area's gate. Prefixes are disjoint, so match
 * order is irrelevant.
 */
const TIER_GATED: ReadonlyArray<readonly [string, (caps: Capabilities) => boolean]> = [
  ['/admin', (caps) => caps.viewTenants], // server-admin only
  ['/tenant', (caps) => caps.manageTenantKeys], // tenant-admin or higher
]

/**
 * Resolve a tier-redirect for a pathname: the landing target when the caps may
 * NOT enter the area, else null (allowed, or an ungated area). `loading` →
 * always null (no redirect before the session resolves, R6). Pure.
 */
export function guardArea(pathname: string, caps: Capabilities): LandingPath | null {
  if (caps.tier === 'loading') return null
  for (const [prefix, allowed] of TIER_GATED) {
    if (pathname === prefix || pathname.startsWith(`${prefix}/`)) {
      return allowed(caps) ? null : landingFor(caps)
    }
  }
  return null
}
