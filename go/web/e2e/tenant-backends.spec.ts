import { test, expect } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors } from './fixtures'

// /tenant/backends route + gate (design 04 §4 E04-4 / §5.5, wave U11). The
// E04-4 rot-fix: before the route was registered the deep link hit NotFound.
// This spec is the NotFound→reachable gate (the tenant-admin reaches the pool,
// never the 404 copy) plus the member-negative guard probe. The visual/aria/
// deny dimensions ride the tenant-backends PageContract (registry.ts). The
// backend list is server-side tenant-filtered (backends_manage.go, E04-4); this
// spec proves REACHABILITY + the tenant self-gate, not the filter (server-side,
// §5.2/§5.4).

test.describe('tenant backends — E04-4 route gate (U11)', () => {
  test('tenant-admin reaches the pool (no NotFound), vault hidden', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'tenant-admin', theme: 'dark' })
    await gotoArea(page, '/tenant/backends')
    // NOT NotFound: the tenant pool heading renders, never the 404 copy.
    await expect(page.getByRole('heading', { name: 'Backend pool', exact: true })).toBeVisible()
    const content = page.locator('main.content')
    await expect(content).not.toContainText('No such route')
    // Pool load path reached past the tenant self-gate (fixture [] → empty state,
    // the positive control that the tenant-admin can read the pool).
    await expect(content).toContainText('no backends — create one to populate the pool')
    // The secrets vault is server-admin only → hidden in the tenant variant.
    await expect(page.locator('section.card[aria-label="secrets vault"]')).toHaveCount(0)
    // Crumb links back to the tenant self-service area.
    await page.locator('.crumb').getByRole('link', { name: 'Tenant' }).click()
    await expect(page).toHaveURL(/\/tenant$/)
    expect(errors, errors.join('\n')).toEqual([])
  })

  test('member is guarded off /tenant/backends → /home, backend-list never fires', async ({ page }) => {
    const session = await seedSession(page, { role: 'member', theme: 'dark' })
    await gotoArea(page, '/tenant/backends')
    // /tenant prefix-guard (guard.ts TIER_GATED → manageTenantKeys) redirects a member.
    await expect(page).toHaveURL(/\/home$/)
    const fired = session.calls.map((x) => x.action ?? x.path)
    expect(fired, 'no tenant backend read for a member').not.toContain('backend-list')
  })
})
