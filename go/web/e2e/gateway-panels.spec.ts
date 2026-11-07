// Model API panels beyond the generated contract dimensions (RC-D cluster
// C6, layer 1 — red against the S1 stubs). One test per gate of design/05
// §7 W05-09/W05-10/W05-12/W05-17/W05-20 that the 'gateway-consumer'
// PageContract (registry.ts) does not already carry in its primary flow:
//   - W05-09 server-admin gets the call detail control (positive control of
//     the "no link below server-admin" gate) — and the tenant-admin none
//   - W05-09 leak: tenant B on A's consumer ⇒ "consumer not found", A's
//     label never rendered; B on its own consumer ⇒ its sentinel (positive
//     control first)
//   - W05-10 window switch re-reads with the window's bucket
//   - W05-17 tile off on gateway=null and on a pre-RC-D answer; tenant tile
//     shows the depth class only (the mock leaks waiting:37); server-admin
//     tile carries the int/ext partition; the status page fires no panel call
//   - W05-20 tenant card links /gateway?tenant=<id>; following it filters the
//     list to that tenant (tenant_id on the wire, only its rows); a
//     tenant-admin's ?tenant= is ignored

import { expect, test, type Page } from '@playwright/test'
import { SENTINEL, gotoArea, modelApiCapabilities, seedSession, type SeededSession } from './fixtures'
import { PANEL_CALL_ID, PANEL_CONSUMER_A, PANEL_CONSUMER_B, PANEL_TENANT_ID } from './gateway-panels-fixtures'

const CAPS = modelApiCapabilities(true, true)

async function openDetail(page: Page, id: string): Promise<void> {
  await page.goto(`/gateway/consumers/${id}`)
  await page.locator('.shell').waitFor({ state: 'visible', timeout: 10_000 })
  await page.locator('main.content > *').first().waitFor({ state: 'attached', timeout: 10_000 })
}

function listBodies(session: SeededSession): Record<string, unknown>[] {
  return session.calls
    .filter((c) => c.action === 'model-consumer-list')
    .map((c) => ((c.body as { data?: Record<string, unknown> } | undefined)?.data ?? {}))
}

test.describe('consumer detail (W05-09)', () => {
  test('gateway-panels: server-admin gets the call detail control, tenant-admin none (positive control first)', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark', capabilities: CAPS })
    await openDetail(page, PANEL_CONSUMER_A)
    const row = page.locator(`table[aria-label="last calls"] tr[data-call-row]:has([data-call-id="${PANEL_CALL_ID}"])`)
    await expect(row, 'gate W05-09: geseedeter Call sichtbar').toHaveCount(1)
    await expect(row.getByRole('button', { name: /^Open call/ }), 'gate W05-09: server-admin reaches the detail').toHaveCount(1)
  })

  test('gateway-panels: tenant-admin sees the seeded call without any llmlog detail link', async ({ page }) => {
    await seedSession(page, { role: 'tenant-admin', theme: 'dark', capabilities: CAPS })
    await openDetail(page, PANEL_CONSUMER_A)
    const rows = page.locator('table[aria-label="last calls"] tr[data-call-row]')
    await expect(rows.filter({ has: page.locator(`[data-call-id="${PANEL_CALL_ID}"]`) }), 'gate W05-09: positive control').toHaveCount(1)
    await expect(page.locator('main.content a[href*="/api/llmlog"]'), 'gate W05-09: rot gegen Link').toHaveCount(0)
    await expect(page.getByRole('button', { name: /^Open call/ })).toHaveCount(0)
  })

  test("gateway-panels: leak — tenant B on A's consumer reads 'consumer not found', on its own its sentinel", async ({ page }) => {
    await seedSession(page, { role: 'tenant-admin', theme: 'dark', tenant: 'B', capabilities: CAPS })
    await openDetail(page, PANEL_CONSUMER_B)
    await expect(page.getByRole('heading', { level: 1, name: SENTINEL.B }), 'positive control: own consumer renders').toBeVisible()
    await openDetail(page, PANEL_CONSUMER_A)
    await expect(page.locator('main.content'), 'gate W05-09: No-Oracle text').toContainText('consumer not found')
    await expect(page.locator('main.content')).not.toContainText('rotated-app')
    await expect(page.locator('main.content')).not.toContainText(SENTINEL.A)
  })
})

test.describe('usage panel (W05-10)', () => {
  test('gateway-panels: the window switch re-reads /api/usage with the window bucket', async ({ page }) => {
    const session = await seedSession(page, { role: 'tenant-admin', theme: 'dark', capabilities: CAPS })
    await openDetail(page, PANEL_CONSUMER_A)
    const usage = page.locator('section[aria-label="usage"]')
    await expect(usage.getByRole('radio', { name: '24h' }), 'gate W05-10: window 24h preselected').toBeChecked()
    const before = session.calls.filter((c) => c.path === '/api/usage').length
    expect(before, 'gate W05-10: the panel read /api/usage').toBeGreaterThanOrEqual(1)
    await usage.getByRole('radio', { name: '30d' }).check()
    await expect.poll(() => session.calls.filter((c) => c.path === '/api/usage').length).toBe(before + 1)
  })
})

test.describe('status tile "Model API" (W05-17)', () => {
  test('gateway-panels: server-admin tile shows the int/ext partition (positive control)', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark', capabilities: CAPS, panels: { gatewayStatus: 'on' } })
    await gotoArea(page, '/status')
    const tile = page.locator('section[aria-label="model api"]')
    await expect(tile, 'gate W05-17: tile shown for the admin shape').toBeVisible()
    const row = tile.getByRole('table', { name: 'model api partition' }).locator('tbody tr')
    await expect(row).toHaveCount(1)
    await expect(row.first()).toContainText('http://spark:30000')
    await expect(tile, 'gate W05-17: ablaufende Keys').toContainText('2 keys expire soon')
  })

  test('gateway-panels: tile off when the status answer carries gateway = null', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark', capabilities: CAPS, panels: { gatewayStatus: 'null' } })
    await gotoArea(page, '/status')
    await expect(page.locator('section[aria-label="dispatch registry"], section[aria-label="health"]').first()).toBeVisible()
    await expect(page.locator('section[aria-label="model api"]'), 'gate W05-17: gateway=null ⇒ Kachel aus').toHaveCount(0)
  })

  test('gateway-panels: tenant tile shows the depth class only — never the leaked number', async ({ page }) => {
    const session = await seedSession(page, { role: 'tenant-admin', theme: 'dark', capabilities: CAPS, panels: { gatewayStatus: 'on' } })
    await gotoArea(page, '/status')
    const tile = page.locator('section[aria-label="model api"]')
    await expect(tile.locator('[data-tenant-depth]'), 'gate W05-17: Tiefe-Klasse').toHaveText(['high'])
    await expect(tile, 'gate W05-17: rot gegen numerische Fremdtiefe').not.toContainText('37')
    const panelCalls = session.calls.filter((c) => c.path === '/api/usage' || c.action === 'model-consumer-list')
    expect(panelCalls, 'gate W05-17: the tile rides the status poll (no own request)').toEqual([])
  })
})

test.describe('server-admin bridge (W05-20)', () => {
  test('gateway-panels: the tenant card links /gateway?tenant=<id> and the list opens filtered', async ({ page }) => {
    const session = await seedSession(page, { role: 'server-admin', theme: 'dark', capabilities: CAPS })
    await gotoArea(page, `/admin/tenants/${PANEL_TENANT_ID.B}`)
    const card = page.locator('section[aria-label="model api"]')
    const link = card.getByRole('link', { name: 'Open consumers' })
    await expect(link, 'gate W05-20: Link trägt Tenant-Filter').toHaveAttribute('href', `/gateway?tenant=${PANEL_TENANT_ID.B}`)
    await expect(card.locator('[data-consumer-count]')).toHaveText('2')
    await link.click()
    await expect(page).toHaveURL(new RegExp(`/gateway\\?tenant=${PANEL_TENANT_ID.B}$`))
    const rows = page.locator('main.content tr[data-consumer-row]')
    await expect(rows.first()).toBeVisible()
    expect(listBodies(session).at(-1)?.tenant_id, 'gate W05-20: rot gegen ignorierten Query-Parameter').toBe(PANEL_TENANT_ID.B)
    await expect(page.locator('main.content'), 'only the bridged tenant').not.toContainText('ci-chat')
  })

  test('gateway-panels: a direct /gateway?tenant=<id> filters the first list read', async ({ page }) => {
    const session = await seedSession(page, { role: 'server-admin', theme: 'dark', capabilities: CAPS })
    await gotoArea(page, `/gateway?tenant=${PANEL_TENANT_ID.B}`)
    await expect(page.locator('main.content tr[data-consumer-row]').first()).toBeVisible()
    expect(listBodies(session)[0]?.tenant_id, 'gate W05-20: the FIRST read carries the filter').toBe(PANEL_TENANT_ID.B)
  })

  test('gateway-panels: a tenant-admin ?tenant= is ignored (tenant_id is server-admin only)', async ({ page }) => {
    const session = await seedSession(page, { role: 'tenant-admin', theme: 'dark', capabilities: CAPS })
    await gotoArea(page, `/gateway?tenant=${PANEL_TENANT_ID.B}`)
    await expect(page.locator('main.content tr[data-consumer-row]').first()).toBeVisible()
    expect(listBodies(session)[0]?.tenant_id, 'never sent below server-admin').toBeUndefined()
  })
})
