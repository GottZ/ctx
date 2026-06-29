import { test, expect } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors } from './fixtures'

// Role-gated areas + routing behaviour (waves A2, TK3, N4, N5, N7). The router
// (and thus beforeLoad) only mounts once App.svelte has finished session.restore,
// so caps are resolved when the landing/guard run — no loading-race here.

const SHOTS = 'e2e/__shots__'

test.describe('role-gated areas + routing', () => {
  test('A2: server-admin sees the /admin tenant register', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/admin')
    await expect(page).toHaveURL(/\/admin$/)
    // tenant-list mock → one tenant "Acme Corp" rendered in the table.
    await expect(page.locator('main.content')).toContainText('Acme Corp')
    await page.screenshot({ path: `${SHOTS}/admin-server-admin.png` })
    expect(errors, errors.join('\n')).toEqual([])
  })

  test('TK3: tenant-admin sees the /tenant key table', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'tenant-admin', theme: 'dark' })
    await gotoArea(page, '/tenant')
    await expect(page).toHaveURL(/\/tenant$/)
    // api-key-list mock → first key label "smoke-key" rendered in the table.
    await expect(page.locator('main.content')).toContainText('smoke-key')
    await page.screenshot({ path: `${SHOTS}/tenant-tenant-admin.png` })
    expect(errors, errors.join('\n')).toEqual([])
  })

  test('N4: member landing / → /blocks', async ({ page }) => {
    await seedSession(page, { role: 'member', theme: 'dark' })
    await gotoArea(page, '/')
    await expect(page).toHaveURL(/\/blocks$/)
  })

  test('N4: server-admin landing / → /status', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/')
    await expect(page).toHaveURL(/\/status$/)
  })

  test('N5: member guarded out of /admin → landing', async ({ page }) => {
    await seedSession(page, { role: 'member', theme: 'dark' })
    await gotoArea(page, '/admin')
    await expect(page).toHaveURL(/\/blocks$/)
  })

  test('N5: member guarded out of /tenant → landing', async ({ page }) => {
    await seedSession(page, { role: 'member', theme: 'dark' })
    await gotoArea(page, '/tenant')
    await expect(page).toHaveURL(/\/blocks$/)
  })

  test('N7: identity badge shows tenant + role per tier', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/blocks')
    await expect(page.locator('nav.rail [aria-label="Role: owner"]')).toBeVisible()
    await expect(page.locator('nav.rail')).toContainText('Acme Corp')
  })
})
