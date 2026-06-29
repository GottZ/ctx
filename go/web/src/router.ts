// sv-router 0.16.3 (exactly pinned, design 04-§2.3 / R11), history mode —
// deep links work because ctxd serves the SPA fallback for HTML navigations
// (go/web/web.go). The login gate lives in App.svelte, before the Router
// renders; routes need no per-route auth guard.

import { createRouter } from 'sv-router'
import { areaRoutes, entryRedirect } from './routes'
import { guardArea } from './lib/nav/guard'
import { session } from './lib/auth.svelte'

export const { p, navigate, isActive, route } = createRouter({
  ...areaRoutes,
  hooks: {
    beforeLoad({ pathname }) {
      // N4 then N5, in order. entryRedirect first so `/` resolves to the
      // caps-adaptive landing (member→/blocks, higher tiers/loading→/status)
      // and never falls through to the area-guard. guardArea then redirects a
      // tier-forbidden area (/admin needs server-admin, /tenant needs
      // tenant-admin+) to that same landing; loading → null (no redirect before
      // whoami resolves). Both read the live session caps here — the policy
      // functions stay pure (lib/nav/guard.ts). Documented sv-router redirect
      // idiom: navigate() queues the new navigation, the throw aborts this one.
      const landing = entryRedirect(pathname, session.caps)
      if (landing !== null) throw navigate(landing, { replace: true })
      const guarded = guardArea(pathname, session.caps)
      if (guarded !== null) throw navigate(guarded, { replace: true })
    },
  },
})
