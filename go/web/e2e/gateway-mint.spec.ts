// /gateway mutations (design/05 §4.6, wave W05-06) — create with reveal-once,
// deactivate / reactivate, key revoke — against the stateful W05-06 mock in
// fixtures.ts (server semantics of internal/handler/model_consumers.go):
//   G1 reveal-once: the plaintext is in the DOM exactly once (the
//      RevealOnceKey input), 0 after "I've stored it — done", never in
//      sessionStorage/localStorage/console/URL, not in the reloaded list, not
//      in a re-opened dialog
//   G2 revoke only after typing the 4 hint characters exactly (case counts)
//   G3 no kind select in the dialog, no kind on the wire
//   G4 the offer picker follows the scope (tenant alias shadows _global,
//      disabled never pickable, IDs on the wire)
//   G5 toast from cancelled_inflight: null ⇒ "continue until they finish",
//      a number ⇒ "N running streams ended"
//   G6 tier UX mirror: no "+ New consumer" without mintModelKeys, lower
//      sensitivities disabled for a tenant-admin, a 403 at the dialog
//   G7 a label with markup renders as text
//   Lead decision 1: a tenant-admin is offered only its home scope — every
//      scope option the picker shows is one the server accepts (unfiltered ⇒ 400)

import { AxeBuilder } from '@axe-core/playwright'
import { expect, test, type Page } from '@playwright/test'
import { AXE_TAGS } from './contract/axe'
import { FIXED_NOW } from './contract/contract'
import { MODEL_ALIASES, REVEAL_MARKER, modelApiCapabilities, gotoArea, seedSession, type SeededSession, type SeedOptions } from './fixtures'

const TENANT_A = { id: '550e8400-e29b-41d4-a716-446655440aaa', slug: 'acme', display_name: 'Acme Corp', status: 'active', created_at: '2026-05-01T08:00:00Z', updated_at: '2026-06-01T08:00:00Z' }
const TENANT_B = { id: '550e8400-e29b-41d4-a716-446655440bbb', slug: 'globex', display_name: 'Globex Inc', status: 'active', created_at: '2026-05-02T08:00:00Z', updated_at: '2026-06-01T08:00:00Z' }

const aliasId = (scope: string, name: string): string => {
  const a = MODEL_ALIASES.find((x) => x.scope === scope && x.name === name)
  if (!a) throw new Error(`no alias ${scope}/${name}`)
  return a.id
}

/**
 * Seed with the page clock fixed (FIXED_NOW — the mock server's now) and open
 * /gateway. `extra` registers route overrides AFTER the seed (later
 * page.route registrations take precedence) and before the navigation.
 */
async function open(page: Page, opts: Partial<SeedOptions> = {}, extra?: (page: Page) => Promise<void>): Promise<SeededSession> {
  await page.clock.setFixedTime(FIXED_NOW)
  const session = await seedSession(page, {
    role: 'tenant-admin',
    theme: 'dark',
    capabilities: modelApiCapabilities(true, true),
    ...opts,
  })
  if (extra) await extra(page)
  await gotoArea(page, '/gateway')
  await expect(page.locator('main.content tr[data-consumer-row]').first()).toBeVisible()
  return session
}

/** The server-admin tenant-list answers both tenants (the shared fixture lists one). */
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

function createBodies(session: SeededSession): Record<string, unknown>[] {
  return session.calls
    .filter((c) => c.action === 'model-consumer-create')
    .map((c) => ((c.body as { data?: Record<string, unknown> } | undefined)?.data ?? {}) as Record<string, unknown>)
}

/**
 * Elements (and text nodes) of the whole document that carry `needle` —
 * attributes, text or a form value. Default = the full plaintext; the
 * fragment 'TESTKEY' (no hint and no UI copy contains it) also catches a
 * partial leak, e.g. a truncated key in a title or label (Fremd-Review F5).
 */
async function markerCount(page: Page, needle: string = REVEAL_MARKER): Promise<number> {
  return page.evaluate((m) => {
    let n = 0
    const walk = document.createTreeWalker(document.documentElement, NodeFilter.SHOW_ELEMENT | NodeFilter.SHOW_TEXT)
    for (let node: Node | null = walk.currentNode; node !== null; node = walk.nextNode()) {
      if (node.nodeType === Node.TEXT_NODE) {
        if ((node.nodeValue ?? '').includes(m)) n++
        continue
      }
      const el = node as Element
      let hit = false
      for (const a of Array.from(el.attributes)) if (a.value.includes(m)) hit = true
      if ((el instanceof HTMLInputElement || el instanceof HTMLTextAreaElement) && el.value.includes(m)) hit = true
      if (hit) n++
    }
    return n
  }, needle)
}

async function storageDump(page: Page): Promise<string> {
  return page.evaluate(() =>
    JSON.stringify({
      session: Object.entries(sessionStorage),
      local: Object.entries(localStorage),
      url: location.href,
      title: document.title,
    }),
  )
}

function createDialog(page: Page) {
  return page.getByRole('dialog', { name: /New model consumer|Consumer created/ })
}

async function openCreate(page: Page) {
  await page.getByRole('button', { name: '+ New consumer' }).click()
  const dialog = createDialog(page)
  await expect(dialog.getByRole('heading', { name: 'New model consumer' })).toBeVisible()
  // the catalog arrived: the models fieldset renders its checkboxes or its empty hint
  await expect(dialog.locator('[data-testid="models-ready"]')).toBeAttached()
  return dialog
}

test.describe('gateway mint (W05-06)', () => {
  test('gateway mint G1: the plaintext shows exactly once, ack wipes it, nothing persists it (DOM, storage, console, list, re-open)', async ({ page }) => {
    const logs: string[] = []
    page.on('console', (msg) => logs.push(msg.text()))
    const errors: string[] = []
    page.on('pageerror', (err) => errors.push(err.message))
    const session = await open(page)
    expect(await markerCount(page)).toBe(0)

    const dialog = await openCreate(page)
    await dialog.getByLabel('label', { exact: true }).fill('reveal-probe')
    await dialog.getByRole('button', { name: 'Create consumer' }).click()
    await expect(dialog.getByRole('heading', { name: 'Consumer created' })).toBeVisible()

    // exactly once: the RevealOnceKey input
    const keyInput = dialog.getByRole('textbox', { name: 'new api key — plaintext, shown once' })
    await expect(keyInput).toHaveValue(REVEAL_MARKER)
    expect(await markerCount(page)).toBe(1)
    expect(await markerCount(page, 'TESTKEY'), 'no fragment of the key outside the reveal input').toBe(1)
    // the base URL beside it carries no secret
    await expect(dialog.locator('[data-testid="base-url"]')).toContainText('/v1')
    await expect(dialog.locator('[data-testid="base-url"]')).not.toContainText('ctxm_')
    expect(await storageDump(page)).not.toContain(REVEAL_MARKER)

    await dialog.getByRole('button', { name: "I've stored it — done" }).click()
    await expect(createDialog(page)).toHaveCount(0)
    expect(await markerCount(page)).toBe(0)
    expect(await markerCount(page, 'TESTKEY')).toBe(0)

    // the list after the reload: the new consumer with its HINT, never the plaintext
    const row = page.locator('main.content tr[data-consumer-row]', { hasText: 'reveal-probe' })
    await expect(row).toBeVisible()
    await expect(row).toContainText('ctxm_…sist')
    expect(await markerCount(page)).toBe(0)

    // re-opening the dialog starts empty
    await openCreate(page)
    expect(await markerCount(page)).toBe(0)
    await createDialog(page).getByRole('button', { name: 'Cancel' }).click()
    await expect(createDialog(page)).toHaveCount(0)

    // a full page reload: nothing survived anywhere
    await page.reload()
    await expect(page.locator('main.content tr[data-consumer-row]').first()).toBeVisible()
    expect(await markerCount(page)).toBe(0)
    expect(await markerCount(page, 'TESTKEY')).toBe(0)
    expect(await storageDump(page)).not.toContain('TESTKEY')
    expect(logs.filter((l) => l.includes(REVEAL_MARKER) || l.includes('TESTKEY')), 'console must never carry the plaintext').toEqual([])
    expect(errors).toEqual([])
    expect(createBodies(session)).toHaveLength(1)
  })

  test('gateway mint G1: dismissing the reveal with Esc drops the plaintext too', async ({ page }) => {
    await open(page)
    const dialog = await openCreate(page)
    await dialog.getByLabel('label', { exact: true }).fill('esc-probe')
    await dialog.getByRole('button', { name: 'Create consumer' }).click()
    await expect(dialog.getByRole('heading', { name: 'Consumer created' })).toBeVisible()
    expect(await markerCount(page)).toBe(1)
    await page.keyboard.press('Escape')
    await expect(createDialog(page)).toHaveCount(0)
    expect(await markerCount(page)).toBe(0)
    expect(await storageDump(page)).not.toContain(REVEAL_MARKER)
  })

  test('gateway mint G2+G5: revoking a key needs its 4 hint characters exactly; the toast says streams continue (null)', async ({ page }) => {
    const session = await open(page)
    const ci = page.locator('main.content tr[data-consumer-row]', { hasText: 'ci-chat' })
    await ci.getByRole('button', { name: 'Revoke key ctxm_…a9Fq' }).click()
    const confirm = page.getByRole('dialog', { name: 'Revoke key' })
    await expect(confirm).toContainText('New requests are refused immediately.')
    await confirm.getByRole('button', { name: 'Revoke', exact: true }).click()
    const typed = confirm.getByRole('textbox')
    await expect(typed).toBeVisible()
    const go = confirm.getByRole('button', { name: 'Revoke', exact: true })
    await expect(go).toBeDisabled()
    await typed.fill('a9fq') // wrong case
    await expect(go).toBeDisabled()
    await typed.fill('a9F')
    await expect(go).toBeDisabled()
    await typed.fill('a9Fq')
    await expect(go).toBeEnabled()
    await go.click()
    await expect(confirm).toHaveCount(0)
    const revoke = session.calls.filter((c) => c.action === 'model-key-revoke')
    expect(revoke.map((c) => (c.body as { data?: unknown }).data)).toEqual([{ key_id: '0190000000007000800000000000c0a1k0' }])
    const toast = page.locator('[data-testid="gateway-toast"]')
    await expect(toast).toContainText('Running streams continue until they finish')
    await expect(toast).not.toContainText('ended')
    await expect(toast).not.toContainText(/\b0\b/)
    // the row follows the server: no live key left
    await expect(ci.locator('[data-consumer-status]')).toHaveText('no key')
  })

  // Key ids must name exactly one row (Copilot on #121): a revoke of a key of a
  // NON-first row must take that row's key and leave every other row alone.
  test('gateway mint G2: revoking a key of a non-first row revokes that key only (ctx-peer-b, not ci-chat)', async ({ page }) => {
    const session = await open(page)
    const rows = page.locator('main.content tr[data-consumer-row]')
    const ci = rows.filter({ hasText: 'ci-chat' })
    const peer = rows.filter({ hasText: 'ctx-peer-b' })
    await peer.getByRole('button', { name: 'Revoke key ctxm_…Qe7z' }).click()
    const confirm = page.getByRole('dialog', { name: 'Revoke key' })
    await confirm.getByRole('button', { name: 'Revoke', exact: true }).click()
    await confirm.getByRole('textbox').fill('Qe7z')
    await confirm.getByRole('button', { name: 'Revoke', exact: true }).click()
    await expect(confirm).toHaveCount(0)
    // the peer lost exactly the revoked key; ci-chat is untouched. The server
    // re-judges the peer: its one live key mP2c expires 2026-06-29T21:00Z,
    // inside gateway.key_expiry_warn (14 d) ⇒ consumer AND key `expiring`
    // (consumerStatus / newKeyView — no longer superseded, it is the youngest live key)
    await expect(peer).not.toContainText('ctxm_…Qe7z')
    await expect(peer).toContainText('ctxm_…mP2c')
    await expect(peer.locator('[data-consumer-status]')).toHaveText('expiring')
    await expect(peer.locator('[data-key-status]')).toHaveText(['expiring'])
    await expect(ci).toContainText('ctxm_…a9Fq')
    await expect(ci.locator('[data-consumer-status]')).toHaveText('active')
    const sent = session.calls.filter((c) => c.action === 'model-key-revoke').map((c) => (c.body as { data?: { key_id?: string } }).data?.key_id)
    expect(sent).toEqual(['0190000000007000800000000000c0a2k0'])
  })

  test('gateway mint G2: revoking the key of an older created consumer leaves the newer one live', async ({ page }) => {
    await open(page)
    for (const label of ['older-app', 'newer-app']) {
      const dialog = await openCreate(page)
      await dialog.getByLabel('label', { exact: true }).fill(label)
      await dialog.getByRole('button', { name: 'Create consumer' }).click()
      await expect(dialog.getByRole('heading', { name: 'Consumer created' })).toBeVisible()
      await dialog.getByRole('button', { name: "I've stored it — done" }).click()
      await expect(createDialog(page)).toHaveCount(0)
    }
    const rows = page.locator('main.content tr[data-consumer-row]')
    const older = rows.filter({ has: page.locator('[data-testid="consumer-label"]', { hasText: /^older-app$/ }) })
    const newer = rows.filter({ has: page.locator('[data-testid="consumer-label"]', { hasText: /^newer-app$/ }) })
    // both carry the same hint (ctxm_…sist) — only the key id tells them apart
    await older.getByRole('button', { name: 'Revoke key ctxm_…sist' }).click()
    const confirm = page.getByRole('dialog', { name: 'Revoke key' })
    await expect(confirm).toContainText('older-app')
    await confirm.getByRole('button', { name: 'Revoke', exact: true }).click()
    await confirm.getByRole('textbox').fill('sist')
    await confirm.getByRole('button', { name: 'Revoke', exact: true }).click()
    await expect(confirm).toHaveCount(0)
    await expect(older.locator('[data-consumer-status]')).toHaveText('no key')
    await expect(newer.locator('[data-consumer-status]')).toHaveText('active')
    await expect(newer).toContainText('ctxm_…sist')
  })

  test('gateway mint G2+G5: deactivating types the newest live key tail; a server count shows as "N running streams ended"', async ({ page }) => {
    const session = await open(page, { cancelledInflight: 3 })
    const peer = page.locator('main.content tr[data-consumer-row]', { hasText: 'ctx-peer-b' })
    await peer.getByRole('button', { name: 'Deactivate ctx-peer-b' }).click()
    const confirm = page.getByRole('dialog', { name: 'Deactivate consumer' })
    await expect(confirm).toContainText('New requests are refused immediately.')
    // a peer row warns that the federation link breaks (design/05 §4.5)
    await expect(confirm).toContainText('federation')
    await confirm.getByRole('button', { name: 'Deactivate', exact: true }).click()
    const go = confirm.getByRole('button', { name: 'Deactivate', exact: true })
    await expect(go).toBeDisabled()
    await confirm.getByRole('textbox').fill('mP2c') // the OLDER key's tail
    await expect(go).toBeDisabled()
    await confirm.getByRole('textbox').fill('Qe7z') // the newest live key
    await go.click()
    await expect(confirm).toHaveCount(0)
    expect(session.calls.filter((c) => c.action === 'model-consumer-delete').map((c) => (c.body as { data?: unknown }).data)).toEqual([
      { id: '0190000000007000800000000000c0a2' },
    ])
    await expect(page.locator('[data-testid="gateway-toast"]')).toContainText('3 running streams ended')
    // default filter hides the now inactive consumer
    await expect(peer).toHaveCount(0)
  })

  test('gateway mint: a consumer without a live key is deactivated by typing its label', async ({ page }) => {
    await open(page)
    const row = page.locator('main.content tr[data-consumer-row]', { hasText: 'nightly-embed' })
    await row.getByRole('button', { name: 'Deactivate nightly-embed' }).click()
    const confirm = page.getByRole('dialog', { name: 'Deactivate consumer' })
    await confirm.getByRole('button', { name: 'Deactivate', exact: true }).click()
    await expect(confirm).toContainText('nightly-embed')
    await confirm.getByRole('textbox').fill('nightly-embed')
    await confirm.getByRole('button', { name: 'Deactivate', exact: true }).click()
    await expect(confirm).toHaveCount(0)
    await expect(row).toHaveCount(0)
  })

  test('gateway mint: reactivating is a plain confirm, mirrors the server tier and spares peers for a tenant-admin', async ({ page }) => {
    await open(page)
    const content = page.locator('main.content')
    // deactivate the peer first (revoking is always allowed)
    const peer = content.locator('tr[data-consumer-row]', { hasText: 'ctx-peer-b' })
    await peer.getByRole('button', { name: 'Deactivate ctx-peer-b' }).click()
    const dc = page.getByRole('dialog', { name: 'Deactivate consumer' })
    await dc.getByRole('button', { name: 'Deactivate', exact: true }).click()
    await dc.getByRole('textbox').fill('Qe7z')
    await dc.getByRole('button', { name: 'Deactivate', exact: true }).click()
    await expect(dc).toHaveCount(0)

    await content.getByRole('checkbox', { name: 'inactive', exact: true }).check()
    const legacy = content.locator('tr[data-consumer-row]', { hasText: 'legacy-app' })
    await expect(legacy).toBeVisible()
    // a tenant-admin never gets a granting control on a peer row
    await expect(content.locator('tr[data-consumer-row]', { hasText: 'ctx-peer-b' }).getByRole('button', { name: /Reactivate/ })).toHaveCount(0)
    await legacy.getByRole('button', { name: 'Reactivate legacy-app' }).click()
    const rc = page.getByRole('dialog', { name: 'Reactivate consumer' })
    await expect(rc).toContainText('no key')
    // not destructive: no typed gate, the first click commits
    await rc.getByRole('button', { name: 'Reactivate', exact: true }).click()
    await expect(rc).toHaveCount(0)
    await expect(legacy.locator('[data-consumer-status]')).toHaveText('no key')
    await expect(page.locator('[data-testid="gateway-toast"]')).toContainText('reactivated')
  })

  test('gateway mint G6: a tenant-admin without self-service gets no "+ New consumer" and no Reactivate, but keeps revoking', async ({ page }) => {
    await open(page, { capabilities: modelApiCapabilities(true, false) })
    const content = page.locator('main.content')
    await expect(content.getByRole('button', { name: '+ New consumer' })).toHaveCount(0)
    await expect(content.getByRole('button', { name: 'Revoke key ctxm_…a9Fq' })).toBeVisible()
    await expect(content.getByRole('button', { name: 'Deactivate ci-chat' })).toBeVisible()
    await content.getByRole('checkbox', { name: 'inactive', exact: true }).check()
    await expect(content.locator('tr[data-consumer-row]', { hasText: 'legacy-app' })).toBeVisible()
    await expect(content.getByRole('button', { name: /Reactivate/ })).toHaveCount(0)
  })

  test('gateway mint G6: a tenant-admin sees the lower sensitivities disabled; a 403 lands at the dialog, never as success', async ({ page }) => {
    await open(page, { faults: [{ action: 'model-consumer-create', status: 403, error: 'admin key required' }] })
    const dialog = await openCreate(page)
    const sens = dialog.getByRole('group', { name: 'prompt sensitivity' })
    await expect(sens.getByRole('radio', { name: 'credentials' })).toBeChecked()
    await expect(sens.getByRole('radio', { name: 'credentials' })).toBeEnabled()
    for (const v of ['personal', 'internal', 'public']) await expect(sens.getByRole('radio', { name: v })).toBeDisabled()
    await expect(sens).toContainText('server-admin only')
    await dialog.getByLabel('label', { exact: true }).fill('refused')
    await dialog.getByRole('button', { name: 'Create consumer' }).click()
    await expect(dialog.getByRole('alert')).toContainText('admin key required')
    await expect(dialog.getByRole('heading', { name: 'New model consumer' })).toBeVisible()
    expect(await markerCount(page)).toBe(0)
  })

  test('gateway mint G6: a server-admin may pick every sensitivity', async ({ page }) => {
    await open(page, { role: 'server-admin' })
    const dialog = await openCreate(page)
    const sens = dialog.getByRole('group', { name: 'prompt sensitivity' })
    for (const v of ['credentials', 'personal', 'internal', 'public']) await expect(sens.getByRole('radio', { name: v })).toBeEnabled()
    await sens.getByRole('radio', { name: 'internal' }).check()
    await dialog.getByLabel('label', { exact: true }).fill('sa-internal')
    await dialog.getByRole('button', { name: 'Create consumer' }).click()
    await expect(dialog.getByRole('heading', { name: 'Consumer created' })).toBeVisible()
  })

  test('gateway mint G3: no kind control in the dialog and no kind on the wire', async ({ page }) => {
    const session = await open(page)
    const dialog = await openCreate(page)
    await expect(dialog.locator('[name="kind"], #kind, [data-field="kind"]')).toHaveCount(0)
    await expect(dialog.getByLabel(/\bkind\b/i)).toHaveCount(0)
    await expect(dialog.getByRole('combobox', { name: /kind/i })).toHaveCount(0)
    await expect(dialog).not.toContainText('ctx-peer')
    await dialog.getByLabel('label', { exact: true }).fill('no-kind')
    await dialog.getByRole('button', { name: 'Create consumer' }).click()
    await expect(dialog.getByRole('heading', { name: 'Consumer created' })).toBeVisible()
    const body = createBodies(session).at(-1) ?? {}
    expect('kind' in body, JSON.stringify(body)).toBe(false)
  })

  test('gateway mint G4: the picker follows the scope — _global only without one, the tenant alias in place of the _global one, disabled never, IDs on the wire', async ({ page }) => {
    const session = await open(page)
    const dialog = await openCreate(page)
    const picked = dialog.locator('input[type="checkbox"][data-alias-id]')
    // no scope: the enabled _global aliases (legacy is disabled), sorted by name
    await expect(picked).toHaveCount(3)
    expect(await picked.evaluateAll((els) => els.map((e) => e.getAttribute('data-alias-id')))).toEqual([
      aliasId('_global', 'chat'),
      aliasId('_global', 'draft'),
      aliasId('_global', 'embed'),
    ])
    // scope home: home/chat shadows _global/chat; the DISABLED home/draft shadows _global/draft — neither is offered
    await dialog.getByLabel('scope', { exact: true }).selectOption('home')
    await expect(picked).toHaveCount(2)
    expect(await picked.evaluateAll((els) => els.map((e) => e.getAttribute('data-alias-id')))).toEqual([
      aliasId('home', 'chat'),
      aliasId('_global', 'embed'),
    ])
    await expect(dialog).not.toContainText('legacy')
    await dialog.locator(`input[data-alias-id="${aliasId('home', 'chat')}"]`).check()
    await dialog.getByLabel('label', { exact: true }).fill('scoped-chat')
    await dialog.getByRole('button', { name: 'Create consumer' }).click()
    await expect(dialog.getByRole('heading', { name: 'Consumer created' })).toBeVisible()
    const body = createBodies(session).at(-1) ?? {}
    expect(body.tenant_scope).toBe('home')
    expect(body.models).toEqual([aliasId('home', 'chat')])
    expect(JSON.stringify(body.models)).not.toContain('"chat"')
  })

  test('gateway mint G4: switching the scope back drops a selection the new scope is not offered', async ({ page }) => {
    const session = await open(page)
    const dialog = await openCreate(page)
    await dialog.getByLabel('scope', { exact: true }).selectOption('home')
    await dialog.locator(`input[data-alias-id="${aliasId('home', 'chat')}"]`).check()
    await dialog.getByLabel('scope', { exact: true }).selectOption('')
    await expect(dialog.locator(`input[data-alias-id="${aliasId('home', 'chat')}"]`)).toHaveCount(0)
    await dialog.getByLabel('label', { exact: true }).fill('global-only')
    await dialog.getByRole('button', { name: 'Create consumer' }).click()
    await expect(dialog.getByRole('heading', { name: 'Consumer created' })).toBeVisible()
    const body = createBodies(session).at(-1) ?? {}
    expect(body).not.toHaveProperty('tenant_scope')
    expect(body.models ?? []).toEqual([])
  })

  test('gateway mint lead gate: a tenant-admin is offered only its home scope, and every scope × every offered model is accepted by the server', async ({ page }) => {
    const session = await open(page)
    let dialog = await openCreate(page)
    const options = await dialog
      .getByLabel('scope', { exact: true })
      .locator('option')
      .evaluateAll((els) => els.map((e) => (e as HTMLOptionElement).value))
    await dialog.getByRole('button', { name: 'Cancel' }).click()
    // positive proof over the whole picker FIRST: no scope option it shows, with
    // every model it offers there, is one the server refuses (an unfiltered list
    // offers 'shared', whose tenant embed shadows the _global embed the
    // tenant-admin cannot see ⇒ the server's 400 at models)
    for (const value of options) {
      dialog = await openCreate(page)
      await dialog.getByLabel('scope', { exact: true }).selectOption(value)
      const boxes = dialog.locator('input[type="checkbox"][data-alias-id]')
      for (const box of await boxes.all()) await box.check()
      await dialog.getByLabel('label', { exact: true }).fill(`every-${value || 'none'}`)
      await dialog.getByRole('button', { name: 'Create consumer' }).click()
      await expect(dialog.getByRole('heading', { name: 'Consumer created' }), `scope "${value}" refused`).toBeVisible()
      await dialog.getByRole('button', { name: "I've stored it — done" }).click()
      await expect(createDialog(page)).toHaveCount(0)
    }
    expect(createBodies(session)).toHaveLength(options.length)
    // … and the option set is exactly "none" + the home scope
    expect(options).toEqual(['', 'home'])
  })

  test('gateway mint lead gate: a server-admin picks the tenant, then every scope of that tenant; tenant_id and the tenant scope ride the wire', async ({ page }) => {
    const session = await open(page, { role: 'server-admin' }, bothTenants)
    const dialog = await openCreate(page)
    const scope = dialog.getByLabel('scope', { exact: true })
    await expect(scope.locator('option')).toHaveCount(3)
    expect(await scope.locator('option').evaluateAll((els) => els.map((e) => (e as HTMLOptionElement).value))).toEqual(['', 'home', 'shared'])
    // scope shared: its own embed in place of the _global one
    await scope.selectOption('shared')
    expect(await dialog.locator('input[data-alias-id]').evaluateAll((els) => els.map((e) => e.getAttribute('data-alias-id')))).toEqual([
      aliasId('_global', 'chat'),
      aliasId('_global', 'draft'),
      aliasId('shared', 'embed'),
    ])
    // another tenant: its scope list, the selection reset
    await dialog.getByLabel('tenant', { exact: true }).selectOption(TENANT_B.id)
    await expect(scope.locator('option')).toHaveCount(2)
    expect(await scope.locator('option').evaluateAll((els) => els.map((e) => (e as HTMLOptionElement).value))).toEqual(['', 'globex:home'])
    await scope.selectOption('globex:home')
    await dialog.locator(`input[data-alias-id="${aliasId('globex:home', 'chat')}"]`).check()
    await dialog.getByLabel('label', { exact: true }).fill('globex-chat')
    await dialog.getByRole('button', { name: 'Create consumer' }).click()
    await expect(dialog.getByRole('heading', { name: 'Consumer created' })).toBeVisible()
    const body = createBodies(session).at(-1) ?? {}
    expect(body.tenant_id).toBe(TENANT_B.id)
    expect(body.tenant_scope).toBe('globex:home')
    expect(body.models).toEqual([aliasId('globex:home', 'chat')])
    const scopeCalls = session.calls.filter((c) => c.action === 'scope-list').map((c) => (c.body as { id?: string }).id)
    expect(scopeCalls).toContain(TENANT_B.id)
  })

  test('gateway mint: templates preselect the offered models of their kind and 365 days; all models carries a warning', async ({ page }) => {
    const session = await open(page)
    const dialog = await openCreate(page)
    await dialog.getByRole('button', { name: 'App · Chat' }).click()
    await expect(dialog.locator(`input[data-alias-id="${aliasId('_global', 'chat')}"]`)).toBeChecked()
    await expect(dialog.locator(`input[data-alias-id="${aliasId('_global', 'draft')}"]`)).toBeChecked()
    await expect(dialog.locator(`input[data-alias-id="${aliasId('_global', 'embed')}"]`)).not.toBeChecked()
    await expect(dialog.getByRole('radio', { name: '365 days' })).toBeChecked()
    await dialog.getByRole('button', { name: 'App · Embedding' }).click()
    await expect(dialog.locator(`input[data-alias-id="${aliasId('_global', 'embed')}"]`)).toBeChecked()
    await expect(dialog.locator(`input[data-alias-id="${aliasId('_global', 'chat')}"]`)).not.toBeChecked()
    await dialog.getByRole('checkbox', { name: 'all offered models incl. future' }).check()
    await expect(dialog).toContainText('including models added later')
    await dialog.getByLabel('label', { exact: true }).fill('all-models')
    await dialog.getByRole('button', { name: 'Create consumer' }).click()
    await expect(dialog.getByRole('heading', { name: 'Consumer created' })).toBeVisible()
    const body = createBodies(session).at(-1) ?? {}
    expect(body.all_models).toBe(true)
    expect(body).not.toHaveProperty('models')
    expect(body).not.toHaveProperty('expires_at')
  })

  test('gateway mint: expiry — 30 days from the page clock, a date to its end of day, "never" only without a ceiling', async ({ page }) => {
    const session = await open(page)
    let dialog = await openCreate(page)
    await expect(dialog.getByRole('radio', { name: 'never expires' })).toHaveCount(0)
    await dialog.getByRole('radio', { name: '30 days' }).check()
    await dialog.getByLabel('label', { exact: true }).fill('thirty')
    await dialog.getByRole('button', { name: 'Create consumer' }).click()
    await expect(dialog.getByRole('heading', { name: 'Consumer created' })).toBeVisible()
    expect(createBodies(session).at(-1)?.expires_at).toBe(new Date(FIXED_NOW.getTime() + 30 * 86_400_000).toISOString())
    await dialog.getByRole('button', { name: "I've stored it — done" }).click()

    dialog = await openCreate(page)
    await dialog.getByRole('radio', { name: 'on date' }).check()
    await dialog.getByLabel('expiry date').fill('2026-12-31')
    await dialog.getByLabel('label', { exact: true }).fill('dated')
    await dialog.getByRole('button', { name: 'Create consumer' }).click()
    await expect(dialog.getByRole('heading', { name: 'Consumer created' })).toBeVisible()
    expect(createBodies(session).at(-1)?.expires_at).toBe('2026-12-31T23:59:59.000Z')
  })

  test('gateway mint: without a lifetime ceiling "never expires" is offered and sends no expiry', async ({ page }) => {
    const caps = modelApiCapabilities(true, true)
    ;(caps.model_api as Record<string, unknown>).key_max_lifetime_s = 0
    const session = await open(page, { capabilities: caps })
    const dialog = await openCreate(page)
    await dialog.getByRole('radio', { name: 'never expires' }).check()
    await dialog.getByLabel('label', { exact: true }).fill('forever')
    await dialog.getByRole('button', { name: 'Create consumer' }).click()
    await expect(dialog.getByRole('heading', { name: 'Consumer created' })).toBeVisible()
    expect(createBodies(session).at(-1)).not.toHaveProperty('expires_at')
  })

  test('gateway mint: the CIDR allowlist is form-checked client-side and sent as a list', async ({ page }) => {
    const session = await open(page)
    const dialog = await openCreate(page)
    await dialog.getByLabel('label', { exact: true }).fill('netted')
    await dialog.getByLabel('network allowlist').fill('10.0.0.1')
    await dialog.getByRole('button', { name: 'Create consumer' }).click()
    await expect(dialog.getByRole('alert')).toContainText('needs a prefix length')
    expect(createBodies(session)).toHaveLength(0)
    await dialog.getByLabel('network allowlist').fill('10.0.0.0/8, fd00::/8')
    await dialog.getByRole('button', { name: 'Create consumer' }).click()
    await expect(dialog.getByRole('heading', { name: 'Consumer created' })).toBeVisible()
    expect(createBodies(session).at(-1)?.allowed_cidrs).toEqual(['10.0.0.0/8', 'fd00::/8'])
  })

  test('gateway mint: server errors land at the dialog — 409 label taken, 429 quota', async ({ page }) => {
    await open(page, {
      faults: [
        { action: 'model-consumer-create', status: 429, error: 'tenant model consumer quota exceeded', afterCalls: 1 },
      ],
    })
    const dialog = await openCreate(page)
    await dialog.getByLabel('label', { exact: true }).fill('ci-chat') // exists in tenant A
    await dialog.getByRole('button', { name: 'Create consumer' }).click()
    await expect(dialog.getByRole('alert')).toContainText('already exists in the tenant')
    await dialog.getByLabel('label', { exact: true }).fill('fresh')
    await dialog.getByRole('button', { name: 'Create consumer' }).click()
    await expect(dialog.getByRole('alert')).toContainText('tenant model consumer quota exceeded')
    await expect(dialog.getByRole('heading', { name: 'New model consumer' })).toBeVisible()
    expect(await markerCount(page)).toBe(0)
  })

  // The picker reads model-alias-list (the DB rows); the server judges models
  // against its registry snapshot. A row the snapshot does not offer — invalid
  // policy, the reload window, a failed reload — is the residual the picker
  // cannot mirror (Fremd-Review F4): the catalog here carries one alias the mock
  // server does not know, and the server's 400 at models must land at the
  // dialog, mark the models fieldset and never reveal (Fremd-Review F1).
  test('gateway mint: a 400 at models (catalog ahead of the server) lands at the dialog, never as success', async ({ page }) => {
    const stale = { id: '01900000-0000-7000-8000-00000000f001', scope: '_global', name: 'stale', kind: 'chat', role: 'chat', sensitivity: 'public', policy: {}, description: '', enabled: true, created_at: '2026-06-01T08:00:00Z', updated_at: '2026-06-01T08:00:00Z', writable: false }
    const session = await open(page, {}, async (p) => {
      await p.route('**/api/manage', async (route) => {
        let action: unknown
        try {
          action = (route.request().postDataJSON() as { action?: unknown } | null)?.action
        } catch {
          action = undefined
        }
        if (action !== 'model-alias-list') return route.fallback()
        const rows = MODEL_ALIASES.filter((a) => a.scope === '_global' || a.scope === 'home').map((a) => ({
          ...stale,
          id: a.id,
          scope: a.scope,
          name: a.name,
          kind: a.kind,
          role: a.kind,
          enabled: a.enabled,
        }))
        return route.fulfill({ json: { success: true, model_aliases: [...rows, stale], count: rows.length + 1 } })
      })
    })
    const dialog = await openCreate(page)
    await dialog.locator(`input[data-alias-id="${stale.id}"]`).check()
    await dialog.getByLabel('label', { exact: true }).fill('stale-pick')
    await dialog.getByRole('button', { name: 'Create consumer' }).click()
    await expect(dialog.getByRole('alert')).toContainText('is not a model alias this consumer can be granted')
    await expect(dialog.locator('fieldset.models.invalid')).toHaveCount(1)
    await expect(dialog.getByRole('heading', { name: 'New model consumer' })).toBeVisible()
    expect(await markerCount(page, 'TESTKEY')).toBe(0)
    expect(createBodies(session).at(-1)?.models).toEqual([stale.id])
  })

  test('gateway mint: a refused reactivation (429 quota) stays in the confirm with the server text', async ({ page }) => {
    await open(page, { faults: [{ action: 'model-consumer-update', status: 429, error: 'tenant model consumer quota exceeded' }] })
    const content = page.locator('main.content')
    await content.getByRole('checkbox', { name: 'inactive', exact: true }).check()
    const legacy = content.locator('tr[data-consumer-row]', { hasText: 'legacy-app' })
    await legacy.getByRole('button', { name: 'Reactivate legacy-app' }).click()
    const rc = page.getByRole('dialog', { name: 'Reactivate consumer' })
    await rc.getByRole('button', { name: 'Reactivate', exact: true }).click()
    await expect(rc.getByRole('alert')).toContainText('tenant model consumer quota exceeded')
    await expect(page.locator('[data-testid="gateway-toast"]')).toHaveCount(0)
    await rc.getByRole('button', { name: 'Cancel' }).click()
    await expect(rc).toHaveCount(0)
    await expect(content.getByRole('alert')).toContainText('tenant model consumer quota exceeded')
    await expect(legacy.locator('[data-consumer-status]')).toHaveText('inactive')
  })

  test('gateway mint G7: a label with markup renders as text in the list', async ({ page }) => {
    const errors: string[] = []
    page.on('pageerror', (err) => errors.push(err.message))
    await open(page)
    const evil = '<img src=x onerror="window.__xss=1"><script>window.__xss=2</script>xss-probe'
    const dialog = await openCreate(page)
    await dialog.getByLabel('label', { exact: true }).fill(evil)
    await dialog.getByRole('button', { name: 'Create consumer' }).click()
    await expect(dialog.getByRole('heading', { name: 'Consumer created' })).toBeVisible()
    await dialog.getByRole('button', { name: "I've stored it — done" }).click()
    const label = page.locator('main.content [data-testid="consumer-label"]', { hasText: 'xss-probe' })
    await expect(label).toHaveText(evil)
    await expect(page.locator('main.content img[src="x"]')).toHaveCount(0)
    expect(await page.evaluate(() => (window as unknown as { __xss?: unknown }).__xss)).toBeUndefined()
    expect(errors).toEqual([])
  })
  // The contract runner scans only the default state with axe; the three new
  // dialog views are scanned here — a NEW surface carries no ledger debt
  // (design 06 §3.3: new violations are fixed, never baselined).
  for (const theme of ['dark', 'light'] as const) {
    test(`gateway mint a11y (${theme}): create form, reveal and revoke confirm carry no axe violation`, async ({ page }) => {
      await page.emulateMedia({ reducedMotion: 'reduce' })
      await open(page, { theme })
      const scan = async (what: string) => {
        await page.waitForTimeout(400)
        const res = await new AxeBuilder({ page }).withTags([...AXE_TAGS]).analyze()
        expect(res.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).join(', ')}`), what).toEqual([])
      }
      const dialog = await openCreate(page)
      await scan('create form')
      await dialog.getByLabel('label', { exact: true }).fill('axe-probe')
      await dialog.getByRole('button', { name: 'Create consumer' }).click()
      await expect(dialog.getByRole('heading', { name: 'Consumer created' })).toBeVisible()
      await scan('reveal')
      await dialog.getByRole('button', { name: "I've stored it — done" }).click()
      await page.getByRole('button', { name: 'Revoke key ctxm_…a9Fq' }).click()
      const confirm = page.getByRole('dialog', { name: 'Revoke key' })
      await confirm.getByRole('button', { name: 'Revoke', exact: true }).click()
      await expect(confirm.getByRole('textbox')).toBeVisible()
      await scan('revoke confirm (armed)')
    })

    // RC-D S3: the refusal surfaces were unscanned (the contract runner scans the
    // default state only) — axe measured --danger on --danger-dim in light at
    // 4.3:1 (create refusal) and 3.96:1 (page load error) before the fix.
    test(`gateway mint a11y (${theme}): a create refusal carries no axe violation`, async ({ page }) => {
      await page.emulateMedia({ reducedMotion: 'reduce' })
      await open(page, { theme, faults: [{ action: 'model-consumer-create', status: 403, error: 'admin key required' }] })
      const scan = async (what: string) => {
        await page.waitForTimeout(400)
        const res = await new AxeBuilder({ page }).withTags([...AXE_TAGS]).analyze()
        expect(res.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).join(', ')}`), what).toEqual([])
      }
      const dialog = await openCreate(page)
      await dialog.getByLabel('label', { exact: true }).fill('refused')
      await dialog.getByRole('button', { name: 'Create consumer' }).click()
      await expect(dialog.getByRole('alert')).toContainText('admin key required')
      await scan('create refusal')
    })

    test(`gateway mint a11y (${theme}): the page load error carries no axe violation`, async ({ page }) => {
      await page.emulateMedia({ reducedMotion: 'reduce' })
      await page.clock.setFixedTime(FIXED_NOW)
      await seedSession(page, { role: 'tenant-admin', theme, capabilities: modelApiCapabilities(true, true), faults: [{ action: 'model-consumer-list', status: 500 }] })
      await gotoArea(page, '/gateway')
      await expect(page.locator('main.content .error p')).toBeVisible()
      await page.waitForTimeout(400)
      const res = await new AxeBuilder({ page }).withTags([...AXE_TAGS]).analyze()
      expect(res.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).join(', ')}`), 'page load error').toEqual([])
    })
  }
})
