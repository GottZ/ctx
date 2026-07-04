// @live — tenant isolation against the REAL server enforcement (design 06 §4.7
// probe 3 / §5.6b, wave PV10). The flagship proof the mock tier can NEVER make
// (the mock returns whatever the test asks for). Positive control FIRST (the
// detector provably sees data), THEN absence — so an error state / empty store
// / marker drift can't make the probe vacuously green.
//
// Negative gate (design 06 PV10 gate c): re-run the seed with
// CTX_E2E_LEAK_INJECT=1 (writes tenant A's sentinel into tenant B) — this spec
// then goes RED, proving the detector detects.

import { test, expect } from '@playwright/test'
import { readState } from './state'
import { loginAs } from './helpers'

const state = readState()

test('@live tenant-isolation: B sees its own sentinel, never A’s', async ({ page }) => {
  await loginAs(page, state.tenants.b.ownerKey)
  await page.goto('/blocks')

  const search = page.locator('input[type="search"]')

  // (1) POSITIVE CONTROL: B's own sentinel is reachable — the data region
  //     provably renders real tenant-B data.
  await search.fill(state.tenants.b.sentinel)
  await search.press('Enter')
  // The sentinel renders in both the row title and its preview — scope to the
  // result row (first match) so the positive control is unambiguous.
  await expect(page.locator('ul.results li', { hasText: state.tenants.b.sentinel }).first()).toBeVisible()

  // (2) ABSENCE: searching A's sentinel as tenant B returns it NOWHERE. The
  //     server-side scope isolation is what makes this true — kill it and the
  //     A sentinel would surface here.
  await search.fill(state.tenants.a.sentinel)
  await search.press('Enter')
  // Deterministic completion signal: the clean end state of this search IS the
  // "No matches" empty state. A transient assert ("B's sentinel left the list")
  // is satisfied mid-refresh — after the list clears but BEFORE the new hits
  // render — so a leaked row could slip past a bare not.toContainText (verified
  // live: the API returned the injected row while the probe stayed green).
  // With a leak the hit row renders instead and this assert goes red.
  await expect(page.getByText('No matches')).toBeVisible()
  // The search box VALUE holds the typed A-sentinel, but that is not a text
  // node; the rendered content must not contain it anywhere.
  await expect(page.locator('main.content')).not.toContainText(state.tenants.a.sentinel)
})
