// /gateway forms of RC-D C5 (design/05 §4.5 Feld-Tier-UX, §4.6 RotateDialog /
// key row, §5 B-6/B-20) against the stateful mock world in fixtures.ts
// (GatewayWorld.update with the field tiers of model_consumers.go,
// GatewayWorld.rotate with grace semantics, ROTATE_MARKER = the plaintext):
//   W05-07  E07-1 tenant-admin: lower sensitivities disabled "server-admin only"
//           E07-2 without mintModelKeys: widening controls disabled, a wider
//                 CIDR prefix never reaches the wire
//           E07-3 a ctx-peer row has no edit control below server-admin
//           E07-4 a 403 lands at the dialog, never as success
//           E07-5 only the changed fields ride the wire
//   W05-08  E08-1 "Leak — revoke now" preselected ⇒ grace 0; reveal-once of the
//                 new key (second reveal-once path): exactly once, gone after ack
//           E08-2 planned change ⇒ grace from the select; in `rotating` only the
//                 predecessor carries "End grace period now"
//           E08-3 "End grace period now" revokes the predecessor's OWN key_id
//           E08-4 no rotate control without self-service / on a peer row
//   W05-11  E11-1 lane radios == [urgent, bulk]; tenant-admin: urgent disabled on bulk
//           E11-2 server-admin raises the lane ⇒ {admission_lane:'urgent'}
// RC-D S1: red against the stubs (no Edit/Rotate control on the page yet).

import { AxeBuilder } from '@axe-core/playwright'
import { expect, test, type Page } from '@playwright/test'
import { AXE_TAGS } from './contract/axe'
import { FIXED_NOW } from './contract/contract'
import { MODEL_ALIASES, ROTATE_MARKER, SENTINEL, gotoArea, modelApiCapabilities, seedSession, type SeededSession, type SeedOptions } from './fixtures'

const CI_CHAT = '0190000000007000800000000000c0a1'
const PEER_B = '0190000000007000800000000000c0a2'

const aliasId = (scope: string, name: string): string => {
  const a = MODEL_ALIASES.find((x) => x.scope === scope && x.name === name)
  if (!a) throw new Error(`no alias ${scope}/${name}`)
  return a.id
}

async function open(page: Page, opts: Partial<SeedOptions> = {}): Promise<SeededSession> {
  await page.clock.setFixedTime(FIXED_NOW)
  const session = await seedSession(page, { role: 'tenant-admin', theme: 'dark', capabilities: modelApiCapabilities(true, true), ...opts })
  await gotoArea(page, '/gateway')
  await expect(page.locator('main.content tr[data-consumer-row]').first()).toBeVisible()
  return session
}

const row = (page: Page, label: string) => page.locator('main.content tr[data-consumer-row]').filter({ hasText: label })

function bodies(session: SeededSession, action: string): Record<string, unknown>[] {
  return session.calls
    .filter((c) => c.action === action)
    .map((c) => ((c.body as { data?: Record<string, unknown> } | undefined)?.data ?? {}) as Record<string, unknown>)
}

async function openEdit(page: Page, label: string) {
  const btn = row(page, label).getByRole('button', { name: `Edit policy of ${label}` })
  await expect(btn, `W05-07: "Edit policy of ${label}" is offered`).toBeVisible()
  await btn.click()
  const dialog = page.getByRole('dialog', { name: new RegExp(`Edit policy`) })
  await expect(dialog, 'W05-07: the edit dialog opens').toBeVisible()
  return dialog
}

async function openRotate(page: Page, label: string) {
  const btn = row(page, label).getByRole('button', { name: `Rotate key of ${label}` })
  await expect(btn, `W05-08: "Rotate key of ${label}" is offered`).toBeVisible()
  await btn.click()
  const dialog = page.getByRole('dialog', { name: /Rotate key/ })
  await expect(dialog, 'W05-08: the rotate dialog opens').toBeVisible()
  return dialog
}

/** Elements / text nodes carrying `needle` (precedent gateway-mint.spec.ts markerCount). */
async function markerCount(page: Page, needle: string): Promise<number> {
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

async function storageClean(page: Page, needle: string): Promise<void> {
  const dump = await page.evaluate(() => JSON.stringify({ s: { ...sessionStorage }, l: { ...localStorage }, u: location.href, t: document.title }))
  expect(dump, 'B-6: no plaintext in sessionStorage / localStorage / URL / title').not.toContain(needle)
}

test.describe('gateway forms W05-07 policy edit (RC-D C5)', () => {
  test('gateway forms E07-1: a tenant-admin sees the lower sensitivities disabled ("server-admin only")', async ({ page }) => {
    await open(page)
    const dialog = await openEdit(page, 'ci-chat')
    const sens = dialog.getByRole('group', { name: 'prompt sensitivity' })
    await expect(sens.getByRole('radio', { name: 'credentials' }), 'current value checked').toBeChecked()
    await expect(sens.getByRole('radio', { name: 'credentials' })).toBeEnabled()
    for (const v of ['personal', 'internal', 'public']) await expect(sens.getByRole('radio', { name: v }), `W05-07 gate: ${v} disabled for a tenant-admin`).toBeDisabled()
    await expect(sens).toContainText('server-admin only')
  })

  test('gateway forms E07-1: a server-admin may pick every sensitivity', async ({ page }) => {
    await open(page, { role: 'server-admin' })
    const dialog = await openEdit(page, 'ci-chat')
    const sens = dialog.getByRole('group', { name: 'prompt sensitivity' })
    for (const v of ['credentials', 'personal', 'internal', 'public']) await expect(sens.getByRole('radio', { name: v }), `server-admin: ${v}`).toBeEnabled()
  })

  test('gateway forms E07-2: without mintModelKeys widening is disabled and a wider CIDR prefix never reaches the wire', async ({ page }) => {
    const session = await open(page, { capabilities: modelApiCapabilities(true, false) })
    const dialog = await openEdit(page, 'ci-chat')
    const models = dialog.getByRole('group', { name: 'models' })
    const pick = (name: string) => models.locator(`input[data-alias-id="${aliasId('_global', name)}"]`)
    await expect(pick('chat'), 'positive control: the granted model is checked').toBeChecked()
    await expect(pick('chat'), 'positive control: removing it (narrowing) stays possible').toBeEnabled()
    for (const m of ['draft', 'embed']) await expect(pick(m), `W05-07 gate: adding ${m} needs self-service`).toBeDisabled()
    await expect(dialog.getByRole('checkbox', { name: /all offered models/ }), 'W05-07 gate: "all offered" needs self-service').toBeDisabled()
    await expect(dialog.getByRole('checkbox', { name: 'any source' }), 'W05-07 gate: dropping the allowlist needs self-service').toBeDisabled()
    await expect(dialog.getByLabel('scope', { exact: true }), 'W05-07 gate: the scope needs self-service').toBeDisabled()
    const cidrs = dialog.getByLabel('network allowlist')
    await cidrs.fill('192.168.0.0/16')
    await expect(dialog.getByRole('button', { name: 'Save' }), 'W05-07 gate: a wider allowlist cannot be saved').toBeDisabled()
    await expect(dialog).toContainText('server-admin')
    // narrowing stays possible: a sub-prefix of the stored 10.0.0.0/8
    await cidrs.fill('10.1.0.0/16')
    await dialog.getByRole('button', { name: 'Save' }).click()
    await expect(dialog).toHaveCount(0)
    expect(bodies(session, 'model-consumer-update'), 'only the narrowing request was sent').toEqual([{ id: CI_CHAT, allowed_cidrs: ['10.1.0.0/16'] }])
  })

  test('gateway forms E07-3: a ctx-peer row carries no edit control below server-admin; a server-admin edits it', async ({ page }) => {
    await open(page)
    await expect(row(page, 'ci-chat').getByRole('button', { name: 'Edit policy of ci-chat' }), 'positive control: the app row is editable').toHaveCount(1)
    await expect(row(page, 'ctx-peer-b').getByRole('button', { name: /edit/i }), 'W05-07 gate: no edit control on the peer row').toHaveCount(0)
  })

  test('gateway forms E07-3: a server-admin has the edit control on the peer row', async ({ page }) => {
    await open(page, { role: 'server-admin' })
    await expect(row(page, 'ctx-peer-b').getByRole('button', { name: 'Edit policy of ctx-peer-b' }), 'server-admin edits peers').toHaveCount(1)
  })

  test('gateway forms E07-4: a 403 lands at the dialog, never as success', async ({ page }) => {
    await open(page, { faults: [{ action: 'model-consumer-update', status: 403, error: 'admin key required' }] })
    const dialog = await openEdit(page, 'ci-chat')
    await dialog.getByLabel('description').fill('changed')
    await dialog.getByRole('button', { name: 'Save' }).click()
    await expect(dialog.getByRole('alert'), 'W05-07 gate: the 403 is shown in the dialog').toContainText('admin key required')
    await expect(dialog, 'the dialog stays open').toBeVisible()
    await expect(page.locator('[data-testid="gateway-toast"]'), 'no success toast').toHaveCount(0)
  })

  test('gateway forms E07-5: only the changed fields ride the wire; the list shows the server answer', async ({ page }) => {
    const session = await open(page)
    const dialog = await openEdit(page, 'ci-chat')
    await dialog.getByLabel('label', { exact: true }).fill('ci-chat-renamed')
    await dialog.getByRole('button', { name: 'Save' }).click()
    await expect(dialog).toHaveCount(0)
    expect(bodies(session, 'model-consumer-update'), 'W05-07: exactly {id, label}').toEqual([{ id: CI_CHAT, label: 'ci-chat-renamed' }])
    await expect(row(page, 'ci-chat-renamed')).toHaveCount(1)
  })
})

test.describe('gateway forms W05-08 rotation (RC-D C5)', () => {
  test('gateway forms E08-1: "Leak — revoke now" is preselected and sends grace 0; the new key shows exactly once', async ({ page }) => {
    // review C5 S2 #2: the console is a leak channel too (RV-H survived without this)
    const consoleLines: string[] = []
    page.on('console', (m) => consoleLines.push(m.text()))
    const session = await open(page)
    const dialog = await openRotate(page, 'ci-chat')
    const reason = dialog.getByRole('group', { name: 'reason' })
    await expect(reason.getByRole('radio', { name: /Leak/ }), 'W05-08 gate: the leak reason is preselected').toBeChecked()
    await expect(dialog.getByLabel('grace period'), 'no grace window in the leak case').toHaveCount(0)
    await dialog.getByRole('button', { name: 'Rotate key' }).click()
    expect(bodies(session, 'model-key-rotate'), 'W05-08 gate: grace 0 on the wire').toEqual([{ consumer_id: CI_CHAT, grace: 0 }])
    const reveal = dialog.getByRole('textbox', { name: 'new api key — plaintext, shown once' })
    await expect(reveal, 'second reveal-once path: the new plaintext').toHaveValue(ROTATE_MARKER)
    expect(await markerCount(page, ROTATE_MARKER), 'B-6: exactly once in the DOM').toBe(1)
    await dialog.getByRole('button', { name: "I've stored it — done" }).click()
    await expect(dialog).toHaveCount(0)
    expect(await markerCount(page, ROTATE_MARKER), 'B-6: gone after the ack').toBe(0)
    expect(await markerCount(page, 'TESTKEY'), 'B-6: no fragment either').toBe(0)
    await storageClean(page, 'TESTKEY')
    expect(consoleLines.filter((l) => l.includes('TESTKEY')), 'B-6: the console never carries the plaintext').toEqual([])
    // leak: the predecessor is gone at once, the row holds the new key only
    await expect(row(page, 'ci-chat').locator('[data-key-status]')).toHaveText(['active'])
    await expect(row(page, 'ci-chat')).toContainText('ctxm_…sist')
    await expect(row(page, 'ci-chat')).not.toContainText('ctxm_…a9Fq')
    // re-opening the dialog shows no plaintext
    await openRotate(page, 'ci-chat')
    expect(await markerCount(page, 'TESTKEY'), 'B-6: a re-opened dialog holds nothing').toBe(0)
  })

  test('gateway forms E08-2: a planned change sends the chosen grace; then only the predecessor carries "End grace period now"', async ({ page }) => {
    // the A sentinel consumer: one live key ctxm_…Xk4d (expires 2026-07-05, live at the mock clock)
    const session = await open(page)
    const dialog = await openRotate(page, SENTINEL.A)
    await dialog.getByRole('group', { name: 'reason' }).getByRole('radio', { name: /Planned change/ }).check()
    const grace = dialog.getByLabel('grace period')
    await expect(grace.locator('option'), 'W05-08: 1 h / 24 h / 7 d').toHaveText(['1 hour', '24 hours', '7 days'])
    await expect(grace, 'preselected = rotation_grace_s (24 h)').toHaveValue('86400')
    await dialog.getByRole('button', { name: 'Rotate key' }).click()
    expect(bodies(session, 'model-key-rotate'), 'W05-08: the planned grace').toEqual([{ consumer_id: '0190000000007000800000000000c0a3', grace: 86_400 }])
    await dialog.getByRole('button', { name: "I've stored it — done" }).click()
    const r = row(page, SENTINEL.A)
    await expect(r.locator('[data-consumer-status]')).toHaveText('rotating')
    await expect(r.getByRole('button', { name: /End grace period/ }), 'W05-08 gate: exactly one end-grace control').toHaveCount(1)
    await expect(r.getByRole('button', { name: 'End grace period of key ctxm_…Xk4d' }), 'W05-08 gate: on the predecessor').toHaveCount(1)
    await expect(r.getByRole('button', { name: 'End grace period of key ctxm_…sist' }), 'never on the successor').toHaveCount(0)
  })

  test('gateway forms E08-3: "End grace period now" revokes the predecessor by its OWN key_id; the successor stays', async ({ page }) => {
    const session = await open(page)
    const r = row(page, 'ctx-peer-b')
    const end = r.getByRole('button', { name: 'End grace period of key ctxm_…mP2c' })
    await expect(end, 'W05-08: the predecessor of the rotating peer row carries the control (revoking is always allowed)').toHaveCount(1)
    await expect(r.getByRole('button', { name: 'End grace period of key ctxm_…Qe7z' }), 'never on the successor').toHaveCount(0)
    await end.click()
    const confirm = page.getByRole('dialog', { name: 'End grace period' })
    await expect(confirm).toContainText('The new key stays valid')
    // RC-D S2 (W19): a danger ConfirmDialog arms on the first click and commits
    // on the second (S1 decision 3: danger, no typed characters) — the S1 test
    // clicked once; the second click is the commit, nothing is sent before it.
    await confirm.getByRole('button', { name: 'End grace period', exact: true }).click()
    expect(bodies(session, 'model-key-revoke'), 'the first click only arms').toEqual([])
    await confirm.getByRole('button', { name: 'End grace period', exact: true }).click()
    await expect(confirm).toHaveCount(0)
    const revokes = bodies(session, 'model-key-revoke')
    expect(revokes, 'W05-08 gate: the predecessor key id').toEqual([{ key_id: `${PEER_B}k1` }])
    expect(revokes[0]?.key_id, 'never the consumer id').not.toBe(PEER_B)
    expect(revokes[0]?.key_id, 'never the successor').not.toBe(`${PEER_B}k0`)
    await expect(r.locator('[data-consumer-status]')).toHaveText('active')
    await expect(r).toContainText('ctxm_…Qe7z')
    await expect(r).not.toContainText('ctxm_…mP2c')
  })

  test('gateway forms E08-4: no rotate control without self-service, none on a peer row below server-admin', async ({ page }) => {
    await open(page, { capabilities: modelApiCapabilities(true, false) })
    await expect(row(page, 'ci-chat').getByRole('button', { name: 'Edit policy of ci-chat' }), 'positive control: the page carries the RC-D controls').toHaveCount(1)
    await expect(page.locator('main.content').getByRole('button', { name: /Rotate key/ }), 'W05-08: no rotation without self-service').toHaveCount(0)
  })

  test('gateway forms E08-4: with self-service the peer row still has no rotate control', async ({ page }) => {
    await open(page)
    await expect(row(page, 'ci-chat').getByRole('button', { name: 'Rotate key of ci-chat' }), 'positive control').toHaveCount(1)
    await expect(row(page, 'ctx-peer-b').getByRole('button', { name: /Rotate key/ }), 'W05-08: never on a peer below server-admin').toHaveCount(0)
  })
})

test.describe('gateway forms W05-11 lane (RC-D C5)', () => {
  test('gateway forms E11-1: the lane radios are exactly urgent and bulk; a tenant-admin cannot raise bulk', async ({ page }) => {
    await open(page)
    const dialog = await openEdit(page, 'ci-chat')
    const lane = dialog.getByRole('group', { name: 'admission lane' })
    await expect(lane.getByRole('radio'), 'W05-11 gate: exactly the closed vocabulary').toHaveCount(2)
    await expect(lane.getByRole('radio', { name: 'bulk' }), 'the current lane').toBeChecked()
    await expect(lane.getByRole('radio', { name: 'urgent' }), 'W05-11 gate: urgent disabled on a bulk consumer').toBeDisabled()
    await expect(lane).toContainText('server-admin only')
    for (const v of ['interactive', 'background', 'external', 'external_bulk']) await expect(lane.getByRole('radio', { name: v }), `V-03-a: ${v} not representable`).toHaveCount(0)
  })

  test('gateway forms E11-2: a server-admin raises the lane — {admission_lane:"urgent"} on the wire', async ({ page }) => {
    const session = await open(page, { role: 'server-admin' })
    const dialog = await openEdit(page, 'ci-chat')
    const lane = dialog.getByRole('group', { name: 'admission lane' })
    await expect(lane.getByRole('radio', { name: 'urgent' })).toBeEnabled()
    await lane.getByRole('radio', { name: 'urgent' }).check()
    await dialog.getByRole('button', { name: 'Save' }).click()
    await expect(dialog).toHaveCount(0)
    expect(bodies(session, 'model-consumer-update'), 'W05-11: the raise').toEqual([{ id: CI_CHAT, admission_lane: 'urgent' }])
  })
})

// Fremd-Review C5 S2 #6: the contract runner scans only the default state; the
// new dialogs get their own axe gate (precedent gateway-mint.spec.ts a11y) —
// a NEW surface carries no ledger debt (design 06 §3.3).
test.describe('gateway forms a11y (RC-D C5)', () => {
  for (const theme of ['dark', 'light'] as const) {
    test(`gateway forms a11y (${theme}): edit dialog with a refusal, rotate dialog, reveal and the end-grace confirm carry no axe violation`, async ({ page }) => {
      await page.emulateMedia({ reducedMotion: 'reduce' })
      await open(page, { theme, faults: [{ action: 'model-consumer-update', status: 403, error: 'admin key required' }] })
      const scan = async (what: string) => {
        await page.waitForTimeout(400)
        const res = await new AxeBuilder({ page }).withTags([...AXE_TAGS]).analyze()
        expect(res.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).join(', ')}`), what).toEqual([])
      }
      const edit = await openEdit(page, 'ci-chat')
      await expect(edit.getByLabel('label', { exact: true }), 'focus lands in the dialog').toBeFocused()
      await scan('edit dialog')
      await edit.getByLabel('description').fill('axe-probe')
      await edit.getByRole('button', { name: 'Save' }).click()
      await expect(edit.getByRole('alert')).toContainText('admin key required')
      await scan('edit dialog with the refusal')
      await edit.getByRole('button', { name: 'Cancel' }).click()
      const rot = await openRotate(page, 'ci-chat')
      await rot.getByRole('group', { name: 'reason' }).getByRole('radio', { name: /Planned change/ }).check()
      await scan('rotate dialog (planned)')
      await rot.getByRole('button', { name: 'Rotate key' }).click()
      await expect(rot.getByRole('textbox', { name: 'new api key — plaintext, shown once' })).toBeVisible()
      await scan('rotate reveal')
      await rot.getByRole('button', { name: "I've stored it — done" }).click()
      // ctx-peer-b is rotating in the mock world (ci-chat's predecessor a9Fq is past its expiry at the mock clock)
      await row(page, 'ctx-peer-b').getByRole('button', { name: 'End grace period of key ctxm_…mP2c' }).click()
      const confirm = page.getByRole('dialog', { name: 'End grace period' })
      await confirm.getByRole('button', { name: 'End grace period', exact: true }).click()
      await scan('end-grace confirm (armed)')
    })
  }
})
