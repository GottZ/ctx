import { test, expect, type Route } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors } from './fixtures'

// Type-registry admin functional gates (design 04 §4.7/§5.5, wave U10). The
// contract (registry.ts admin-types) carries the primary Edit flow + the visual/
// aria/deny dimensions; these free specs carry the behavioural gates the visual
// contract cannot: the guard redirect at the ROUTE (Ist-probe), the two-tier
// delete (builtin disabled ↔ custom confirm→wire) and the 422-DRAFT preservation.
//
// The pure mechanic (submitErrorFrom / canDeleteType) is proven in
// types-admin.svelte.test.ts; here it is exercised end-to-end through the DOM.

const BUILTIN = {
  id: '77777777-7777-7777-7777-777777777777',
  name: 'issue',
  scope: '_global',
  display_name: 'Issue',
  description: 'A tracked work item',
  builtin: true,
  is_default: false,
  config: { v: 1, retrieval: { policy: 'full-pass' }, parent: { mode: 'none' } },
  created_at: '2026-07-03T00:00:00Z',
  updated_at: '2026-07-03T00:00:00Z',
  source: 'builtin',
}
const CUSTOM = {
  ...BUILTIN,
  id: '88888888-8888-8888-8888-888888888888',
  name: 'sprint',
  scope: 'acme',
  display_name: 'Sprint',
  description: 'A tenant sprint',
  builtin: false,
  source: 'tenant',
}

test.describe('type-registry admin — guard redirect (U10, Ist-probe)', () => {
  test('member on /admin/types is redirected off the gated area, never the surface', async ({ page }) => {
    await seedSession(page, { role: 'member', theme: 'dark' })
    await gotoArea(page, '/admin/types')
    // guardArea('/admin/types', member) → landingFor(member) = /home (guard.ts).
    await expect(page).toHaveURL(/\/home$/)
    await expect(page.locator('main.content')).not.toContainText('type registry')
  })

  test('tenant-admin on /admin/types is redirected to /status (server-admin only)', async ({ page }) => {
    await seedSession(page, { role: 'tenant-admin', theme: 'dark' })
    await gotoArea(page, '/admin/types')
    await expect(page).toHaveURL(/\/status$/)
  })

  test('server-admin reaches the surface and reads the registry', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/admin/types')
    await expect(page.getByRole('heading', { name: 'Types', exact: true })).toBeVisible()
    await expect(page.locator('section.card[aria-label="type registry"]')).toContainText('issue')
  })
})

test.describe('type-registry admin — builtin guard + custom delete (U10)', () => {
  test('builtin Delete is disabled; a custom type deletes through the confirm→wire path', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'server-admin', theme: 'dark' })

    // Stateful override: a two-type list (builtin issue + custom sprint) so both
    // delete directions are observable. DELETE mutates the shared array so the
    // post-delete reload provably drops the row (not a vacuous green).
    const list = [structuredClone(BUILTIN), structuredClone(CUSTOM)]
    await page.route('**/api/types', (route: Route) =>
      route.fulfill({ json: { success: true, types: list } }),
    )
    const deleteCalls: string[] = []
    await page.route('**/api/types/*', (route: Route) => {
      const name = decodeURIComponent(new URL(route.request().url()).pathname.split('/').pop() ?? '')
      if (route.request().method() === 'DELETE') {
        deleteCalls.push(name)
        const i = list.findIndex((t) => t.name === name)
        if (i >= 0) list.splice(i, 1)
        return route.fulfill({ json: { success: true, deleted: name } })
      }
      return route.fulfill({ json: { success: true, type: list.find((t) => t.name === name) ?? BUILTIN } })
    })

    await gotoArea(page, '/admin/types')
    const rows = page.locator('section.card[aria-label="type registry"] tbody tr')
    await expect(rows).toHaveCount(2)

    // Builtin row: Delete disabled (the comfort half of the double-layer guard).
    const builtinRow = rows.filter({ hasText: 'issue' })
    await expect(builtinRow.getByRole('button', { name: 'Delete' })).toBeDisabled()

    // Custom row: Delete enabled → confirm (danger two-click arm) → wire DELETE.
    const customRow = rows.filter({ hasText: 'sprint' })
    await expect(customRow.getByRole('button', { name: 'Delete' })).toBeEnabled()
    await customRow.getByRole('button', { name: 'Delete' }).click()
    const dialog = page.getByRole('dialog')
    await expect(dialog).toBeVisible()
    const confirm = dialog.getByRole('button', { name: 'Delete type' })
    await confirm.click() // arm (danger)
    await confirm.click() // commit
    await expect(dialog).toBeHidden()

    // Wire proof + DOM proof: the DELETE fired for 'sprint' and the reload drops it.
    expect(deleteCalls).toEqual(['sprint'])
    await expect(rows).toHaveCount(1)
    await expect(page.locator('section.card[aria-label="type registry"]')).not.toContainText('sprint')
    expect(errors, errors.join('\n')).toEqual([])
  })
})

test.describe('type-registry admin — 422 draft preservation (U10 gate)', () => {
  test('a 422 on save keeps the form open with the field error and the input intact', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    // The default frozen mock lists the builtin 'issue'; override PUT → 422.
    await page.route('**/api/types/*', (route: Route) => {
      if (route.request().method() === 'PUT') {
        return route.fulfill({
          status: 422,
          json: { success: false, error: 'display_name exceeds the 120-char cap' },
        })
      }
      return route.fulfill({ json: { success: true, type: BUILTIN } })
    })

    await gotoArea(page, '/admin/types')
    const row = page.locator('section.card[aria-label="type registry"] tbody tr').filter({ hasText: 'issue' })
    await row.getByRole('button', { name: 'Edit' }).click()

    const dialog = page.getByRole('dialog')
    await expect(dialog).toBeVisible()
    // Edit a field, then save into the 422.
    const nameInput = dialog.getByRole('textbox').nth(1) // key(0), display name(1)
    await nameInput.fill('An edited display name')
    await dialog.getByRole('button', { name: 'Save type' }).click()

    // The 422-draft invariant: modal STAYS OPEN, the field error is shown, and the
    // user's edit is preserved verbatim (no silent input loss).
    await expect(dialog).toBeVisible()
    await expect(dialog.getByRole('alert')).toContainText('exceeds the 120-char cap')
    await expect(nameInput).toHaveValue('An edited display name')
  })
})
