// @live — store→search→detail roundtrip against the REAL handlers (design 06
// §4.7 probe 2, wave PV10). The block was written by the seed through the live
// /api/store handler; here the SPA searches and opens it, so every shape on
// the way (search result, block detail) comes from the server, not fixtures.

import { test, expect } from '@playwright/test'
import { readState } from './state'
import { loginAs } from './helpers'

const state = readState()

test('@live store-roundtrip: seeded block is searchable and opens in detail', async ({ page }) => {
  await loginAs(page, state.tenants.b.ownerKey)
  await page.goto('/blocks')

  // Full-text search for the seeded roundtrip block via the real search path.
  await page.locator('input[type="search"]').fill(state.roundtripTitle)
  await page.locator('input[type="search"]').press('Enter')

  const hit = page.locator('ul.results li', { hasText: state.roundtripTitle })
  await expect(hit).toBeVisible()

  // Open it — the detail view renders server-delivered content. Scoped to the
  // detail panel: in the split view the same sentence also renders in the list
  // row's preview span, so a page-wide getByText resolves to TWO elements and
  // trips Playwright's strict mode (first real CI run, dispatch 28722792265 —
  // locally the list re-render window hid the double match).
  await hit.click()
  await expect(
    page.getByLabel('block details').getByText('Roundtrip content stored via the live /api/store handler.'),
  ).toBeVisible()
})
