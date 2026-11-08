// FOLGE-Z (briefings/FOLGE-Z-INDEX.md §1 C-SPA, §3 contract) — against the
// §3 mock world (fixtures.ts GatewayWorld.create + aliasView,
// gateway-panels-fixtures.ts subjectLimitsGet + mockStartLimits):
//   Z4 the create dialog shows every value a new consumer starts with:
//      the lane (server-admin `urgent`, tenant-admin `bulk` + the reason), the
//      start limits per op prefilled from the TARGET tenant's effective limits,
//      only changed fields on the wire as `limits`, templates 8/120 + 4/240
//      (clamped visibly below server-admin), and after the create the answer's
//      effective_limits + lane beside the key — "not reported" when the server
//      says nothing (never an invented number)
//   Z5 the catalog row badge "images: …" from effective_vision/vision_source,
//      the policy form's vision default "as the backend reports", the pool
//      row badge "vision: …" from capabilities with probed_at as its title

import { expect, test, type Page, type Route } from '@playwright/test'
import { FIXED_NOW } from './contract/contract'
import { REVEAL_MARKER, gotoArea, modelApiCapabilities, seedSession, type SeededSession, type SeedOptions } from './fixtures'

const TENANT_A = { id: '550e8400-e29b-41d4-a716-446655440aaa', slug: 'acme', display_name: 'Acme Corp', status: 'active', created_at: '2026-05-01T08:00:00Z', updated_at: '2026-06-01T08:00:00Z' }
const TENANT_B = { id: '550e8400-e29b-41d4-a716-446655440bbb', slug: 'globex', display_name: 'Globex Inc', status: 'active', created_at: '2026-05-02T08:00:00Z', updated_at: '2026-06-01T08:00:00Z' }

async function bothTenants(page: Page): Promise<void> {
  await page.route('**/api/manage', async (route) => {
    let action: unknown
    try {
      action = (route.request().postDataJSON() as { action?: unknown } | null)?.action
    } catch {
      action = undefined
    }
    if (action === 'tenant-list') return route.fulfill({ json: { success: true, tenants: [TENANT_A, TENANT_B] } })
    return route.fallback()
  })
}

async function openGateway(page: Page, opts: Partial<SeedOptions> = {}, extra?: (page: Page) => Promise<void>): Promise<SeededSession> {
  await page.clock.setFixedTime(FIXED_NOW)
  const session = await seedSession(page, { role: 'tenant-admin', theme: 'dark', capabilities: modelApiCapabilities(true, true), ...opts })
  if (extra) await extra(page)
  await gotoArea(page, '/gateway')
  await expect(page.locator('main.content tr[data-consumer-row]').first()).toBeVisible()
  return session
}

function createDialog(page: Page) {
  return page.getByRole('dialog', { name: /New model consumer|Consumer created/ })
}

async function openCreate(page: Page) {
  await page.getByRole('button', { name: '+ New consumer' }).click()
  const dialog = createDialog(page)
  await expect(dialog.getByRole('heading', { name: 'New model consumer' })).toBeVisible()
  await expect(dialog.locator('[data-testid="models-ready"]')).toBeAttached()
  return dialog
}

function bodiesOf(session: SeededSession, action: string): Record<string, unknown>[] {
  return session.calls
    .filter((c) => c.action === action)
    .map((c) => ((c.body as { data?: Record<string, unknown> } | undefined)?.data ?? {}) as Record<string, unknown>)
}

async function create(dialog: ReturnType<typeof createDialog>, label: string): Promise<void> {
  await dialog.getByLabel('label', { exact: true }).fill(label)
  await dialog.getByRole('button', { name: 'Create consumer' }).click()
  await expect(dialog.getByRole('heading', { name: 'Consumer created' })).toBeVisible()
  await expect(dialog.getByRole('textbox', { name: 'new api key — plaintext, shown once' })).toHaveValue(REVEAL_MARKER)
}

test.describe('FOLGE-Z Z4 — create dialog without silent values', () => {
  test('Z4 tenant-admin: lane bulk with the reason, start limits prefilled from the own tenant, nothing changed ⇒ no limits, reveal shows effective limits + lane', async ({ page }) => {
    const session = await openGateway(page)
    const dialog = await openCreate(page)
    const lane = dialog.getByRole('group', { name: 'admission lane' })
    await expect(lane.getByRole('radio', { name: 'bulk' }), 'Z4-lane: bulk is visibly preselected').toBeChecked()
    await expect(lane.getByRole('radio', { name: 'urgent' }), 'Z4-lane: urgent is server-admin only').toBeDisabled()
    await expect(dialog.locator('[data-testid="lane-hint"]'), 'Z4-lane: the reason is said').toContainText('urgent is server-admin only')
    const start = dialog.getByRole('group', { name: 'start limits' })
    await expect(start.getByLabel('chat rpm', { exact: true }), 'Z4-prefill: chat rpm from the tenant (settings)').toHaveValue('30')
    await expect(start.getByLabel('chat concurrency', { exact: true })).toHaveValue('2')
    await expect(start.getByLabel('embed rpm', { exact: true })).toHaveValue('120')
    await expect(start.getByRole('group', { name: 'rerank start limits' }), 'Z4-ops: no rerank alias offered ⇒ no rerank block').toHaveCount(0)
    expect(bodiesOf(session, 'gateway-limits-get').at(-1), 'Z4-source: the own tenant').toEqual({ subject_kind: 'tenant', tenant_id: TENANT_A.id })
    await create(dialog, 'z4-plain')
    const body = bodiesOf(session, 'model-consumer-create').at(-1) ?? {}
    expect(body.admission_lane, 'Z4-request: the shown lane is sent').toBe('bulk')
    expect('limits' in body, `Z4-request: nothing changed ⇒ no limits: ${JSON.stringify(body)}`).toBe(false)
    await expect(dialog.locator('[data-testid="reveal-lane"]'), 'Z4-reveal: lane of the answer').toContainText('bulk')
    await expect(dialog.locator('[data-reveal="chat.rpm"]'), 'Z4-reveal: effective chat rpm').toHaveText('30')
    await expect(dialog.locator('[data-reveal="chat.max_concurrency"]')).toHaveText('2')
    await expect(dialog.locator('[data-reveal-source="chat.rpm"]'), 'Z4-reveal: the source').toHaveText('from settings')
  })

  test('Z4 server-admin: urgent preselected, App · Chat sets 8 / 120, both go on the wire and come back as consumer rows', async ({ page }) => {
    const session = await openGateway(page, { role: 'server-admin' }, bothTenants)
    const dialog = await openCreate(page)
    await expect(dialog.getByRole('group', { name: 'admission lane' }).getByRole('radio', { name: 'urgent' }), 'Z4-lane: urgent for an app consumer').toBeChecked()
    await expect(dialog.locator('[data-testid="lane-hint"]')).toHaveCount(0)
    await dialog.getByRole('button', { name: 'App · Chat' }).click()
    const start = dialog.getByRole('group', { name: 'start limits' })
    await expect(start.getByLabel('chat concurrency', { exact: true }), 'Z4-template: concurrency 8').toHaveValue('8')
    await expect(start.getByLabel('chat rpm', { exact: true }), 'Z4-template: rpm 120').toHaveValue('120')
    await create(dialog, 'z4-urgent')
    const body = bodiesOf(session, 'model-consumer-create').at(-1) ?? {}
    expect(body.admission_lane).toBe('urgent')
    expect(body.limits, 'Z4-request: exactly the changed fields').toEqual({ chat: { max_concurrency: 8, rpm: 120 } })
    await expect(dialog.locator('[data-testid="reveal-lane"]')).toContainText('urgent')
    await expect(dialog.locator('[data-reveal="chat.max_concurrency"]')).toHaveText('8')
    await expect(dialog.locator('[data-reveal-source="chat.max_concurrency"]')).toHaveText('consumer row (server-admin)')
    await expect(dialog.locator('[data-reveal="embed.rpm"]'), 'the untouched op keeps its inherited value').toHaveText('120')
  })

  test('Z4 server-admin: the prefill follows the chosen tenant (tenant row), not the instance', async ({ page }) => {
    const session = await openGateway(page, { role: 'server-admin' }, bothTenants)
    const dialog = await openCreate(page)
    const start = dialog.getByRole('group', { name: 'start limits' })
    await expect(start.getByLabel('chat rpm', { exact: true })).toHaveValue('30')
    await dialog.getByLabel('tenant', { exact: true }).selectOption(TENANT_B.id)
    await expect(start.getByLabel('chat rpm', { exact: true }), 'Z4-source: tenant B carries a tenant_server row rpm 10').toHaveValue('10')
    expect(bodiesOf(session, 'gateway-limits-get').at(-1)).toEqual({ subject_kind: 'tenant', tenant_id: TENANT_B.id })
    await create(dialog, 'z4-globex')
    expect('limits' in (bodiesOf(session, 'model-consumer-create').at(-1) ?? {})).toBe(false)
    await expect(dialog.locator('[data-reveal="chat.rpm"]')).toHaveText('10')
    await expect(dialog.locator('[data-reveal-source="chat.rpm"]')).toHaveText('tenant row (server-admin)')
  })

  test('Z4 tenant-admin: a template is clamped visibly, widening is refused at the dialog, narrowing goes out', async ({ page }) => {
    const session = await openGateway(page)
    const dialog = await openCreate(page)
    await dialog.getByRole('button', { name: 'App · Chat' }).click()
    const start = dialog.getByRole('group', { name: 'start limits' })
    await expect(start.getByLabel('chat concurrency', { exact: true }), 'Z4-template: clamped to the inherited 2').toHaveValue('2')
    await expect(start.locator('[data-start-note="chat.max_concurrency"]'), 'Z4-template: the clamp is said').toContainText('template asks 8')
    await start.getByLabel('chat rpm', { exact: true }).fill('50')
    await dialog.getByLabel('label', { exact: true }).fill('z4-wide')
    await dialog.getByRole('button', { name: 'Create consumer' }).click()
    await expect(dialog.getByRole('alert'), 'Z4-request: widening is refused before the wire').toContainText('at most 30')
    expect(bodiesOf(session, 'model-consumer-create')).toHaveLength(0)
    await start.getByLabel('chat rpm', { exact: true }).fill('10')
    await dialog.getByRole('button', { name: 'Create consumer' }).click()
    await expect(dialog.getByRole('heading', { name: 'Consumer created' })).toBeVisible()
    expect(bodiesOf(session, 'model-consumer-create').at(-1)?.limits).toEqual({ chat: { rpm: 10 } })
    await expect(dialog.locator('[data-reveal="chat.rpm"]')).toHaveText('10')
    await expect(dialog.locator('[data-reveal-source="chat.rpm"]')).toHaveText('consumer row (tenant-admin)')
  })

  test('Z4 a server refusal of a start limit (400, param limits.<op>.<field>) stays at the dialog and marks that field', async ({ page }) => {
    const session = await openGateway(page, { role: 'server-admin' }, bothTenants)
    const dialog = await openCreate(page)
    const field = dialog.getByRole('group', { name: 'start limits' }).getByLabel('chat concurrency', { exact: true })
    // above dispatch.external_queue_per_consumer (8): only the server knows the cap
    await field.fill('9')
    await dialog.getByLabel('label', { exact: true }).fill('z4-cap')
    await dialog.getByRole('button', { name: 'Create consumer' }).click()
    await expect(dialog.getByRole('alert'), 'H1: the server text reaches the dialog').toContainText('max_concurrency over the cap')
    await expect(field, 'H1: the refused field is marked').toHaveAttribute('aria-invalid', 'true')
    await expect(dialog.getByRole('heading', { name: 'New model consumer' }), 'a refusal is never a success').toBeVisible()
    expect(bodiesOf(session, 'model-consumer-create').at(-1)?.limits).toEqual({ chat: { max_concurrency: 9 } })
  })

  test('Z4 hardening (review 1/2/4): bulk share named, the remaining inherited values shown, the reveal carries every field and every op', async ({ page }) => {
    await openGateway(page)
    const dialog = await openCreate(page)
    await expect(dialog.locator('[data-testid="lane-share"]'), 'H9: the bulk share is said').toContainText('xu_reserve_ratio')
    await expect(dialog.locator('[data-start-inherited="chat"]'), 'H2: the non-start values are visible before the create').toContainText('external: no')
    await expect(dialog.locator('[data-start-inherited="chat"]')).toContainText('monthly cost: $1')
    await create(dialog, 'z4-all-fields')
    await expect(dialog.locator('[data-reveal="chat.allow_external"]'), 'H2: every §3 E field in the reveal').toHaveText('no')
    await expect(dialog.locator('[data-reveal="chat.monthly_cost_usd"]')).toHaveText('$1')
    await expect(dialog.locator('[data-reveal="rerank.rpm"]'), 'H4/review 4: rerank limits exist even without a rerank alias — shown').toHaveText('60')
  })

  test('Z4 hardening (review 3): a tenant_tenant row caps a server-admin value — noted before, visible after', async ({ page }) => {
    const session = await openGateway(page, { role: 'server-admin' }, bothTenants)
    const dialog = await openCreate(page)
    await dialog.getByLabel('tenant', { exact: true }).selectOption(TENANT_B.id)
    const start = dialog.getByRole('group', { name: 'start limits' })
    await expect(start.getByLabel('embed rpm', { exact: true })).toHaveValue('60')
    await expect(start.locator('[data-start-note="embed.rpm"]'), 'H3: the cap is said').toContainText("the tenant's own row caps this at 60")
    await start.getByLabel('embed rpm', { exact: true }).fill('100')
    await create(dialog, 'z4-capped')
    expect(bodiesOf(session, 'model-consumer-create').at(-1)?.limits).toEqual({ embed: { rpm: 100 } })
    await expect(dialog.locator('[data-reveal="embed.rpm"]'), 'H3: the answer says what applies').toHaveText('60')
    await expect(dialog.locator('[data-reveal-source="embed.rpm"]')).toHaveText('tenant row (tenant-admin)')
  })

  test('Z4 fail-closed: a server before FOLGE-Z answers without effective_limits ⇒ "not reported", no number', async ({ page }) => {
    await openGateway(page, { gatewayLegacy: true })
    const dialog = await openCreate(page)
    await create(dialog, 'z4-legacy')
    await expect(dialog.locator('[data-testid="reveal-limits"]'), 'Z4-reveal: fail-closed').toContainText('not reported')
    await expect(dialog.locator('[data-reveal]'), 'Z4-reveal: no invented value cell').toHaveCount(0)
    await expect(dialog.locator('[data-testid="reveal-lane"]')).toContainText('bulk')
  })

  test('Z4 the limits read fails: the block says so, the create still works and sends no limits', async ({ page }) => {
    const session = await openGateway(page, { faults: [{ action: 'gateway-limits-get', status: 500 }] })
    const dialog = await openCreate(page)
    await expect(dialog.locator('[data-testid="start-limits-error"]'), 'Z4-prefill: the failure is visible').toContainText('start limits unavailable')
    await create(dialog, 'z4-noread')
    const body = bodiesOf(session, 'model-consumer-create').at(-1) ?? {}
    expect('limits' in body).toBe(false)
    expect(body.admission_lane).toBe('bulk')
    await expect(dialog.locator('[data-reveal="chat.rpm"]'), 'the answer still reports the effective values').toHaveText('30')
  })
})

test.describe('FOLGE-Z Z5 — vision without an extra step, made visible', () => {
  async function openModels(page: Page, opts: Partial<SeedOptions> = {}): Promise<void> {
    await page.clock.setFixedTime(FIXED_NOW)
    await seedSession(page, { role: 'tenant-admin', theme: 'dark', capabilities: modelApiCapabilities(true, true), ...opts })
    await gotoArea(page, '/gateway/models')
    await expect(page.getByRole('heading', { name: 'Model catalog', exact: true })).toBeVisible()
    await expect(page.locator('main.content tr[data-alias-row]').first()).toBeVisible()
  }

  function rowOf(page: Page, scope: string, name: string) {
    return page
      .locator('main.content tr[data-alias-row]')
      .filter({ has: page.locator(`[data-alias-scope="${scope}"]`) })
      .filter({ has: page.locator('[data-alias-name]', { hasText: new RegExp(`^${name}$`) }) })
  }

  test('Z5 catalog: the badge says on/off and why — backend, policy, no image-capable backend; none on embedding rows', async ({ page }) => {
    await openModels(page)
    await expect(rowOf(page, '_global', 'chat').locator('[data-alias-vision]'), 'Z5-alias: backend probe').toHaveText('images: on (backend)')
    await expect(rowOf(page, 'home', 'draft').locator('[data-alias-vision]'), 'Z5-alias: policy on').toHaveText('images: on (policy)')
    await expect(rowOf(page, '_global', 'vault').locator('[data-alias-vision]'), 'Z5-alias: policy off').toHaveText('images: off (policy)')
    await expect(rowOf(page, '_global', 'legacy').locator('[data-alias-vision]'), 'Z5-alias: no image-capable backend').toHaveText('images: off (no image-capable backend)')
    await expect(rowOf(page, '_global', 'embed').locator('[data-alias-vision]'), 'Z5-alias: images are a chat matter').toHaveCount(0)
  })

  test('Z5 catalog fail-closed: a server before FOLGE-Z ⇒ "images: not reported"', async ({ page }) => {
    await openModels(page, { gatewayLegacy: true })
    await expect(rowOf(page, '_global', 'chat').locator('[data-alias-vision]')).toHaveText('images: not reported')
  })

  test('Z5 policy form: the vision default reads "as the backend reports"', async ({ page }) => {
    await openModels(page)
    await page.locator('main.content').getByRole('button', { name: '+ New alias' }).click()
    const editor = page.getByRole('dialog', { name: 'New model alias' })
    const vision = editor.getByLabel('vision', { exact: true })
    await expect(vision.locator('option[value=""]'), 'Z5-policy: the default is explained').toHaveText('default — as the backend reports')
    await expect(editor.getByLabel('reasoning', { exact: true }).locator('option[value=""]'), 'other keys keep "default"').toHaveText('default')
  })

  test('Z5 backend pool: the vision badge per row from capabilities, probed_at as its title, unknown when absent', async ({ page }) => {
    const base = {
      base_url: 'http://10.0.0.1/x', protocol: 'openai', provider_class: 'generic', api_key_ref: '', trust: 'trusted', locality: 'local',
      scope: '_global', roles: ['chat'], model_map: {}, timeouts: {}, num_ctx: 8192, enabled: true, extra_headers: {}, extra_body: {},
      limits: {}, metadata: {}, disable_profiles: [], effective_state: 'active', cooldown_remaining_s: 0, consecutive_fails: 0,
    }
    const rows = [
      { ...base, id: 'b-spark', name: 'spark-chat', priority: 40, capabilities: { vision: true, probed_at: '2026-11-08T10:00:00Z', source: 'sglang:/get_model_info has_image_understanding' } },
      { ...base, id: 'b-cpu', name: 'llama-cpu', priority: 30, capabilities: { vision: false, probed_at: '2026-11-08T10:00:01Z', source: 'llamacpp:/props' } },
      { ...base, id: 'b-ext', name: 'openrouter', priority: 20, capabilities: { vision: null, probed_at: null, source: 'unknown' } },
      { ...base, id: 'b-old', name: 'old-row', priority: 10 },
    ]
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await page.route('**/api/manage', async (route: Route) => {
      let action: unknown
      try {
        action = (route.request().postDataJSON() as { action?: unknown } | null)?.action
      } catch {
        action = undefined
      }
      if (action === 'backend-list') return route.fulfill({ json: { success: true, backends: rows } })
      return route.fallback()
    })
    await gotoArea(page, '/settings/backends')
    const row = (id: string) => page.locator(`tbody[data-id="${id}"]`)
    await expect(row('b-spark').locator('[data-backend-vision]'), 'Z5-backend: true').toHaveText('vision: yes')
    await expect(row('b-spark').locator('[data-backend-vision]')).toHaveAttribute('title', 'probed 2026-11-08T10:00:00Z · sglang:/get_model_info has_image_understanding')
    await expect(row('b-cpu').locator('[data-backend-vision]'), 'Z5-backend: false').toHaveText('vision: no')
    await expect(row('b-ext').locator('[data-backend-vision]'), 'Z5-backend: null = unknown').toHaveText('vision: unknown')
    await expect(row('b-ext').locator('[data-backend-vision]')).toHaveAttribute('title', 'never probed · unknown')
    await expect(row('b-old').locator('[data-backend-vision]'), 'Z5-backend: no capabilities = unknown, never yes').toHaveText('vision: unknown')
    await expect(row('b-old').locator('[data-backend-vision]')).toHaveAttribute('title', 'not reported by this server')
  })
})
