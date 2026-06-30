import { test, expect } from '@playwright/test'
import type { Page, Route } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors } from './fixtures'

// FE-10 / FE-SS2 — structural tenant usage on /tenant (design 05-frontend-a3-
// selfservice §4 SS2). The TenantUsageCard reads tenant-usage-get ("scopes N/M ·
// keys X/Y", null cap → "unlimited"); the key-create CTA is disabled at the key
// cap (atKeyLimit). Visibility only — the server caps hard (429). Identity =
// tenant B (Globex): the default fixture usage is scope_count 1 (globex:home),
// key_count 2, caps 25/50.
const B_ID = '550e8400-e29b-41d4-a716-446655440bbb'

/** tenant-usage-get override layered over the fixtures (everything else falls
 *  through) — pins the counts/caps a single test needs. */
async function installUsage(page: Page, usage: Record<string, unknown>): Promise<void> {
  await page.route('**/api/manage', async (route: Route) => {
    let action: string | undefined
    try {
      action = (route.request().postDataJSON() as { action?: string } | null)?.action
    } catch {
      action = undefined
    }
    if (action === 'tenant-usage-get') return route.fulfill({ json: { success: true, usage } })
    return route.fallback()
  })
}

test.describe('FE-SS2: structural usage + key-limit gate', () => {
  test('usage card shows scopes and keys as N/M', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'tenant-admin', theme: 'dark', tenant: 'B' })
    await gotoArea(page, '/tenant')

    const card = page.locator('section[aria-label="tenant usage"]')
    await expect(card).toBeVisible()
    await expect(card.getByLabel('scopes usage')).toHaveText('1 / 25')
    await expect(card.getByLabel('keys usage')).toHaveText('2 / 50')
    expect(errors, errors.join('\n')).toEqual([])
  })

  test('a null cap renders "unlimited"', async ({ page }) => {
    await seedSession(page, { role: 'tenant-admin', theme: 'dark', tenant: 'B' })
    await installUsage(page, { tenant_id: B_ID, max_scopes: null, max_keys: null, scope_count: 3, key_count: 4 })
    await gotoArea(page, '/tenant')

    const card = page.locator('section[aria-label="tenant usage"]')
    await expect(card.getByLabel('scopes usage')).toHaveText('3 / unlimited')
    await expect(card.getByLabel('keys usage')).toHaveText('4 / unlimited')
  })

  test('at the key cap the "+ New key" CTA is disabled with a limit banner', async ({ page }) => {
    await seedSession(page, { role: 'tenant-admin', theme: 'dark', tenant: 'B' })
    await installUsage(page, { tenant_id: B_ID, max_scopes: 25, max_keys: 50, scope_count: 1, key_count: 50 })
    await gotoArea(page, '/tenant')

    await expect(page.getByRole('button', { name: '+ New key' })).toBeDisabled()
    await expect(page.getByText(/key limit reached \(50\/50\)/)).toBeVisible()
  })
})
