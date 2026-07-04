import { test, expect } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors } from './fixtures'

// U12 — project-provisioning wizard in /admin (design 04 §7-U12, workflow-seams.md
// §7.1/§7.2). The wizard is a GUIDED SEQUENCER over the real compound model: three
// ordered manage-actions (tenant-create → scope-create → api-key-create) for a new
// tenant, or two (scope-create → api-key-create) into an existing one. These specs
// carry the WIRE + FLOW halves the visual/aria baselines cannot: the step ORDER
// (Fixture-Sequenz-Log, not just call count), the K12 agent-key template on the
// wire, the abort/resume checkpoint, reveal-once hygiene and the 409 draft-keep.
//
// Fixture note: the scope-create fixture prefixes with the SESSION tenant slug
// (ctx.slug='acme'), not the target tenant — like admin-tenant-provision.spec.ts.
// The wire proof is that the client sends the BARE name + tenant_id and renders
// the SERVER-built scope (S1); the prefix value is the fixture's, not client-built.

const OWNER_KEY = 'ctx_sk_TESTOWNER_reveal_once_do_not_persist'
const AGENT_KEY = 'ctx_sk_TESTKEY_reveal_once_do_not_persist'
const NEW_TENANT_ID = '550e8400-e29b-41d4-a716-446655440ccc' // tenant-create fixture id
const ACME_ID = '550e8400-e29b-41d4-a716-446655440aaa' // fixture register tenant
const PLAINTEXT_LABEL = 'new api key — plaintext, shown once'

/** The ordered provisioning wire, read off the recorded manage calls. */
type Manage = { action?: string }
function provisionSeq(calls: Manage[]): (string | undefined)[] {
  const steps = ['tenant-create', 'scope-create', 'api-key-create']
  return calls.filter((c) => steps.includes(c.action ?? '')).map((c) => c.action)
}

test.describe('U12: provisioning wizard — new tenant (3 steps)', () => {
  test('drives the 3 ordered steps and mints the K12 agent key on the wire', async ({ page }) => {
    const errors = trackPageErrors(page)
    const session = await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/admin')

    await page.getByRole('button', { name: '+ Provision project' }).click()
    const dialog = page.getByRole('dialog')
    await expect(dialog).toBeVisible()

    // Entry → new-tenant flow.
    await dialog.getByRole('button', { name: 'New tenant + repo scope' }).click()

    // Step 1: the atomic tenant-create compound.
    await expect(dialog.getByLabel('wizard progress')).toHaveText('step 1 of 3')
    await dialog.getByLabel('tenant slug').fill('globex')
    await dialog.getByLabel('display name').fill('Globex Inc')
    await dialog.getByRole('button', { name: 'Create tenant' }).click()

    // Owner-key revealed once, then acknowledged.
    await expect(dialog.getByLabel(PLAINTEXT_LABEL)).toHaveValue(OWNER_KEY)
    await dialog.getByRole('button', { name: /stored it/ }).click()

    // Step 2: the repo scope (server-built, rendered — never client-built).
    await expect(dialog.getByLabel('wizard progress')).toHaveText('step 2 of 3')
    await dialog.getByLabel('repo scope name').fill('myrepo')
    await dialog.getByRole('button', { name: 'Create scope' }).click()

    // Step 3: the agent key over the server-built home scope.
    await expect(dialog.getByLabel('wizard progress')).toHaveText('step 3 of 3')
    await expect(dialog.getByLabel('agent home scope')).toHaveValue('acme:myrepo')
    await dialog.getByLabel('agent key label').fill('agent-bot')
    await dialog.getByRole('button', { name: 'Mint agent key' }).click()

    await expect(dialog.getByLabel(PLAINTEXT_LABEL)).toHaveValue(AGENT_KEY)
    await dialog.getByRole('button', { name: /stored it/ }).click()
    await dialog.getByRole('button', { name: 'Finish' }).click()
    await expect(dialog).toBeHidden()

    // WIRE ORDER (negative probe: this is toEqual on the ORDERED sequence, so a
    // permuted call order fails — it proves step ORDER, not just call count).
    expect(provisionSeq(session.calls)).toEqual(['tenant-create', 'scope-create', 'api-key-create'])

    // Per-step wire asserts: scope-create carries the BARE name + the new
    // tenant_id; api-key-create carries the K12 template (home=repo scope,
    // allowed=[], write=[]) bound to the new tenant.
    const scopeCall = session.calls.find((c) => (c as { action?: string }).action === 'scope-create') as
      | { body?: { data?: { name?: string; tenant_id?: string } } }
      | undefined
    expect(scopeCall?.body?.data).toMatchObject({ name: 'myrepo', tenant_id: NEW_TENANT_ID })

    const keyCall = session.calls.find((c) => (c as { action?: string }).action === 'api-key-create') as
      | { body?: { data?: Record<string, unknown> } }
      | undefined
    expect(keyCall?.body?.data).toMatchObject({
      home_scope: 'acme:myrepo',
      allowed_scopes: [],
      write_scopes: [],
      tenant_id: NEW_TENANT_ID,
    })

    expect(errors, errors.join('\n')).toEqual([])
  })
})

test.describe('U12: provisioning wizard — existing tenant (2 steps, §9.7)', () => {
  test('skips tenant-create: only scope-create then api-key-create on the wire', async ({ page }) => {
    const errors = trackPageErrors(page)
    const session = await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/admin')

    await page.getByRole('button', { name: '+ Provision project' }).click()
    const dialog = page.getByRole('dialog')

    // Alt-entry: pick an existing tenant → the 2-step flow.
    await dialog.getByLabel('target tenant').selectOption(ACME_ID)
    await dialog.getByRole('button', { name: 'Continue' }).click()
    await expect(dialog.getByLabel('wizard progress')).toHaveText('step 1 of 2')

    await dialog.getByLabel('repo scope name').fill('altrepo')
    await dialog.getByRole('button', { name: 'Create scope' }).click()

    await expect(dialog.getByLabel('wizard progress')).toHaveText('step 2 of 2')
    await expect(dialog.getByLabel('agent home scope')).toHaveValue('acme:altrepo')
    await dialog.getByLabel('agent key label').fill('agent-bot')
    await dialog.getByRole('button', { name: 'Mint agent key' }).click()
    await expect(dialog.getByLabel(PLAINTEXT_LABEL)).toHaveValue(AGENT_KEY)
    await dialog.getByRole('button', { name: /stored it/ }).click()
    await dialog.getByRole('button', { name: 'Finish' }).click()

    // Exactly the 2 calls, in order — NO tenant-create.
    expect(provisionSeq(session.calls)).toEqual(['scope-create', 'api-key-create'])
    const keyCall = session.calls.find((c) => (c as { action?: string }).action === 'api-key-create') as
      | { body?: { data?: { tenant_id?: string } } }
      | undefined
    expect(keyCall?.body?.data?.tenant_id).toBe(ACME_ID)

    expect(errors, errors.join('\n')).toEqual([])
  })
})

test.describe('U12: provisioning wizard — abort after step 2 resumes at step 3', () => {
  test('closing after the repo scope keeps the checkpoint; Resume shows the agent step', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/admin')

    await page.getByRole('button', { name: '+ Provision project' }).click()
    let dialog = page.getByRole('dialog')
    await dialog.getByRole('button', { name: 'New tenant + repo scope' }).click()
    await dialog.getByLabel('tenant slug').fill('globex')
    await dialog.getByLabel('display name').fill('Globex Inc')
    await dialog.getByRole('button', { name: 'Create tenant' }).click()
    await dialog.getByRole('button', { name: /stored it/ }).click()
    await dialog.getByLabel('repo scope name').fill('myrepo')
    await dialog.getByRole('button', { name: 'Create scope' }).click()
    // The repo scope now exists (stage 'key'). Abort the wizard.
    await expect(dialog.getByLabel('wizard progress')).toHaveText('step 3 of 3')
    await dialog.getByRole('button', { name: 'close' }).click()
    await expect(dialog).toBeHidden()

    // The register shows the in-progress checkpoint + a Resume affordance.
    const resume = page.getByText('A project provisioning is in progress')
    await expect(resume).toBeVisible()
    await page.getByRole('button', { name: 'Resume' }).click()

    // Resumed straight onto step 3 — the agent-key form over the built scope.
    dialog = page.getByRole('dialog')
    await expect(dialog.getByLabel('wizard progress')).toHaveText('step 3 of 3')
    await expect(dialog.getByLabel('agent home scope')).toHaveValue('acme:myrepo')
    await expect(dialog.getByRole('button', { name: 'Mint agent key' })).toBeVisible()
  })
})

test.describe('U12: provisioning wizard — reveal-once hygiene', () => {
  test('the agent key shows once; ack wipes it from the DOM and it cannot be re-fetched', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/admin')

    await page.getByRole('button', { name: '+ Provision project' }).click()
    const dialog = page.getByRole('dialog')
    await dialog.getByLabel('target tenant').selectOption(ACME_ID)
    await dialog.getByRole('button', { name: 'Continue' }).click()
    await dialog.getByLabel('repo scope name').fill('altrepo')
    await dialog.getByRole('button', { name: 'Create scope' }).click()
    await dialog.getByLabel('agent key label').fill('agent-bot')
    await dialog.getByRole('button', { name: 'Mint agent key' }).click()

    await expect(dialog.getByLabel(PLAINTEXT_LABEL)).toHaveValue(AGENT_KEY)
    await dialog.getByRole('button', { name: /stored it/ }).click()

    // Ack wiped it — the plaintext is nowhere in the DOM, and the done view has
    // no second reveal (the model never held the secret, so it is unrecoverable).
    await expect(page.getByText(AGENT_KEY)).toHaveCount(0)
    await expect(dialog.getByLabel(PLAINTEXT_LABEL)).toHaveCount(0)
    await expect(dialog.getByRole('button', { name: 'Finish' })).toBeVisible()
  })
})

test.describe('U12: provisioning wizard — scope-create 409 keeps the draft', () => {
  test('a 409 surfaces as a banner and the typed repo name is retained (U10 draft)', async ({ page }) => {
    await seedSession(page, {
      role: 'server-admin',
      theme: 'dark',
      faults: [{ action: 'scope-create', status: 409, error: 'scope already exists' }],
    })
    await gotoArea(page, '/admin')

    await page.getByRole('button', { name: '+ Provision project' }).click()
    const dialog = page.getByRole('dialog')
    await dialog.getByLabel('target tenant').selectOption(ACME_ID)
    await dialog.getByRole('button', { name: 'Continue' }).click()

    await dialog.getByLabel('repo scope name').fill('dup')
    await dialog.getByRole('button', { name: 'Create scope' }).click()

    // The 409 is rendered, the input is kept, and the wizard stays on the scope step.
    const banner = dialog.getByRole('alert')
    await expect(banner).toBeVisible()
    await expect(banner).toContainText('already exists')
    await expect(dialog.getByLabel('repo scope name')).toHaveValue('dup')
    await expect(dialog.getByRole('button', { name: 'Create scope' })).toBeVisible()
    // No agent step was reached (the scope never got built).
    await expect(dialog.getByLabel('agent home scope')).toHaveCount(0)
  })
})

test.describe('U12: provisioning wizard — accessibility structure', () => {
  test('the open wizard entry has a stable ARIA structure', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/admin')
    await page.getByRole('button', { name: '+ Provision project' }).click()
    const dialog = page.getByRole('dialog')
    await expect(dialog).toBeVisible()
    // Structure gate (platform-independent, runs on the host AND in the container);
    // the pixel baseline rides the /admin `wizard` contract state in smoke.spec.ts.
    await expect(dialog).toMatchAriaSnapshot({ name: 'provision-wizard--aria.yml' })
  })
})
