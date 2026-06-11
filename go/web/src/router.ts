// sv-router 0.16.3 (exactly pinned, design 04-§2.3 / R11), history mode —
// deep links work because ctxd serves the SPA fallback for HTML navigations
// (go/web/web.go). The login gate lives in App.svelte, before the Router
// renders; routes need no per-route auth guard.

import { createRouter } from 'sv-router'
import { areaRoutes, entryRedirect } from './routes'

export const { p, navigate, isActive, route } = createRouter({
  ...areaRoutes,
  hooks: {
    beforeLoad({ pathname }) {
      const target = entryRedirect(pathname)
      // Documented sv-router redirect idiom: navigate() queues the new
      // navigation, the throw aborts the current one.
      if (target !== null) throw navigate(target, { replace: true })
    },
  },
})
