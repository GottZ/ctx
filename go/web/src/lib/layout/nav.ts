// Nav-Rail visibility wrapper (design 01-shell-layout §3, Welle S3). The
// *data* — the ordered, grouped, role-gated item list — lives in lib/nav/items
// (Welle N3, owned by the role-nav axis). shell-layout owns only the *rendering*
// (NavRail/NavDrawer). visibleNav is the thin seam between the two: it reads the
// session's derived capabilities (the single source of truth, lib/auth.svelte →
// capabilitiesFor) and hands navItems the caps it filters on.
//
// Typed structurally on `{ caps }` rather than the concrete Session class so the
// shell passes the live `session` rune while vitest can pass a plain caps stub —
// visibleNav consumes nothing else off the session. A `loading` tier (boot-time
// restore race) yields [] via navItems, so the rail never flickers a partial set
// before the session resolves (R6).

import type { Capabilities } from '../auth/capabilities'
import { navItems } from '../nav/items'
import type { NavItem } from '../nav/items'

export type { NavItem }

/** The role-adaptive nav-item list for a session (reads only its caps). */
export function visibleNav(session: { caps: Capabilities }): NavItem[] {
  return navItems(session.caps)
}
