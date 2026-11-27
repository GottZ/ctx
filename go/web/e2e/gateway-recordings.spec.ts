// Recordings view (issue #575) beyond the generated contract dimensions of
// 'recordings', 'recordings-list' and 'recordings-detail' (registry.ts):
//   - the entry links: /gateway shows "Recordings" and the consumer page
//     "View recordings" to a server-admin while a consumer records — and to a
//     tenant-admin never (positive control first); the existing fixtures carry
//     no recording consumer, so each test answers that one read itself
//     (registered after seedSession, everything else falls back to the mocks)
//   - a member is sent to its landing like the tenant-admin of the contract
//   - the scan bound: a rare filter over 10 000 rows names the rows searched
//     and Continue goes on from the walked row
//   - every named error state of a translation reaches the page
//     (no_eligible_backend), and Stop aborts a running one
//   - the raw view of a whole body: JSON text, and hex for a body that is not UTF-8

import { expect, test, type Page, type Route } from '@playwright/test'
import { gotoArea, modelApiCapabilities, seedSession } from './fixtures'
import { PANEL_CONSUMER_A, PANEL_TENANT_ID } from './gateway-panels-fixtures'
import { REC_CONSUMER, REC_ID, REC_ID_MANY, REC_MANY_RESPONSES, REC_MANY_TOTAL } from './recordings-fixtures'

const CAPS = modelApiCapabilities(true, true)

/** A consumer view as model-consumer-list/-get answer it, recording on. */
function recordingConsumer(id: string, label: string): Record<string, unknown> {
  return {
    id,
    tenant_id: PANEL_TENANT_ID.A,
    tenant_slug: 'acme',
    label,
    description: '',
    kind: 'app',
    tenant_scope: null,
    all_models: false,
    models: [],
    prompt_sensitivity: 'credentials',
    fair_weight: 1,
    admission_lane: 'bulk',
    allowed_cidrs: null,
    allow_plaintext: false,
    record_bodies: true,
    active: true,
    status: 'active',
    last_used_at: null,
    created_at: '2026-06-01T10:00:00Z',
    created_by_label: null,
    keys: [],
    keys_total: 0,
    peer: null,
  }
}

/** Answer one manage action with `json`; every other request goes to the session mocks. */
async function answer(page: Page, action: string, json: Record<string, unknown>): Promise<void> {
  await page.route('**/api/manage', async (route: Route) => {
    let a: unknown
    try {
      a = (route.request().postDataJSON() as { action?: unknown } | null)?.action
    } catch {
      a = undefined
    }
    if (a !== action) return route.fallback()
    return route.fulfill({ json })
  })
}

async function open(page: Page, path: string): Promise<void> {
  await gotoArea(page, path)
}

test.describe('recordings entry links (#575)', () => {
  test('recordings: /gateway links the recordings for a server-admin while a listed consumer records', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark', capabilities: CAPS })
    await answer(page, 'model-consumer-list', { success: true, consumers: [recordingConsumer(PANEL_CONSUMER_A, 'rec-app')], next_cursor: null })
    await open(page, '/gateway')
    await expect(page.locator('main.content tr[data-consumer-row]'), 'positive control: the recording consumer is listed').toHaveCount(1)
    const link = page.locator('main.content').getByRole('link', { name: 'Recordings', exact: true })
    await expect(link).toHaveAttribute('href', '/gateway/recordings')
    await link.click()
    await expect(page).toHaveURL(/\/gateway\/recordings$/)
    await expect(page.getByRole('heading', { level: 1, name: 'Recordings' })).toBeVisible()
  })

  test('recordings: a tenant-admin sees the recording consumer but no recordings link', async ({ page }) => {
    await seedSession(page, { role: 'tenant-admin', theme: 'dark', capabilities: CAPS })
    await answer(page, 'model-consumer-list', { success: true, consumers: [recordingConsumer(PANEL_CONSUMER_A, 'rec-app')], next_cursor: null })
    await open(page, '/gateway')
    await expect(page.locator('main.content tr[data-consumer-row]'), 'positive control: the recording consumer is listed').toHaveCount(1)
    await expect(page.locator('main.content [data-consumer-recording]'), 'the row says it records').toHaveCount(1)
    await expect(page.locator('main.content a[href^="/gateway/recordings"]')).toHaveCount(0)
  })

  test('recordings: the consumer page links its recordings for a server-admin, never for a tenant-admin', async ({ page }) => {
    const get = { success: true, consumer: recordingConsumer(PANEL_CONSUMER_A, 'rec-app'), keys_truncated: false, audit: [] }
    await seedSession(page, { role: 'server-admin', theme: 'dark', capabilities: CAPS })
    await answer(page, 'model-consumer-get', get)
    await open(page, `/gateway/consumers/${PANEL_CONSUMER_A}`)
    const link = page.locator('[data-consumer-recording]').getByRole('link', { name: 'View recordings' })
    await expect(link).toHaveAttribute('href', `/gateway/recordings/${PANEL_CONSUMER_A}`)

    const other = await page.context().newPage()
    await seedSession(other, { role: 'tenant-admin', theme: 'dark', capabilities: CAPS })
    await answer(other, 'model-consumer-get', get)
    await open(other, `/gateway/consumers/${PANEL_CONSUMER_A}`)
    await expect(other.locator('[data-consumer-recording]'), 'positive control: the policy says it records').toHaveCount(1)
    await expect(other.locator('main.content a[href^="/gateway/recordings"]')).toHaveCount(0)
  })

  test('recordings: a member is sent to its landing and no recordings call fires', async ({ page }) => {
    const session = await seedSession(page, { role: 'member', theme: 'dark', capabilities: CAPS })
    await page.goto(`/gateway/recordings/${REC_CONSUMER}/${REC_ID}`)
    await page.locator('.shell').waitFor({ state: 'visible', timeout: 10_000 })
    await expect(page).toHaveURL(/\/home$/)
    expect(session.calls.filter((c) => (c.action ?? '').startsWith('model-recording-'))).toEqual([])
  })
})

test.describe('recordings list (#575)', () => {
  test('recordings: a rare filter stops at the scan bound, names the rows searched and Continue goes on', async ({ page }) => {
    const session = await seedSession(page, { role: 'server-admin', theme: 'dark', capabilities: CAPS, state: '10k' })
    await open(page, `/gateway/recordings/${REC_CONSUMER}`)
    const content = page.locator('main.content')
    const rows = content.locator('tr[data-recording-row]')
    await expect(rows).toHaveCount(50)
    await content.getByLabel('status').selectOption('5xx')
    await content.getByRole('button', { name: 'Apply' }).click()
    await expect(content.locator('[data-scan-limit]')).toContainText('Searched 2000 rows so far, 0 matched')
    await expect(rows).toHaveCount(0)
    await content.getByRole('button', { name: 'Continue' }).click()
    await expect(rows).toHaveCount(1)
    await expect(rows.first()).toContainText('503 backend_unavailable')
    await expect(content.locator('[data-scan-limit]')).toContainText('Searched 4000 rows so far, 1 matched')
    const bodies = session.calls.filter((c) => c.action === 'model-recording-list').map((c) => (c.body as { data: Record<string, unknown> }).data)
    expect(bodies.at(-1), 'Continue: the same filter, the cursor after the walked row').toEqual({ consumer_id: REC_CONSUMER, cursor: 'walk:2000', status: '5xx' })
  })
})

test.describe('recordings detail (#575)', () => {
  const detail = `/gateway/recordings/${REC_CONSUMER}/${REC_ID}`

  test('recordings: a translation the server refuses shows its named state', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark', capabilities: CAPS })
    await open(page, detail)
    const tool = page.locator('[data-block="request/messages/3"]')
    await tool.getByRole('button', { name: 'Translate' }).click()
    await expect(tool.getByRole('alert')).toContainText('No local backend may receive recorded content')
    await expect(tool.locator('[data-error-code="no_eligible_backend"]')).toHaveCount(1)
    await expect(tool.getByRole('status')).toHaveText('translation failed')
    await expect(tool.getByRole('button', { name: 'Translate' }), 'the block can be translated again').toBeEnabled()
  })

  test('recordings: Stop aborts a running translation and asks for no further part', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark', capabilities: CAPS })
    // hold the translation: the answer never comes until the test is done
    let held = 0
    await page.route('**/api/manage', async (route: Route) => {
      let a: unknown
      try {
        a = (route.request().postDataJSON() as { action?: unknown } | null)?.action
      } catch {
        a = undefined
      }
      if (a !== 'model-recording-translate') return route.fallback()
      held++
      await new Promise(() => {})
    })
    await open(page, detail)
    const user = page.locator('[data-block="request/messages/1"]')
    await user.getByRole('button', { name: 'Translate' }).click()
    await expect(user.getByRole('status')).toHaveText('translating part 1…')
    await expect(user.locator('[data-translation]')).toHaveAttribute('aria-busy', 'true')
    await user.getByRole('button', { name: 'Stop' }).click()
    await expect(user.getByRole('status')).toHaveText('stopped after 0 of ? parts')
    await expect(user.getByRole('button', { name: 'Translate' })).toBeEnabled()
    // the held route answers translate before the session mock would record
    // it, so the count of the route is the count of the calls: one part asked
    // for, none after Stop
    expect(held).toBe(1)
    await page.unrouteAll({ behavior: 'ignoreErrors' })
  })

  test('recordings: the raw view of each whole body — JSON as text, the answer that is not UTF-8 as hex', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark', capabilities: CAPS })
    await open(page, detail)
    const req = page.locator('section[data-side="request"]')
    await req.getByRole('button', { name: 'Raw body' }).click()
    await expect(req.getByRole('button', { name: 'Raw body' })).toHaveAttribute('aria-pressed', 'true')
    await expect(req.locator('.side-bytes pre[data-raw]')).toContainText('"temperature":0.2')
    await expect(req.locator('.side-bytes')).toContainText('raw bytes · UTF-8 text')
    const resp = page.locator('section[data-side="response"]')
    await resp.getByRole('button', { name: 'Raw body' }).click()
    await expect(resp.locator('.side-bytes')).toContainText('raw bytes · hex (not valid UTF-8)')
    await expect(resp.locator('.side-bytes pre[data-raw]')).toContainText('00000010  62 61 64 20 67 77 00 ff')
  })

  test('recordings: a recording of 1200 blocks comes 500 at a time — Load more blocks appends by block_offset, grouped by side, until the end', async ({ page }) => {
    const session = await seedSession(page, { role: 'server-admin', theme: 'dark', capabilities: CAPS })
    await open(page, `/gateway/recordings/${REC_CONSUMER}/${REC_ID_MANY}`)
    const content = page.locator('main.content')
    const req = content.locator('section[data-side="request"]')
    const resp = content.locator('section[data-side="response"]')
    await expect(content.locator('[data-block]')).toHaveCount(500)
    // the first page ends inside the request: the button sits there
    await expect(req.locator('[data-more-blocks]')).toContainText(`showing 500 of ${REC_MANY_TOTAL} blocks`)
    await expect(resp.locator('[data-block]')).toHaveCount(0)
    await req.getByRole('button', { name: 'Load more blocks' }).click()
    await expect(content.locator('[data-block]')).toHaveCount(1000)
    await expect(req.locator('[data-more-blocks]')).toContainText(`showing 1000 of ${REC_MANY_TOTAL} blocks`)
    await expect(page.locator('[data-block="request/messages/500"] button').first(), 'focus moves to the first block of the new page').toBeFocused()
    await req.getByRole('button', { name: 'Load more blocks' }).click()
    await expect(content.locator('[data-block]')).toHaveCount(REC_MANY_TOTAL)
    await expect(content.locator('[data-more-blocks]'), 'at the end the button goes').toHaveCount(0)
    await expect(req.locator('[data-block]')).toHaveCount(REC_MANY_TOTAL - REC_MANY_RESPONSES)
    await expect(resp.locator('[data-block]')).toHaveCount(REC_MANY_RESPONSES)
    await expect(resp.locator('[data-block]').first()).toHaveAttribute('data-block', 'response/choices/0/content')
    const gets = session.calls.filter((c) => c.action === 'model-recording-get').map((c) => (c.body as { data: Record<string, unknown> }).data)
    expect(gets).toEqual([{ id: REC_ID_MANY }, { id: REC_ID_MANY, block_offset: 500 }, { id: REC_ID_MANY, block_offset: 1000 }])
  })
})
