import { test, expect } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors } from './fixtures'

// A3b — tenant lifecycle on /admin/tenants/:id (design 05 §4 A3b). Driven against
// the stateful tenant-update / tenant-delete fixtures: a tenant-update records the
// status so the following read paths reflect the suspend/activate toggle. The
// register slug is 'acme' (TENANT A, id …aaa); the default tenant is delete-guarded.
// Eyeball dumps live under the gitignored e2e/.results/ tree since PV3 killed
// e2e/__shots__ (committed __screenshots__ baselines replaced it); these
// remaining manual dumps migrate to PageContracts in wave PV7.
const SHOTS = 'e2e/.results/shots'
const ACME_ID = '550e8400-e29b-41d4-a716-446655440aaa'
// Mirrors store.DefaultTenantID / DEFAULT_TENANT_ID (lib/api/tenants.ts) — hard-
// coded here to keep the spec out of the src module graph (Node test runner).
const DEFAULT_TENANT_ID = '00000000-0000-0000-0000-0000000d3fa0'

test.describe('A3b: tenant lifecycle (suspend / activate / delete)', () => {
  test('suspend toggles the visible status; activate flips it back', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, `/admin/tenants/${ACME_ID}`)

    const badge = page.locator('section[aria-label="tenant register row"] .badge')
    await expect(badge).toContainText('active')

    await page.getByRole('button', { name: 'Suspend' }).click()
    await expect(badge).toContainText('suspended')

    await page.getByRole('button', { name: 'Activate' }).click()
    await expect(badge).toContainText('active')

    await page.screenshot({ path: `${SHOTS}/admin-tenant-lifecycle.png` })
    expect(errors, errors.join('\n')).toEqual([])
  })

  test('delete requires the exact slug typed; a wrong slug keeps confirm disabled', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, `/admin/tenants/${ACME_ID}`)

    await page.getByRole('button', { name: 'Delete tenant' }).click()
    const dialog = page.getByRole('dialog')
    await expect(dialog).toBeVisible()

    // First click arms the two-step danger gate and reveals the slug field.
    const confirm = dialog.getByRole('button', { name: 'Delete', exact: true })
    await confirm.click()

    // Wrong slug → confirm stays disabled (decideConfirm → 'blocked', no call).
    await dialog.getByRole('textbox').fill('not-acme')
    await expect(confirm).toBeDisabled()

    // Exact slug → confirm enabled → delete → navigate back to the register.
    await dialog.getByRole('textbox').fill('acme')
    await expect(confirm).toBeEnabled()
    await confirm.click()
    await expect(page).toHaveURL(/\/admin$/)
  })

  test('the default tenant cannot be deleted — control disabled', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, `/admin/tenants/${DEFAULT_TENANT_ID}`)
    await expect(page.getByRole('button', { name: 'Delete tenant' })).toBeDisabled()
  })
})
