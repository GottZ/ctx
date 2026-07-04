// Live-tier spec helpers (design 06 §4.7, wave PV10). NOT a *.spec.ts, so the
// live config's testMatch ignores it.

import { expect, type Page } from '@playwright/test'

// loginAs drives the REAL login form (not a sessionStorage shortcut) and waits
// for the authenticated shell — every live spec goes through the genuine
// whoami path.
export async function loginAs(page: Page, key: string): Promise<void> {
  await page.goto('/')
  await page.locator('#api-key').fill(key)
  await page.getByRole('button', { name: /sign in/i }).click()
  await expect(page.locator('.shell')).toBeVisible()
}
