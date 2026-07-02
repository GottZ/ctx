import { test, expect } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors } from './fixtures'

// Eyeball dumps live under the gitignored e2e/.results/ tree since PV3 killed
// e2e/__shots__ (committed __screenshots__ baselines replaced it); these
// remaining manual dumps migrate to PageContracts in wave PV7.
const SHOTS = 'e2e/.results/shots'

// Empty-state surfaces (N8) — `empty: true` makes search + graph-overview return
// nothing; chat sessions are always empty in the mocks.
test.describe('empty states (N8)', () => {
  test('blocks empty → "store your first block" CTA (writable key)', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'server-admin', theme: 'dark', empty: true })
    await gotoArea(page, '/blocks')
    await expect(page.locator('.empty-state', { hasText: 'No blocks yet' })).toBeVisible()
    await expect(page.getByRole('button', { name: 'Store your first block' })).toBeVisible()
    await page.screenshot({ path: `${SHOTS}/blocks-empty.png` })
    expect(errors, errors.join('\n')).toEqual([])
  })

  test('graph empty → "no clusters yet"', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark', empty: true })
    await gotoArea(page, '/graph')
    await expect(page.locator('.empty-state', { hasText: 'No clusters yet' })).toBeVisible()
  })

  test('chat empty → onboarding CTA', async ({ page }) => {
    await seedSession(page, { role: 'member', theme: 'dark' })
    await gotoArea(page, '/chat')
    await expect(page.locator('.empty-state', { hasText: 'No conversations yet' })).toBeVisible()
    await expect(page.getByRole('link', { name: /Browse the corpus/ })).toBeVisible()
  })
})

// Corpus maintenance (A7) — server-admin audit/classify dry-run trigger + poll.
test.describe('corpus maintenance (A7)', () => {
  test('server-admin dry-runs the sensitivity audit (trigger → poll → finished)', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/admin')
    const corpus = page.locator('section[aria-label="corpus maintenance"]')
    await expect(corpus).toBeVisible()
    await expect(corpus).toContainText('sensitivity audit')
    await expect(corpus).toContainText('credentials classify')

    await page.getByRole('button', { name: 'Run audit' }).click()
    // The poll must actually converge: while running the button reads "audit
    // running…"; on completion the done BADGE appears — matched with exact text
    // so it is not the ambient "finished —" label (which is always present).
    await expect(corpus.getByText('finished', { exact: true })).toBeVisible({ timeout: 15_000 })
    await expect(page.getByRole('button', { name: 'Run audit' })).toBeEnabled()
    await page.screenshot({ path: `${SHOTS}/admin-corpus.png` })
    expect(errors, errors.join('\n')).toEqual([])
  })
})
