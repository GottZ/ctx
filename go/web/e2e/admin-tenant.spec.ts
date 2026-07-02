import { test, expect } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors } from './fixtures'

// Role-gated areas + routing behaviour (waves A2, TK3, N4, N5, N7). The router
// (and thus beforeLoad) only mounts once App.svelte has finished session.restore,
// so caps are resolved when the landing/guard run — no loading-race here.

// Eyeball dumps live under the gitignored e2e/.results/ tree since PV3 killed
// e2e/__shots__ (committed __screenshots__ baselines replaced it); these
// remaining manual dumps migrate to PageContracts in wave PV7.
const SHOTS = 'e2e/.results/shots'

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

  test('A0-FE: server-admin sees the scope map', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/admin')
    const scopeMap = page.locator('section[aria-label="scope map"]')
    await expect(scopeMap).toBeVisible()
    await expect(scopeMap).toContainText('home') // scope name
    await expect(scopeMap).toContainText('128') // block_count for home
    await expect(scopeMap).toContainText(/unmapped/i) // tenant_id null row (legacy)
    await page.screenshot({ path: `${SHOTS}/admin-scopemap.png` })
  })

  test('A4: tenant detail → set a scope quota', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/admin')
    await page.getByRole('link', { name: 'acme' }).click() // tenant register slug link
    await expect(page).toHaveURL(/\/admin\/tenants\/550e8400/)
    await expect(page.getByRole('heading', { name: 'tenant detail' })).toBeVisible()
    // one QuotaForm per scope the tenant owns (home/shared from scope-overview).
    const homeQuota = page.locator('form[aria-label="quota for scope home"]')
    await expect(homeQuota).toBeVisible()
    await homeQuota.getByLabel('daily cost (USD)').fill('9')
    await homeQuota.getByRole('button', { name: 'save quota' }).click()
    // the set → re-get cycle completes and shows the saved marker.
    await expect(homeQuota.getByText('saved')).toBeVisible()
    await page.screenshot({ path: `${SHOTS}/admin-tenant-detail.png` })
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

  test('N4: member landing / → /home', async ({ page }) => {
    await seedSession(page, { role: 'member', theme: 'dark' })
    await gotoArea(page, '/')
    await expect(page).toHaveURL(/\/home$/)
  })

  test('N4: server-admin landing / → /status', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/')
    await expect(page).toHaveURL(/\/status$/)
  })

  test('N5: member guarded out of /admin → landing', async ({ page }) => {
    await seedSession(page, { role: 'member', theme: 'dark' })
    await gotoArea(page, '/admin')
    await expect(page).toHaveURL(/\/home$/)
  })

  test('N5: member guarded out of /tenant → landing', async ({ page }) => {
    await seedSession(page, { role: 'member', theme: 'dark' })
    await gotoArea(page, '/tenant')
    await expect(page).toHaveURL(/\/home$/)
  })

  test('N7: identity badge shows tenant + role per tier', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/blocks')
    await expect(page.locator('nav.rail [aria-label="Role: owner"]')).toBeVisible()
    await expect(page.locator('nav.rail')).toContainText('Acme Corp')
  })

  test('N6: member /home capability screen', async ({ page }) => {
    await seedSession(page, { role: 'member', theme: 'dark' })
    await gotoArea(page, '/home')
    await expect(page.getByRole('heading', { name: /Welcome/ })).toBeVisible()
    await expect(page.locator('main.content')).toContainText('Acme Corp') // tenant card
    await expect(page.getByRole('link', { name: /Browse blocks/ })).toBeVisible()
    await page.screenshot({ path: `${SHOTS}/home-member.png` })
  })
})

// Tenant key management (TK4 create+reveal-once, TK5 revoke+self-revoke guard,
// TK6 quota) — driven against the api-key-create/delete + tenant-quota-get mocks.
test.describe('tenant key management', () => {
  test('TK6: quota card renders the limits', async ({ page }) => {
    await seedSession(page, { role: 'tenant-admin', theme: 'dark' })
    await gotoArea(page, '/tenant')
    const quota = page.locator('section[aria-label="tenant quota"]')
    await expect(quota).toBeVisible()
    await expect(quota).toContainText('$5.00') // daily_cost_usd: 5
    await page.screenshot({ path: `${SHOTS}/tenant-functional.png` })
  })

  test('TK4: create reveals the plaintext key exactly once', async ({ page }) => {
    await seedSession(page, { role: 'tenant-admin', theme: 'dark' })
    await gotoArea(page, '/tenant')
    await page.getByRole('button', { name: '+ New key' }).click()
    const dialog = page.getByRole('dialog')
    await expect(dialog).toBeVisible()
    await dialog.getByPlaceholder('who/what this key is for').fill('ci-2')
    await dialog.getByRole('button', { name: 'Create key' }).click()
    // Show-once reveal: plaintext surfaced in the role=status region's input.
    await expect(dialog.getByLabel('new api key — plaintext, shown once')).toHaveValue(
      'ctx_sk_TESTKEY_reveal_once_do_not_persist',
    )
    await page.screenshot({ path: `${SHOTS}/tenant-reveal-key.png` })
    await dialog.getByRole('button', { name: /stored it/ }).click()
    await expect(dialog).toBeHidden() // ack wipes + closes
  })

  test('TK5: self-revoke guard — own key disabled, others revocable', async ({ page }) => {
    await seedSession(page, { role: 'tenant-admin', theme: 'dark' })
    await gotoArea(page, '/tenant')
    const ownRow = page.locator('tbody tr', { hasText: 'this key' })
    await expect(ownRow.getByRole('button', { name: /revoke/i })).toBeDisabled()
    const otherRow = page.locator('tbody tr', { hasText: 'ci-runner' })
    await expect(otherRow.getByRole('button', { name: /revoke/i })).toBeEnabled()
  })
})
