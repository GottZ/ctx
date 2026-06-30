import { test, expect } from '@playwright/test'
import type { Request } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors } from './fixtures'

// FE-D2 — KeyCreateDialog server-admin "target tenant" override (design
// 05-frontend-a3-selfservice §4 FE-D2 / Sicherheit S2). The field is capability-
// gated on caps.viewTenants (server-admin only). A tenant-admin never sees it, so
// the cross-tenant tenant_id is structurally inexpressible from the tenant-admin
// UI — that path NEVER puts tenant_id on the wire (the unchanged TK4 behaviour).
// A server-admin who fills it mints WITH tenant_id; empty omits it again.
//
// The api-key-create fixture does NOT echo tenant_id, so the assertion is on the
// REQUEST postData, not the response (design §4 / §Sicherheit S2 render-vs-enforce
// split — the real S2 enforcement is server-side, out of FE reach).

const GLOBEX = '550e8400-e29b-41d4-a716-446655440bbb'

/** True iff this is the POST /api/manage call carrying the given action. */
function isManageAction(req: Request, action: string): boolean {
  if (req.method() !== 'POST' || !req.url().includes('/api/manage')) return false
  try {
    return (req.postDataJSON() as { action?: string } | null)?.action === action
  } catch {
    return false
  }
}

test.describe('FE-D2: KeyCreateDialog target-tenant override', () => {
  test('tenant-admin: no target-tenant field; mint never carries tenant_id', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'tenant-admin', theme: 'dark' })
    await gotoArea(page, '/tenant')

    await page.getByRole('button', { name: '+ New key' }).click()
    const dialog = page.getByRole('dialog')
    await expect(dialog).toBeVisible()

    // Capability-gated: the field is absent from the DOM for a tenant-admin.
    await expect(dialog.getByPlaceholder(/tenant id/i)).toHaveCount(0)

    // The tenant-admin mint path puts NO tenant_id on the wire (S2: the client
    // can't even express the cross-tenant write).
    await dialog.getByPlaceholder('who/what this key is for').fill('ci-2')
    const reqPromise = page.waitForRequest((req) => isManageAction(req, 'api-key-create'))
    await dialog.getByRole('button', { name: 'Create key' }).click()
    const body = (await reqPromise).postDataJSON() as { data?: Record<string, unknown> }
    expect(body.data?.tenant_id).toBeUndefined()

    // TK4 reveal-once still works (no regression).
    await expect(dialog.getByLabel('new api key — plaintext, shown once')).toHaveValue(
      'ctx_sk_TESTKEY_reveal_once_do_not_persist',
    )
    expect(errors, errors.join('\n')).toEqual([])
  })

  test('server-admin: target-tenant field mints WITH tenant_id', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/tenant')

    await page.getByRole('button', { name: '+ New key' }).click()
    const dialog = page.getByRole('dialog')
    await expect(dialog).toBeVisible()

    const tenantField = dialog.getByPlaceholder(/tenant id/i)
    await expect(tenantField).toBeVisible()

    await dialog.getByPlaceholder('who/what this key is for').fill('recovery-key')
    await tenantField.fill(GLOBEX)

    const reqPromise = page.waitForRequest((req) => isManageAction(req, 'api-key-create'))
    await dialog.getByRole('button', { name: 'Create key' }).click()
    const body = (await reqPromise).postDataJSON() as { data?: Record<string, unknown> }
    expect(body.data?.tenant_id).toBe(GLOBEX)

    // Reveal-once flow is intact for the foreign-tenant mint too.
    await expect(dialog.getByLabel('new api key — plaintext, shown once')).toHaveValue(
      'ctx_sk_TESTKEY_reveal_once_do_not_persist',
    )
    expect(errors, errors.join('\n')).toEqual([])
  })

  test('server-admin: empty target-tenant omits tenant_id (optional override)', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/tenant')

    await page.getByRole('button', { name: '+ New key' }).click()
    const dialog = page.getByRole('dialog')
    await expect(dialog).toBeVisible()
    await expect(dialog.getByPlaceholder(/tenant id/i)).toBeVisible()

    // Field present but left empty → the server binds to the caller's own tenant.
    await dialog.getByPlaceholder('who/what this key is for').fill('own-key')
    const reqPromise = page.waitForRequest((req) => isManageAction(req, 'api-key-create'))
    await dialog.getByRole('button', { name: 'Create key' }).click()
    const body = (await reqPromise).postDataJSON() as { data?: Record<string, unknown> }
    expect(body.data?.tenant_id).toBeUndefined()
  })
})
