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
    await page.screenshot({ path: 'e2e/.results/shots/mobile-closed.png' })

    const hamburger = page.getByRole('button', { name: 'Open navigation menu' })
    await expect(hamburger).toBeVisible()
    await hamburger.click()

    await expect(page.locator('#nav-drawer')).toBeVisible()
    await page.screenshot({ path: 'e2e/.results/shots/mobile-drawer-open.png' })
  })
})

test.describe('mobile cluster map', () => {
  // #441: the cluster title bar is anchored top-right with the gutter as its
  // right inset; its max-width leaves the same gutter on the left. The bar
  // fits the viewport either way, so the viewport fit cannot see a missing
  // left gutter — this asserts it directly.
  test('the cluster map bar keeps the gutter on both sides', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/graph')
    await expect(page.locator('.overview .bar .stat')).toBeVisible()
    const { left, right } = await page.evaluate(() => {
      const bar = document.querySelector('.overview .bar')?.getBoundingClientRect()
      const stage = document.querySelector('.overview')?.getBoundingClientRect()
      if (!bar || !stage) throw new Error('cluster map not mounted')
      return { left: bar.left - stage.left, right: stage.right - bar.right }
    })
    expect(right, 'the bar keeps its right inset').toBeGreaterThan(0)
    expect(left, `the bar leaves ${left} px on the left and ${right} px on the right`).toBeGreaterThanOrEqual(right - 0.5)
  })
})
