import { test, expect } from '@playwright/test'
import type { Page, Route } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors } from './fixtures'

// FE-9 / FE-SS1 — tenant-admin self-service scope provisioning on /tenant (design
// 05-frontend-a3-selfservice §4 SS1). The tenant-admin sends ONLY a bare `name`;
// the server namespaces it to `<tenant_slug>:<name>` (S1) and echoes the FULL
// scope, which the card renders read-only — the client never builds the prefix.
//
// Identity = tenant B (Globex, slug 'globex') so the server prefix is the clean
// self-service shape `globex:<name>`, never the default tenant's flat scopes. The
// fixtures already mock scope-create (echoes `${ctx.slug}:${name}`), scope-list
// (tenant-scoped rows) and tenant-usage-get (caps 25/50); negatives ride the
// Fault seam (429 over-limit) or the client charset guard (no request).
const B_ID = '550e8400-e29b-41d4-a716-446655440bbb'

/** scope-list / tenant-usage-get override layered over the fixtures (everything
 *  else falls through). Lets a single test pin scope_count to the cap. */
async function installUsage(page: Page, usage: Record<string, unknown>): Promise<void> {
  await page.route('**/api/manage', async (route: Route) => {
    let action: string | undefined
    try {
      action = (route.request().postDataJSON() as { action?: string } | null)?.action
    } catch {
      action = undefined
    }
    if (action === 'tenant-usage-get') return route.fulfill({ json: { success: true, usage } })
    return route.fallback()
  })
}

test.describe('FE-SS1: self-service scope provisioning', () => {
  test('create scope renders the server-built <slug>:name read-only', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'tenant-admin', theme: 'dark', tenant: 'B' })
    await gotoArea(page, '/tenant')

    const card = page.locator('section[aria-label="self-service scopes"]')
    await expect(card).toBeVisible()

    await card.getByLabel('new scope name').fill('research')
    await card.getByRole('button', { name: 'Create scope' }).click()

    // The displayed value carries the SERVER prefix (globex:), not the bare input.
    const echoed = card.getByLabel('created scope')
    await expect(echoed).toHaveValue('globex:research')
    await expect(echoed).toHaveAttribute('readonly', '')
    expect(errors, errors.join('\n')).toEqual([])
  })

  test('a name with ":" is blocked client-side — no scope-create request fires', async ({ page }) => {
    await seedSession(page, { role: 'tenant-admin', theme: 'dark', tenant: 'B' })
    await gotoArea(page, '/tenant')

    const card = page.locator('section[aria-label="self-service scopes"]')

    // Track any scope-create that leaks past the client guard (there must be none).
    let createSent = false
    page.on('request', (r) => {
      if (r.url().includes('/api/manage') && (r.postData() ?? '').includes('"action":"scope-create"')) {
        createSent = true
      }
    })

    await card.getByLabel('new scope name').fill('bad:name')
    // The live preview tints the violation before submit.
    await expect(card.getByLabel('scope preview')).toHaveText('globex:bad:name')
    await card.getByRole('button', { name: 'Create scope' }).click()

    // Client-block: a validation problem shows, and NO request was sent.
    await expect(card.getByRole('alert')).toContainText('no ":"')
    expect(createSent).toBe(false)
  })

  test('over max_scopes → the server 429 surfaces as a banner (Fault afterCalls)', async ({ page }) => {
    // afterCalls:1 — the first scope-create succeeds (echo shows globex:research),
    // the SECOND is over the cap and faults 429; the card renders the server error.
    await seedSession(page, {
      role: 'tenant-admin',
      theme: 'dark',
      tenant: 'B',
      faults: [{ action: 'scope-create', status: 429, afterCalls: 1 }],
    })
    await gotoArea(page, '/tenant')

    const card = page.locator('section[aria-label="self-service scopes"]')

    await card.getByLabel('new scope name').fill('research')
    await card.getByRole('button', { name: 'Create scope' }).click()
    await expect(card.getByLabel('created scope')).toHaveValue('globex:research')

    // Second create → 429 (the fixtures DEFAULT_ERROR[429] = 'tenant scope quota exceeded').
    await card.getByLabel('new scope name').fill('second')
    await card.getByRole('button', { name: 'Create scope' }).click()
    await expect(card.getByRole('alert')).toContainText('quota exceeded')
  })

  test('at the scope cap the create is disabled with a limit banner', async ({ page }) => {
    // Pin tenant-usage-get to scope_count == max_scopes (5/5) → atScopeLimit true.
    await seedSession(page, { role: 'tenant-admin', theme: 'dark', tenant: 'B' })
    await installUsage(page, { tenant_id: B_ID, max_scopes: 5, max_keys: 50, scope_count: 5, key_count: 2 })
    await gotoArea(page, '/tenant')

    const card = page.locator('section[aria-label="self-service scopes"]')
    await expect(card.getByRole('button', { name: 'Create scope' })).toBeDisabled()
    await expect(card.getByLabel('new scope name')).toBeDisabled()
    await expect(card.getByText(/scope limit reached \(5\/5\)/)).toBeVisible()
  })
})
