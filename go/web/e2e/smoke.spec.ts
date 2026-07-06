import { test, expect, type Page } from '@playwright/test'
import { captureShot } from './contract/capture'
import { FIXED_NOW, definePageContract } from './contract/contract'
import { contracts } from './contract/registry'
import { KEY, seedSession, gotoArea, trackPageErrors, type Role } from './fixtures'

// PageContract host (design 06 §4.1, wave PV4). The five area contracts
// (e2e/contract/registry.ts) are EXECUTED here — every registry entry runs,
// the loop is the guarantee (a contract cannot exist without its tests).
//
// This file deliberately keeps the name smoke.spec.ts: snapshotPathTemplate
// keys baselines by {testFileBaseName}, so the 13 PV3-committed baselines
// under e2e/__screenshots__/smoke.spec/ stay byte-valid — a rename would be a
// silent mass [baseline] event (design 06 §3.1/§5.5).
//
// Consolidated INTO the contracts (PV4, no duplication):
//   - "shell + per-area layout modes" (10 structural tests) → the generated
//     primary-flow tests assert shell/theme/data-mode per area; both THEMES
//     stay pixel-proven by the generated @visual pairs.
//   - "visual baselines" area loop (10 @visual) → generated from the contracts
//     with IDENTICAL baseline names (<area>--default--<theme>--desktop.png).
//   - "blocks master list populates" → blocks primaryFlow (list + detail).
// Kept as FREE special tests beside the contracts (the contract is the
// minimum, not the ceiling — §4.1): rail element baselines, role-adaptive
// rail, theme toggle, graph palette hook.

for (const c of contracts) definePageContract(c)

// ---------------------------------------------------------------------------
// Login negative path (PV7 gate: falscher Key ⇒ Fehlerband, NIE Shell). The
// login CONTRACT freezes the error band visually (state 'error'); this free
// test carries the behavioural halves the visual state cannot: the shell
// never mounts, no key is persisted, and a corrected key still succeeds
// afterwards (the mask stays operable after a failure).
// ---------------------------------------------------------------------------
test.describe('login negative path (PV7)', () => {
  test('wrong key renders the error band and never the shell', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark', anonymous: true })
    await page.goto('/')
    await expect(page.locator('form.card')).toBeVisible()

    await page.getByLabel('API key').fill('wrong-key')
    await page.getByRole('button', { name: 'Sign in' }).click()

    // 401 error band from the whoami auth-header branch — the real handler string.
    await expect(page.getByRole('alert')).toContainText('invalid or revoked API key')
    // NEVER the shell: no .shell, no rail, still the login screen.
    await expect(page.locator('.shell')).toHaveCount(0)
    await expect(page.locator('nav.rail')).toHaveCount(0)
    // The rejected key is not persisted (login() persists only on success).
    expect(await page.evaluate(() => sessionStorage.getItem('ctx.api-key'))).toBeNull()

    // The mask stays operable: the correct key still signs in.
    await page.getByLabel('API key').fill(KEY)
    await page.getByRole('button', { name: 'Sign in' }).click()
    await expect(page.locator('.shell')).toBeVisible()
    expect(errors, errors.join('\n')).toEqual([])
  })
})

// ---------------------------------------------------------------------------
// Shell @768 — icon-rail band (E5, PV7). breakpoints.ts discriminates TWO
// boundaries below desktop: SM 640 (rail → drawer) and MD 1024 (rail icon-
// only). 640–1023 is a real render state neither 390 nor 1440 captures; the
// shell is page-independent, so ONE reference page (/blocks) suffices —
// gezielte weitere @768-Tests bleiben Kontrakt-Deklarationssache (E5).
// ---------------------------------------------------------------------------
test.describe('shell @768 — icon-rail band (E5)', () => {
  test.use({ viewport: { width: 768, height: 1024 } })

  test('rail renders icon-only between sm and md', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/blocks')

    // Persistent rail (NOT the mobile drawer bar) in its icon-only state.
    const rail = page.locator('nav.rail[aria-label="Primary"]')
    await expect(rail).toBeVisible()
    await expect(rail).toHaveClass(/icon/)
    await expect(page.locator('header.mobile-bar')).toHaveCount(0)
    // Icon-only: no textual nav labels; the accessible name moves to aria-label.
    const blocksLink = rail.getByRole('link', { name: 'Blocks' })
    await expect(blocksLink).toBeVisible()
    await expect(blocksLink).toHaveAttribute('aria-label', 'Blocks')
    expect(await blocksLink.innerText()).not.toContain('Blocks')
    expect(errors, errors.join('\n')).toEqual([])
  })

  test('shell @768 visual baseline', { tag: '@visual' }, async ({ page }) => {
    await page.clock.setFixedTime(FIXED_NOW)
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/blocks')
    await expect(page.locator('nav.rail[aria-label="Primary"]')).toHaveClass(/icon/)
    await page.waitForTimeout(600)
    await captureShot(page, 'shell-iconrail--default--dark--768.png')
  })
})

/** Wait for the authenticated shell (App.svelte → AppShell, past restore). */
async function waitForShell(page: Page): Promise<void> {
  await expect(page.locator('.shell')).toBeVisible()
  await expect(page.locator('nav.rail[aria-label="Primary"]')).toBeVisible()
}

// ---------------------------------------------------------------------------
// Rail element baselines (special: element-target shots, not a page contract).
// ---------------------------------------------------------------------------
test.describe('rail visual baselines', () => {
  const RAIL_ROLES: Role[] = ['member', 'tenant-admin', 'server-admin']
  for (const role of RAIL_ROLES) {
    test(`rail ${role} visual baseline`, { tag: '@visual' }, async ({ page }) => {
      await page.clock.setFixedTime(FIXED_NOW)
      await seedSession(page, { role, theme: 'dark' })
      await gotoArea(page, '/blocks')
      await waitForShell(page)
      await captureShot(page, `rail-${role}--default--dark--desktop.png`, {
        target: page.locator('nav.rail'),
      })
    })
  }
})

// ---------------------------------------------------------------------------
// Role-adaptive nav rail (E3 / N1–N3) — the rail's gated sections per tier.
//    One app, three identities: member→Corpus only, tenant-admin→+Tenant,
//    server-admin→+Server. The rail is page-independent; /blocks is shared.
// ---------------------------------------------------------------------------
test.describe('role-adaptive nav rail', () => {
  const expectations: Record<Role, { present: string[]; absent: string[] }> = {
    member: { present: ['Corpus'], absent: ['Server'] },
    'tenant-admin': { present: ['Corpus', 'Tenant'], absent: ['Server'] },
    'server-admin': { present: ['Corpus', 'Server'], absent: [] },
  }

  for (const role of Object.keys(expectations) as Role[]) {
    test(`rail for ${role}`, async ({ page }) => {
      const errors = trackPageErrors(page)
      await seedSession(page, { role, theme: 'dark' })
      await gotoArea(page, '/blocks')
      await waitForShell(page)

      for (const label of expectations[role].present) {
        await expect(page.locator(`nav.rail [role="group"][aria-label="${label}"]`)).toBeVisible()
      }
      for (const label of expectations[role].absent) {
        await expect(page.locator(`nav.rail [role="group"][aria-label="${label}"]`)).toHaveCount(0)
      }

      expect(errors, errors.join('\n')).toEqual([])
    })
  }
})

// ---------------------------------------------------------------------------
// Theme toggle (TH4) — the 3-segment control flips <html data-theme> live.
// ---------------------------------------------------------------------------
test.describe('theme toggle', () => {
  test('light/dark segments switch the painted theme', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/blocks')
    await waitForShell(page)
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark')

    await page.getByRole('radio', { name: 'Light theme' }).click()
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'light')

    await page.getByRole('radio', { name: 'Dark theme' }).click()
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark')
  })
})

// ---------------------------------------------------------------------------
// Graph palette wiring (G1/G2) — the test-hook (window.__ctxGraph) lets us
//    read Sigma settings mechanically (WebGL colours aren't in the DOM). Asserts
//    the live label/edge colours actually came from the CSS --graph-* tokens
//    and re-bake on a dark→light toggle (no remount).
// ---------------------------------------------------------------------------
test.describe('graph theme palette', () => {
  // The colours live in the WebGL canvas (not the DOM); the __ctxGraph hook
  // exposes Sigma's renderer so we can read its settings mechanically.
  const readPalette = (page: Page) =>
    page.evaluate(() => {
      const g = (window as unknown as { __ctxGraph?: { renderer: { getSetting(k: string): unknown } } }).__ctxGraph
      return JSON.stringify({ label: g?.renderer.getSetting('labelColor'), edge: g?.renderer.getSetting('defaultEdgeColor') })
    })

  const readNodeColor = (page: Page) =>
    page.evaluate(() => {
      const g = (window as unknown as { __ctxGraph?: { graph: { nodes(): string[]; getNodeAttribute(n: string, k: string): unknown } } }).__ctxGraph
      const n = g?.graph.nodes()[0]
      return n ? String(g!.graph.getNodeAttribute(n, 'color')) : ''
    })

  test('label/edge colours track the theme', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/graph')
    await waitForShell(page)

    // OverviewMap mounts Sigma + exposes __ctxGraph once the cluster map renders.
    await page.waitForFunction(() => '__ctxGraph' in window, null, { timeout: 10_000 })
    const dark = await readPalette(page)
    expect(dark, 'dark edge token wired into Sigma').toContain('#5d5f80')

    // Overview meta-nodes are categoryColor() = hsl(hue, nodeSat%, nodeLum%) — a
    // light, theme-aware fill (dark tokens 70%/68%), NOT the black that a misread
    // of the small dots once suggested. Mechanically resolves that smoke finding.
    expect(await readNodeColor(page), 'overview node uses dark node sat/lum').toMatch(/70% 68%/)

    await page.getByRole('radio', { name: 'Light theme' }).click()
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'light')

    // The re-color flows through the THEME_CHANGE_EVENT → palette $state → the
    // OverviewMap $effect (setSetting + refresh); poll past Svelte/Sigma flush.
    await expect.poll(() => readPalette(page), { timeout: 5000 }).not.toBe(dark)
    expect(await readPalette(page), 'light edge token after toggle').toContain('#767b91')
  })
})
