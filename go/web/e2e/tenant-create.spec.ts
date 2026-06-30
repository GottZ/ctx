import { test, expect } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors } from './fixtures'

// Self-service onboarding — FE-A3a TenantCreateDialog (compound tenant-create +
// reveal-once owner key). Driven against the FLAT tenant-create fixture
// (manageFixture: { success, tenant, scope:'<slug>:main', owner_key_id, owner_key }).

const SHOTS = 'e2e/__shots__'

test.describe('A3a: tenant create + reveal-once owner key', () => {
  test('server-admin creates a tenant and the owner key is revealed exactly once', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/admin')

    await page.getByRole('button', { name: '+ New tenant' }).click()
    const dialog = page.getByRole('dialog')
    await expect(dialog).toBeVisible()

    // Default caps are prefilled (BEQ-1b: no accidental uncapped tenant).
    await expect(dialog.getByLabel('max scopes')).toHaveValue('25')
    await expect(dialog.getByLabel('max keys')).toHaveValue('50')

    await dialog.getByPlaceholder('e.g. acme-research').fill('globex')
    await dialog.getByPlaceholder('e.g. Acme Research').fill('Globex Inc')
    await dialog.getByRole('button', { name: 'Create tenant' }).click()

    // The FLAT owner_key plaintext is surfaced once in the RevealOnceKey input.
    await expect(dialog.getByLabel('new api key — plaintext, shown once')).toHaveValue(
      'ctx_sk_TESTOWNER_reveal_once_do_not_persist',
    )
    // The server-built initial scope is shown for hand-off context.
    await expect(dialog).toContainText('globex:main')
    await page.screenshot({ path: `${SHOTS}/admin-tenant-create-reveal.png` })

    // Ack wipes the plaintext + closes; it must not linger anywhere in the DOM.
    await dialog.getByRole('button', { name: /stored it/ }).click()
    await expect(dialog).toBeHidden()
    await expect(page.getByText('ctx_sk_TESTOWNER_reveal_once_do_not_persist')).toHaveCount(0)

    expect(errors, errors.join('\n')).toEqual([])
  })

  test('Neg: a member is guarded out of /admin (no create surface)', async ({ page }) => {
    await seedSession(page, { role: 'member', theme: 'dark' })
    await gotoArea(page, '/admin')
    // Route guard redirects to the member landing — the create CTA is unreachable.
    await expect(page).toHaveURL(/\/home$/)
    await expect(page.getByRole('button', { name: '+ New tenant' })).toHaveCount(0)
  })
})
