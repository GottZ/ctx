import { test } from '@playwright/test'
import { seedSession, gotoArea } from './fixtures'
import { runReducedMotionWalk } from './contract/reduced-motion'

// Q12 gate (design 05-§7): the reduced-motion walk proves the motion system is
// abschaltbar. Emulate prefers-reduced-motion for the whole file, then sweep two
// representative surfaces: the login mask (rise animation + input/button
// transitions, pre-shell) and a shell page (Wordmark-Puls signature + nav/button
// transitions). The global app.css guard is page-independent, so one shell page
// proves it for all — a broader per-contract sweep would add cost without adding
// coverage (the guard is a single `*` block, not per-route).
//
// ROT-Beweis (führ ihn von Hand, dann grün): kommentiere den globalen
// @media(prefers-reduced-motion)-Block in src/app.css aus → beide Tests werden
// rot, weil jede var(--dur-1)-Transition dann 120ms statt ~0ms meldet.
test.use({ reducedMotion: 'reduce' })

test.describe('reduced-motion walk (Q12)', () => {
  test('login mask disables all motion', { tag: '@a11y' }, async ({ page }, testInfo) => {
    await seedSession(page, { role: 'member', theme: 'dark', anonymous: true })
    await page.goto('/')
    await page.locator('form.card').waitFor({ state: 'visible', timeout: 10_000 })
    await runReducedMotionWalk(page, testInfo)
  })

  test('shell disables all motion incl. the wordmark signature', { tag: '@a11y' }, async ({ page }, testInfo) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/blocks')
    await runReducedMotionWalk(page, testInfo)
  })
})
