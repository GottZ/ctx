import { test, expect, type Page } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors, type Role } from './fixtures'

// Visual + structural smoke that closes the HANDOVER §8 debt: the 23 shipped
// shell/layout/theme/graph waves were code-green (vitest+svelte-check+build) but
// the actual RENDERING was never seen in a browser. Each test asserts the shell
// structure AND drops a screenshot under e2e/__shots__/ for eyeball review.

const SHOTS = 'e2e/__shots__'

/** Wait for the authenticated shell (App.svelte → AppShell, past restore). */
async function waitForShell(page: Page): Promise<void> {
  await expect(page.locator('.shell')).toBeVisible()
  await expect(page.locator('nav.rail[aria-label="Primary"]')).toBeVisible()
}

const AREAS = [
  { name: 'status', path: '/status', mode: 'reading' },
  { name: 'graph', path: '/graph', mode: 'canvas' },
  { name: 'blocks', path: '/blocks', mode: 'split' },
  { name: 'chat', path: '/chat', mode: 'thread' },
  { name: 'settings', path: '/settings', mode: 'reading' },
] as const

const THEMES = ['dark', 'light'] as const

// ---------------------------------------------------------------------------
// 1. Per-area layout modes × theme (S3–S7, TH3) — server-admin sees every area.
// ---------------------------------------------------------------------------
test.describe('shell + per-area layout modes', () => {
  for (const theme of THEMES) {
    for (const area of AREAS) {
      test(`${area.name} renders in ${theme}`, async ({ page }) => {
        const errors = trackPageErrors(page)
        await seedSession(page, { role: 'server-admin', theme })
        await gotoArea(page, area.path)
        await waitForShell(page)

        // Theme actually painted (theme-boot.js read our localStorage pref).
        await expect(page.locator('html')).toHaveAttribute('data-theme', theme)
        // The content region carries the per-area layout mode (areaMode map).
        await expect(page.locator('main.content')).toHaveAttribute('data-mode', area.mode)

        // Let the area settle (sigma canvas / lazy chunk) before the shot.
        await page.waitForTimeout(600)
        await page.screenshot({ path: `${SHOTS}/${area.name}-${theme}.png`, fullPage: false })

        expect(errors, `uncaught page errors on ${area.path}:\n${errors.join('\n')}`).toEqual([])
      })
    }
  }
})

// ---------------------------------------------------------------------------
// 2. Role-adaptive nav rail (E3 / N1–N3) — the rail's gated sections per tier.
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

      await page.locator('nav.rail').screenshot({ path: `${SHOTS}/rail-${role}.png` })
      expect(errors, errors.join('\n')).toEqual([])
    })
  }
})

// ---------------------------------------------------------------------------
// 3. Theme toggle (TH4) — the 3-segment control flips <html data-theme> live.
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
// 4. Graph palette wiring (G1/G2) — the test-hook (window.__ctxGraph) lets us
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

  test('label/edge colours track the theme', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/graph')
    await waitForShell(page)

    // OverviewMap mounts Sigma + exposes __ctxGraph once the cluster map renders.
    await page.waitForFunction(() => '__ctxGraph' in window, null, { timeout: 10_000 })
    const dark = await readPalette(page)
    expect(dark, 'dark edge token wired into Sigma').toContain('#3a3a52')

    await page.getByRole('radio', { name: 'Light theme' }).click()
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'light')

    // The re-color flows through the THEME_CHANGE_EVENT → palette $state → the
    // OverviewMap $effect (setSetting + refresh); poll past Svelte/Sigma flush.
    await expect.poll(() => readPalette(page), { timeout: 5000 }).not.toBe(dark)
    expect(await readPalette(page), 'light edge token after toggle').toContain('#c4c8d4')
  })
})
