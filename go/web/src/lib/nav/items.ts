// Nav-item contract (design 06-role-nav.md §3/§5, Welle N3). `navItems(caps)`
// is the ordered, grouped list the shell-layout Nav-Rail (S3) renders — this
// module owns the *data*, shell-layout owns the rendering. The NavItem shape
// defined here is the agreed prop contract (§5: {href,label,iconKey,section,
// exact?}); shell-layout imports it rather than re-declaring, so the two stay
// in lockstep (Cross-Dep N3).
//
// Sections are tier-additive — containment server-admin ⊇ tenant-admin ⊇
// member, mirroring capabilities.ts: corpus shows to every resolved tier,
// tenant adds own-tenant management, server adds cross-tenant admin. A
// `loading` tier (boot-time restore race, R6) yields [] — no flickering rail
// before the session resolves. Visibility is UX only; the real authorization
// stays server-side (R2).
//
// Route reality (§5): the corpus items point at routes that exist TODAY
// (/blocks /graph /chat, routes/index.ts:33-35; /blocks is today's
// corpus-browse entry, search/ask is a later theme). The tenant (/tenant,
// /tenant/backends) and server (/admin) management routes are PLACEHOLDERS
// owned by the tenant-admin (TK3) / server-admin (A2) axes — the item is just
// data here, the live route wiring + pages land with those axes.
//
// Ops surfaces (Settings/Status/Backends) sit in the SERVER section gated on
// viewTenants: per the design's conservative D5/K2 stance they stay
// server-admin-visible until the tenant-admin axis owns their relocation.
// `caps.viewOpsSurfaces` is the hook that axis will consume — this module does
// not place them in the tenant section yet.

import type { Capabilities } from '../auth/capabilities'

export interface NavItem {
  /** Route the item links to. Corpus items exist today; tenant/server are placeholders (§5). */
  href: string
  /** Human label for the rail. */
  label: string
  /** Stable key the shell-layout rail maps to an icon glyph (no component coupling here). */
  iconKey: string
  /** The grouped section the rail renders the item under. */
  section: 'corpus' | 'tenant' | 'server'
  /** Match active state on exact pathname rather than startsWith — set on a parent that has a child nav item so they don't both highlight. */
  exact?: boolean
}

/**
 * The ordered, grouped nav-item list for a capability set. Sections appear in
 * corpus → tenant → server order; items inside a section keep this table's
 * order. Pure function of `caps` — single source of truth for rail visibility.
 * `loading` → [].
 */
export function navItems(caps: Capabilities): NavItem[] {
  // R6: no rail before the session resolves. Redundant with the all-false
  // loading flags below, but explicit so a future caller outside App.svelte's
  // session.active gate can't flash a partial rail.
  if (caps.tier === 'loading') return []

  const items: NavItem[] = []

  // CORPUS — every resolved tier.
  if (caps.viewCorpus) {
    items.push(
      { href: '/blocks', label: 'Blocks', iconKey: 'blocks', section: 'corpus' },
      { href: '/graph', label: 'Graph', iconKey: 'graph', section: 'corpus' },
      { href: '/chat', label: 'Chat', iconKey: 'chat', section: 'corpus' },
    )
  }

  // TENANT — own-tenant management (tier ≥ tenant-admin). Placeholder routes
  // owned by the tenant-admin axis (TK3).
  if (caps.manageTenantKeys) {
    // Keys + members share one surface (§5: /tenant/keys + /tenant/members live
    // under /tenant). exact so /tenant/backends doesn't also highlight it.
    items.push({ href: '/tenant', label: 'Keys & Members', iconKey: 'keys', section: 'tenant', exact: true })
  }
  if (caps.viewTenantBackends) {
    items.push({ href: '/tenant/backends', label: 'Backends', iconKey: 'backends', section: 'tenant' })
  }

  // SERVER — cross-tenant admin (server-admin only). Tenants route owned by the
  // server-admin axis (A2); the ops surfaces stay here conservatively (D5/K2).
  if (caps.viewTenants) {
    items.push(
      { href: '/admin', label: 'Tenants', iconKey: 'tenants', section: 'server' },
      // exact so /settings/backends doesn't also highlight the Settings item.
      { href: '/settings', label: 'Settings', iconKey: 'settings', section: 'server', exact: true },
      { href: '/status', label: 'Status', iconKey: 'status', section: 'server' },
      { href: '/settings/backends', label: 'Backends', iconKey: 'backends', section: 'server' },
    )
  }

  return items
}
