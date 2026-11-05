// /gateway guard + nav beyond the generated contract dimensions (design/05
// §4.6, wave W05-05). The PageContract (registry.ts 'gateway') runs in the
// gateway-ON world: its generated deny proves the TIER gate (member ⇒ /home).
// This file carries the FLAG half and the nav pairing W05-01 handed over:
//   - tenant-admin with model_api.enabled=false ⇒ redirect /status, no
//     model-consumer-list call, no Model API nav item
//   - tenant-admin against a server without model_api ⇒ the same (fail-closed)
//   - member with the gateway ON ⇒ redirect /home, no call, no nav item
//   - server-admin with enabled=false ⇒ reaches the area (preparation) + the
//     "switched off" note, nav item present
//   - tenant-admin with the gateway ON ⇒ reaches the area through the nav item
//   - server-admin tenant filter ⇒ tenant_id on the wire, foreign rows gone

import { expect, test, type Page } from '@playwright/test'
import { SENTINEL, gotoArea, modelApiCapabilities, seedSession, type SeededSession } from './fixtures'

const TENANT_A_ID = '550e8400-e29b-41d4-a716-446655440aaa'

function listCalls(session: SeededSession) {
  return session.calls.filter((c) => c.action === 'model-consumer-list')
}

function modelApiLink(page: Page) {
  return page.locator('nav.rail[aria-label="Primary"]').getByRole('link', { name: 'Model API' })
}

/** Deep-link /gateway and wait until the guard redirect (if any) has landed. */
async function deepLink(page: Page): Promise<void> {
  await page.goto('/gateway')
  await page.locator('.shell').waitFor({ state: 'visible', timeout: 10_000 })
  await page.locator('main.content > *').first().waitFor({ state: 'attached', timeout: 10_000 })
}

test.describe('gateway guard + nav (W05-05)', () => {
  test('gateway: tenant-admin with gateway.enabled=false is redirected to /status — no list call, no nav item', async ({ page }) => {
    const session = await seedSession(page, { role: 'tenant-admin', theme: 'dark', capabilities: modelApiCapabilities(false) })
    await deepLink(page)
    await expect(page).toHaveURL(/\/status$/)
    await expect(page.locator('nav.rail[aria-label="Primary"]')).toBeVisible()
    await expect(modelApiLink(page)).toHaveCount(0)
    expect(listCalls(session), 'model-consumer-list must not fire before the switch').toEqual([])
  })

  test('gateway: tenant-admin against a server without model_api is redirected to /status (fail-closed)', async ({ page }) => {
    const session = await seedSession(page, { role: 'tenant-admin', theme: 'dark' })
    await deepLink(page)
    await expect(page).toHaveURL(/\/status$/)
    await expect(modelApiLink(page)).toHaveCount(0)
    expect(listCalls(session)).toEqual([])
  })

  test('gateway: member with the gateway on is redirected to /home — no list call, no nav item', async ({ page }) => {
    const session = await seedSession(page, { role: 'member', theme: 'dark', capabilities: modelApiCapabilities(true) })
    await deepLink(page)
    await expect(page).toHaveURL(/\/home$/)
    await expect(modelApiLink(page)).toHaveCount(0)
    expect(listCalls(session)).toEqual([])
  })

  test('gateway: server-admin reaches /gateway with gateway.enabled=false (preparation) and sees the off note', async ({ page }) => {
    const session = await seedSession(page, { role: 'server-admin', theme: 'dark', capabilities: modelApiCapabilities(false) })
    await gotoArea(page, '/gateway')
    await expect(page).toHaveURL(/\/gateway$/)
    await expect(page.getByRole('heading', { name: 'Model API', exact: true })).toBeVisible()
    await expect(page.locator('main.content')).toContainText('gateway.enabled is off')
    await expect(modelApiLink(page)).toHaveCount(1)
    await expect(page.locator('main.content tr[data-consumer-row]').first()).toBeVisible()
    expect(listCalls(session).length).toBeGreaterThan(0)
  })

  test('gateway: tenant-admin with the gateway on reaches the area through the Model API nav item', async ({ page }) => {
    const session = await seedSession(page, { role: 'tenant-admin', theme: 'dark', capabilities: modelApiCapabilities(true) })
    await gotoArea(page, '/status')
    expect(listCalls(session), 'no list call outside the area').toEqual([])
    await modelApiLink(page).click()
    await expect(page).toHaveURL(/\/gateway$/)
    await expect(modelApiLink(page)).toHaveAttribute('aria-current', 'page')
    await expect(page.locator('main.content tr[data-consumer-row]')).toHaveCount(4)
    // no off note for a tenant-admin: the area is only reachable while the gateway is on
    await expect(page.locator('main.content')).not.toContainText('gateway.enabled is off')
  })

  test('gateway: server-admin tenant filter sends tenant_id and drops the foreign tenant rows', async ({ page }) => {
    const session = await seedSession(page, { role: 'server-admin', theme: 'dark', capabilities: modelApiCapabilities(true) })
    await gotoArea(page, '/gateway')
    const content = page.locator('main.content')
    // all tenants: both tenants' rows, with the tenant column
    await expect(content).toContainText(SENTINEL.B)
    await expect(content).toContainText(SENTINEL.A)
    await content.getByLabel('tenant').selectOption(TENANT_A_ID)
    await expect(content).not.toContainText(SENTINEL.B)
    await expect(content).toContainText(SENTINEL.A)
    const last = listCalls(session).at(-1)?.body as { data?: { tenant_id?: string; cursor?: string } } | undefined
    expect(last?.data?.tenant_id).toBe(TENANT_A_ID)
    expect(last?.data?.cursor, 'a filter change starts at page 1').toBeUndefined()
  })
})
