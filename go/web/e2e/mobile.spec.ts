import { test, expect } from '@playwright/test'
import { seedSession, gotoArea } from './fixtures'

// Mobile shell (S3): below the sm breakpoint the persistent rail is replaced by
// a top-bar + off-canvas hamburger drawer. Overrides the desktop viewport for
// this file only.
test.use({ viewport: { width: 390, height: 844 } })

test.describe('mobile drawer', () => {
  test('hamburger opens the off-canvas nav drawer', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/blocks')

    // Drawer mode: the slim mobile bar replaces the persistent rail.
    await expect(page.locator('.shell')).toBeVisible()
    await expect(page.locator('header.mobile-bar')).toBeVisible()
    await page.screenshot({ path: 'e2e/__shots__/mobile-closed.png' })

    const hamburger = page.getByRole('button', { name: 'Open navigation menu' })
    await expect(hamburger).toBeVisible()
    await hamburger.click()

    await expect(page.locator('#nav-drawer')).toBeVisible()
    await page.screenshot({ path: 'e2e/__shots__/mobile-drawer-open.png' })
  })
})
