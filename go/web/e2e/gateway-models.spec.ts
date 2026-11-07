// /gateway/models — the alias catalog of RC-D C5 W05-13 (design/05 §3.3,
// §4.6 ModelsPage, §5 B-8/B-17/B-23) against the stateful alias mock of
// fixtures.ts (GatewayWorld.aliasCreate / aliasUpdate / aliasPreview with the
// semantics of model_alias_manage.go and the C7 preview contract):
//   E13-1 tenant-admin: `_global` rows carry no write control, home rows do
//   E13-2 "+ New alias" sends `enabled` explicitly (false included); a
//         tenant-admin cannot pick a sensitivity below credentials
//   E13-3 edit: name and kind read-only, never on the wire (id top-level)
//   E13-4 server-admin lowering below credentials only after the confirm;
//         the confirmed request carries confirm_sensitivity_downgrade
//   E13-5 "Suggest from pool" drafts only enabled:false aliases
//   E13-6 a tenant-admin never gets the pool suggestion or the preview
//   E13-7 server-admin chain preview through a picked consumer marks external
//         rows and lists the excluded ones (C7 contract: alias by name,
//         consumer_id required — RC-D S2 alignment, W19 decision in the S2 report)
//   E13-8 a 422 `fields` entry lands at its field
// RC-D S1: red against the stub page.

import { expect, test, type Page, type Route } from '@playwright/test'
import { FIXED_NOW } from './contract/contract'
import { MODEL_ALIASES, POOL_PRIMARY, SENTINEL, gotoArea, modelApiCapabilities, seedSession, type SeededSession, type SeedOptions } from './fixtures'

const aliasId = (scope: string, name: string): string => {
  const a = MODEL_ALIASES.find((x) => x.scope === scope && x.name === name)
  if (!a) throw new Error(`no alias ${scope}/${name}`)
  return a.id
}

async function open(page: Page, opts: Partial<SeedOptions> = {}, extra?: (page: Page) => Promise<void>): Promise<SeededSession> {
  await page.clock.setFixedTime(FIXED_NOW)
  const session = await seedSession(page, { role: 'tenant-admin', theme: 'dark', capabilities: modelApiCapabilities(true, true), ...opts })
  if (extra) await extra(page)
  await gotoArea(page, '/gateway/models')
  await expect(page.getByRole('heading', { name: 'Model catalog', exact: true })).toBeVisible()
  await expect(page.locator('main.content tr[data-alias-row]').first(), 'W05-13: the catalog lists its rows').toBeVisible()
  return session
}

const rowOf = (page: Page, scope: string, name: string) =>
  page.locator('main.content tr[data-alias-row]').filter({ has: page.locator(`[data-alias-scope="${scope}"]`) }).filter({ has: page.locator('[data-alias-name]', { hasText: new RegExp(`^${name}$`) }) })

function calls(session: SeededSession, action: string): Record<string, unknown>[] {
  return session.calls.filter((c) => c.action === action).map((c) => (c.body ?? {}) as Record<string, unknown>)
}

test.describe('gateway models W05-13 alias catalog (RC-D C5)', () => {
  test('gateway models E13-1: a tenant-admin sees _global rows without write controls, home rows with them', async ({ page }) => {
    await open(page)
    await expect(rowOf(page, 'home', SENTINEL.A), 'positive control: the home sentinel alias is listed').toHaveCount(1)
    for (const name of ['chat', 'embed', 'draft', 'legacy', 'vault']) {
      const r = rowOf(page, '_global', name)
      await expect(r, `_global/${name} is listed`).toHaveCount(1)
      await expect(r.getByRole('button'), `W05-13 gate: no write control on _global/${name}`).toHaveCount(0)
      await expect(r.getByRole('checkbox'), `W05-13 gate: no enable toggle on _global/${name}`).toHaveCount(0)
    }
    await expect(rowOf(page, 'home', 'chat').getByRole('button', { name: 'Edit alias chat' }), 'W05-13: the home row is editable').toHaveCount(1)
    await expect(page.locator('main.content'), 'never the other tenant').not.toContainText(SENTINEL.B)
  })

  test('gateway models E13-2: "+ New alias" sends enabled explicitly — false included; credentials is the only tenant-admin sensitivity', async ({ page }) => {
    const session = await open(page)
    await page.getByRole('button', { name: '+ New alias' }).click()
    const editor = page.getByRole('dialog', { name: 'New model alias' })
    await expect(editor).toBeVisible()
    const sens = editor.getByRole('group', { name: 'sensitivity' })
    await expect(sens.getByRole('radio', { name: 'credentials' })).toBeChecked()
    for (const v of ['personal', 'internal', 'public']) await expect(sens.getByRole('radio', { name: v }), `W05-13: ${v} below credentials is server-admin only`).toBeDisabled()
    await editor.getByLabel('name', { exact: true }).fill('team-chat')
    await editor.getByLabel('kind', { exact: true }).selectOption('chat')
    await editor.getByLabel('role', { exact: true }).fill('chat')
    const enabled = editor.getByRole('checkbox', { name: 'enabled' })
    await expect(enabled, 'W05-13: enabled is an explicit control').toBeVisible()
    // review C5 S2 #3: a new alias starts disabled — enabling is a deliberate tick
    await expect(enabled, 'fail-closed default: unchecked').not.toBeChecked()
    await enabled.check()
    await enabled.uncheck()
    await editor.getByRole('button', { name: 'Create alias' }).click()
    await expect(editor).toHaveCount(0)
    const sent = calls(session, 'model-alias-create')
    expect(sent.map((b) => b.data), 'W05-13 gate: enabled:false is on the wire').toEqual([{ name: 'team-chat', kind: 'chat', role: 'chat', enabled: false, sensitivity: 'credentials' }])
    await expect(rowOf(page, 'home', 'team-chat'), 'the new alias lands in the home scope').toHaveCount(1)
  })

  test('gateway models E13-3: edit keeps name and kind read-only and never sends them; the id rides top-level', async ({ page }) => {
    const session = await open(page)
    await rowOf(page, 'home', 'chat').getByRole('button', { name: 'Edit alias chat' }).click()
    const edit = page.getByRole('dialog', { name: 'Edit model alias' })
    await expect(edit.getByLabel('name', { exact: true }), 'W05-13 gate: name read-only').toBeDisabled()
    await expect(edit.getByLabel('kind', { exact: true }), 'W05-13 gate: kind read-only').toBeDisabled()
    await edit.getByLabel('description', { exact: true }).fill('home chat for the CI')
    await edit.getByRole('button', { name: 'Save' }).click()
    await expect(edit).toHaveCount(0)
    expect(calls(session, 'model-alias-update'), 'W05-13 gate: id top-level, only the change in data').toEqual([
      { action: 'model-alias-update', id: aliasId('home', 'chat'), data: { description: 'home chat for the CI' } },
    ])
  })

  test('gateway models E13-4: a server-admin lowers below credentials only after the confirm; the request carries the flag', async ({ page }) => {
    const session = await open(page, { role: 'server-admin' })
    await page.getByRole('button', { name: '+ New alias' }).click()
    const editor = page.getByRole('dialog', { name: 'New model alias' })
    await editor.getByLabel('name', { exact: true }).fill('open-chat')
    await editor.getByLabel('kind', { exact: true }).selectOption('chat')
    await editor.getByLabel('role', { exact: true }).fill('chat')
    await editor.getByRole('group', { name: 'sensitivity' }).getByRole('radio', { name: 'public' }).check()
    await editor.getByRole('button', { name: 'Create alias' }).click()
    const confirm = page.getByRole('dialog', { name: 'Lower sensitivity' })
    await expect(confirm, 'W05-13 gate: lowering asks first').toBeVisible()
    await expect(confirm).toContainText('foreign prompts may then reach backends with trust below full')
    expect(calls(session, 'model-alias-create'), 'nothing sent before the confirm').toEqual([])
    await confirm.getByRole('button', { name: 'Cancel' }).click()
    expect(calls(session, 'model-alias-create'), 'nothing sent after a cancel').toEqual([])
    await editor.getByRole('button', { name: 'Create alias' }).click()
    await page.getByRole('dialog', { name: 'Lower sensitivity' }).getByRole('button', { name: 'Lower sensitivity', exact: true }).click()
    const sent = calls(session, 'model-alias-create').map((b) => b.data as Record<string, unknown>)
    expect(sent.length, 'one create after the confirm').toBe(1)
    expect(sent[0]?.confirm_sensitivity_downgrade, 'W05-13 gate: the confirmed request carries the flag').toBe(true)
    expect(sent[0]?.sensitivity).toBe('public')
  })

  test('gateway models E13-5: "Suggest from pool" drafts only disabled aliases named after the pool primaries', async ({ page }) => {
    const session = await open(page, { role: 'server-admin' })
    await page.getByRole('button', { name: 'Suggest from pool' }).click()
    await expect(rowOf(page, '_global', POOL_PRIMARY.chat ?? ''), 'W05-13: the chat draft lands').toHaveCount(1)
    const previews = calls(session, 'model-alias-preview').map((b) => b.data)
    expect(previews, 'one role preview per pool role').toEqual([{ role: 'chat' }, { role: 'embed' }, { role: 'rerank' }])
    const creates = calls(session, 'model-alias-create').map((b) => b.data as Record<string, unknown>)
    expect(creates.map((c) => c.name).sort(), 'drafts for the roles with a primary (rerank has none)').toEqual([POOL_PRIMARY.chat, POOL_PRIMARY.embed].sort())
    for (const c of creates) expect(c.enabled, `W05-13 gate: draft ${String(c.name)} is enabled:false`).toBe(false)
    await expect(rowOf(page, '_global', POOL_PRIMARY.chat ?? '').locator('[data-alias-enabled]'), 'the draft shows disabled').toHaveText('disabled')
  })

  test('gateway models E13-6: a tenant-admin gets neither the pool suggestion nor the preview', async ({ page }) => {
    const session = await open(page)
    await expect(rowOf(page, 'home', 'chat').getByRole('button', { name: 'Edit alias chat' }), 'positive control: the RC-D page is mounted').toHaveCount(1)
    await expect(page.getByRole('button', { name: 'Suggest from pool' })).toHaveCount(0)
    await expect(page.getByRole('button', { name: /Preview chain/ })).toHaveCount(0)
    expect(calls(session, 'model-alias-preview'), 'never fired').toEqual([])
  })

  test('gateway models E13-7: the server-admin chain preview runs through a picked consumer and marks external rows', async ({ page }) => {
    // RC-D S2 (W19): the C7 contract makes the consumer mandatory — the S1 test
    // clicked the preview and expected a chain at once; now a consumer is picked
    // first, the request carries the alias NAME and the consumer id.
    const session = await open(page, { role: 'server-admin' })
    await rowOf(page, '_global', 'chat').getByRole('button', { name: 'Preview chain of chat' }).click()
    const panel = page.locator('[data-testid="alias-preview"]')
    await expect(panel, 'W05-13: the preview panel opens').toBeVisible()
    expect(calls(session, 'model-alias-preview'), 'no preview before a consumer is picked').toEqual([])
    const picker = panel.getByLabel('consumer', { exact: true })
    // nightly-embed takes public prompts: the external row stays in the chain
    await picker.selectOption({ label: 'nightly-embed (acme)' })
    await panel.getByRole('button', { name: 'Run preview' }).click()
    await expect(panel, 'W05-13: the chain renders').toContainText('spark-chat')
    await expect(panel.locator('[data-locality="external"]'), 'external rows are marked').toContainText('openrouter')
    expect(calls(session, 'model-alias-preview').map((b) => b.data), 'C7 request shape: alias by name + consumer').toEqual([
      { alias: 'chat', consumer_id: '0190000000007000800000000000c0a4' },
    ])
    // ci-chat sits at credentials: the external row moves to `excluded` with its reason
    await picker.selectOption({ label: 'ci-chat (acme)' })
    await panel.getByRole('button', { name: 'Run preview' }).click()
    await expect(panel.locator('[data-testid="preview-excluded"]'), 'excluded rows carry the reason').toContainText('openrouter')
    await expect(panel.locator('[data-testid="preview-chain"] [data-locality="external"]'), 'no external row in this chain').toHaveCount(0)
  })

  test('gateway models E13-8: a 422 fields entry lands at its field', async ({ page }) => {
    await open(page, {}, async (p) => {
      await p.route('**/api/manage', async (route: Route) => {
        let action: unknown
        try {
          action = (route.request().postDataJSON() as { action?: unknown } | null)?.action
        } catch {
          action = undefined
        }
        if (action === 'model-alias-create') {
          return route.fulfill({ status: 422, json: { success: false, error: 'validation failed', fields: [{ field: 'role', message: 'role "nope" is in no backend row' }] } })
        }
        return route.fallback()
      })
    })
    await page.getByRole('button', { name: '+ New alias' }).click()
    const editor = page.getByRole('dialog', { name: 'New model alias' })
    await editor.getByLabel('name', { exact: true }).fill('x-chat')
    await editor.getByLabel('kind', { exact: true }).selectOption('chat')
    await editor.getByLabel('role', { exact: true }).fill('nope')
    await editor.getByRole('button', { name: 'Create alias' }).click()
    await expect(editor.getByLabel('role', { exact: true }), 'W05-13: the field is marked invalid').toHaveAttribute('aria-invalid', 'true')
    await expect(editor, 'the message stands at the dialog').toContainText('role "nope" is in no backend row')
    await expect(editor, 'the editor stays open').toBeVisible()
  })
})
