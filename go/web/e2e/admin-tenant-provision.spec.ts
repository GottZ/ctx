import { test, expect } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors } from './fixtures'

// A3c — server-admin provisioning on /admin/tenants/:id (design 05 §4 A3c,
// FE-6). The TenantProvision card mints a tenant ACTIVE: a scope (server-built
// <slug>:name, rendered read-only — the client never builds the prefix, S1) and
// a recovery key bound to this tenant_id (S2). The register slug is 'acme'
// (TENANT A, id …aaa); scope-create echoes `${ctx.slug}:${name}` = 'acme:…'.
const SHOTS = 'e2e/__shots__'
const ACME_ID = '550e8400-e29b-41d4-a716-446655440aaa'

test.describe('A3c: server-admin tenant provisioning (scope + recovery key)', () => {
  test('provision scope renders the server-built <slug>:name read-only', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, `/admin/tenants/${ACME_ID}`)

    const provision = page.locator('section[aria-label="provision scope and key"]')
    await expect(provision).toBeVisible()

    // Client sends only the bare name; the server prefixes and echoes the full
    // scope. Assert the displayed value carries the SERVER prefix, not the input.
    await provision.getByLabel('new scope name').fill('research')
    await provision.getByRole('button', { name: 'Provision scope' }).click()

    const echoed = provision.getByLabel('provisioned scope')
    await expect(echoed).toHaveValue('acme:research')
    await expect(echoed).toHaveAttribute('readonly', '')

    await page.screenshot({ path: `${SHOTS}/admin-tenant-provision.png` })
    expect(errors, errors.join('\n')).toEqual([])
  })

  test('mint recovery key sends this tenant_id and reveals the plaintext once', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, `/admin/tenants/${ACME_ID}`)

    const provision = page.locator('section[aria-label="provision scope and key"]')
    await provision.getByRole('button', { name: '+ Mint key for this tenant' }).click()
    await provision.getByLabel('key label').fill('recovery-key')
    // home scope defaults to a scope this tenant owns ('home') — leave it.

    // The mint must carry tenant_id = the page's tenant (S2 server-admin override).
    const mintReq = page.waitForRequest(
      (r) => r.url().includes('/api/manage') && (r.postData() ?? '').includes('"action":"api-key-create"'),
    )
    await provision.getByRole('button', { name: 'Mint key', exact: true }).click()

    const body = JSON.parse((await mintReq).postData() ?? '{}') as { data?: { tenant_id?: string } }
    expect(body.data?.tenant_id).toBe(ACME_ID)

    // Reveal-once: the plaintext is shown exactly once (RevealOnceKey hygiene).
    await expect(provision.getByLabel('new api key')).toHaveValue(/ctx_sk_TESTKEY/)
    await expect(provision.getByRole('button', { name: /stored it/ })).toBeVisible()

    expect(errors, errors.join('\n')).toEqual([])
  })

  test('cross-tenant / unowned scope → server 403 rendered as a banner', async ({ page }) => {
    // The enforcement is server-side (BE5-3 / SEC-6 prefix-from-ar.TenantID); this
    // is the FE RENDER proof — the Fault seam returns 403 and the card surfaces it.
    await seedSession(page, {
      role: 'server-admin',
      theme: 'dark',
      faults: [{ action: 'scope-create', status: 403, error: 'forbidden: scope target belongs to another tenant' }],
    })
    await gotoArea(page, `/admin/tenants/${ACME_ID}`)

    const provision = page.locator('section[aria-label="provision scope and key"]')
    await provision.getByLabel('new scope name').fill('research')
    await provision.getByRole('button', { name: 'Provision scope' }).click()

    const banner = provision.getByRole('alert')
    await expect(banner).toBeVisible()
    await expect(banner).toContainText('another tenant')
    // No scope was provisioned — the read-only echo never appears.
    await expect(provision.getByLabel('provisioned scope')).toHaveCount(0)
  })
})
