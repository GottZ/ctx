import { test, expect, type Route } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors } from './fixtures'

// Grants-tab functional gates (design 04 §7.1/§5.5, wave U11). The
// admin-tenant-detail contract carries the visual/aria/deny dimensions; these
// free specs carry the behaviour the visual contract cannot: the list→create→
// revoke wire round-trips and the deny-fault (a server 409 stays visible, the
// modal keeps the draft — no silent loss). All three grant actions are
// tierServerAdmin, matching the server-admin-only tenant-detail mount.

const TENANT_A = '550e8400-e29b-41d4-a716-446655440aaa'
const DETAIL = `/admin/tenants/${TENANT_A}`
const PANEL = 'section.card[aria-label="cross-tenant read grants"]'

test.describe('tenant grants — CRUD wire round-trips (U11)', () => {
  test('list renders, create + revoke hit the wire with the right payload, list moves', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'server-admin', theme: 'dark' })

    // Stateful grants array so create/revoke provably move the list (not vacuous
    // green). A later page.route wins; every non-grant manage action falls back
    // to the seedSession fixture (tenant-get / scope-overview / quota render).
    const grants: Record<string, unknown>[] = [
      { id: 'g-1', grantee_tenant: TENANT_A, granted_scope: 'shared', created_at: '2026-06-20T08:00:00Z', created_by: 'ke7' },
    ]
    const wire: { action: string; data?: Record<string, unknown>; id?: string }[] = []
    await page.route('**/api/manage', async (route: Route) => {
      const body = route.request().postDataJSON() as
        | { action?: string; data?: Record<string, unknown>; id?: string }
        | null
      const action = body?.action
      if (action === 'tenant-grant-list') {
        return route.fulfill({ json: { success: true, grants: [...grants] } })
      }
      if (action === 'tenant-grant-create') {
        wire.push({ action, data: body?.data })
        const g = {
          id: `g-${grants.length + 1}`,
          grantee_tenant: TENANT_A,
          granted_scope: body?.data?.granted_scope as string,
          created_at: '2026-06-30T12:00:00Z',
          created_by: 'ke7',
        }
        grants.push(g)
        return route.fulfill({ json: { success: true, grant: g } })
      }
      if (action === 'tenant-grant-delete') {
        wire.push({ action, id: body?.id })
        const i = grants.findIndex((x) => x.id === body?.id)
        if (i >= 0) grants.splice(i, 1)
        return route.fulfill({ json: { success: true, deleted: body?.id } })
      }
      return route.fallback()
    })

    await gotoArea(page, DETAIL)
    const panel = page.locator(PANEL)
    await expect(panel).toBeVisible()
    await expect(panel).toContainText('shared') // the seeded grant

    // Create: modal → enter scope → submit → tenant-grant-create on the wire.
    await panel.getByRole('button', { name: '+ Grant scope access' }).click()
    const dialog = page.getByRole('dialog')
    await expect(dialog).toBeVisible()
    await dialog.getByLabel('granted scope').fill('globex:home')
    await dialog.getByRole('button', { name: 'Grant access' }).click()
    await expect(dialog).toBeHidden()
    await expect(panel).toContainText('globex:home')

    // Revoke: danger confirm (two-click arm → commit) → tenant-grant-delete, row drops.
    const row = panel.locator('tbody tr', { hasText: 'globex:home' })
    await row.getByRole('button', { name: 'Revoke' }).click()
    const confirm = page.getByRole('dialog')
    const btn = confirm.getByRole('button', { name: 'Revoke grant' })
    await btn.click() // arm
    await btn.click() // commit
    await expect(confirm).toBeHidden()
    await expect(panel).not.toContainText('globex:home')

    // Wire proof: exactly one create carrying the (grantee, scope) pair + one delete.
    const creates = wire.filter((w) => w.action === 'tenant-grant-create')
    expect(creates).toHaveLength(1)
    expect(creates[0].data?.grantee_tenant).toBe(TENANT_A)
    expect(creates[0].data?.granted_scope).toBe('globex:home')
    expect(wire.filter((w) => w.action === 'tenant-grant-delete')).toHaveLength(1)
    expect(errors, errors.join('\n')).toEqual([])
  })
})

test.describe('tenant grants — deny-fault (U11)', () => {
  test('a 409 on create keeps the modal open with the server message and the draft', async ({ page }) => {
    await seedSession(page, {
      role: 'server-admin',
      theme: 'dark',
      faults: [{ action: 'tenant-grant-create', status: 409, error: 'grant already exists' }],
    })
    await gotoArea(page, DETAIL)
    const panel = page.locator(PANEL)
    await panel.getByRole('button', { name: '+ Grant scope access' }).click()
    const dialog = page.getByRole('dialog')
    await dialog.getByLabel('granted scope').fill('shared')
    await dialog.getByRole('button', { name: 'Grant access' }).click()
    // Deny-fault: the modal STAYS OPEN, the server message shows, the draft is
    // intact — no silent loss (§5.5 deny-fault, content-free error echo).
    await expect(dialog).toBeVisible()
    await expect(dialog.getByRole('alert')).toContainText('grant already exists')
    await expect(dialog.getByLabel('granted scope')).toHaveValue('shared')
  })
})

test.describe('tenant grants — member negative (U11)', () => {
  test('member is guarded off the tenant detail; tenant-grant-list never fires', async ({ page }) => {
    const session = await seedSession(page, { role: 'member', theme: 'dark' })
    await gotoArea(page, DETAIL)
    // /admin prefix-guard (guard.ts TIER_GATED → viewTenants) redirects a member.
    await expect(page).toHaveURL(/\/home$/)
    const fired = session.calls.map((x) => x.action ?? x.path)
    expect(fired, 'no grant read for a member').not.toContain('tenant-grant-list')
  })
})
