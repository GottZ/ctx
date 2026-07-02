import { test, expect, type Page } from '@playwright/test'
import { captureShot } from './contract/capture'
import { seedSession, gotoArea, trackPageErrors, type Role } from './fixtures'

// Visual + structural smoke that closes the HANDOVER §8 debt: the 23 shipped
// shell/layout/theme/graph waves were code-green (vitest+svelte-check+build) but
// the actual RENDERING was never seen in a browser. Structural asserts run
// everywhere; the visual baselines (@visual, wave PV3) replaced the former
// e2e/__shots__ eyeball dumps with committed toHaveScreenshot references —
// compared ONLY inside the digest-pinned toolchain container (e2e-visual.sh).

// Fixture "now": e2e/fixtures.ts pins as_of/finished_at to 2026-06-29T12:00:00Z;
// fixing the page clock 5 s after that keeps every relative-time render
// ("5s ago" on /status) byte-stable across runs (design 06 §4.3, time rule).
const FIXED_NOW = new Date('2026-06-29T12:00:05Z')

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

        expect(errors, `uncaught page errors on ${area.path}:\n${errors.join('\n')}`).toEqual([])
      })
    }
  }
})

// ---------------------------------------------------------------------------
// 1v. Visual baselines (@visual, wave PV3) — per-area toHaveScreenshot against
//     the committed e2e/__screenshots__ references. Gated: these tests only
//     run with CTX_E2E_CONTAINER=1 (playwright.config.ts grepInvert), i.e.
//     inside the digest-pinned toolchain container via `bash e2e-visual.sh`.
//     The graph canvas (sigma/ForceAtlas2, not seed-stable) carries
//     data-e2e-mask and legitimately exceeds the 40 % mask budget → declared
//     override (design 06 §4.3).
// ---------------------------------------------------------------------------
test.describe('visual baselines', () => {
  for (const theme of THEMES) {
    for (const area of AREAS) {
      test(`${area.name} ${theme} visual baseline`, { tag: '@visual' }, async ({ page }) => {
        await page.clock.setFixedTime(FIXED_NOW)
        await seedSession(page, { role: 'server-admin', theme })
        await gotoArea(page, area.path)
        await waitForShell(page)
        await expect(page.locator('html')).toHaveAttribute('data-theme', theme)
        await expect(page.locator('main.content')).toHaveAttribute('data-mode', area.mode)

        // Let the area settle (sigma boot / lazy chunk) before capture;
        // fonts are awaited inside captureShot (stabilize).
        await page.waitForTimeout(600)
        await captureShot(
          page,
          `${area.name}--default--${theme}--desktop.png`,
          area.name === 'graph'
            ? {
                maskBudgetOverride: {
                  reason:
                    'full-bleed sigma canvas (ForceAtlas2 layout is not seed-stable); graph SEMANTICS stay asserted via the __ctxGraph hook below',
                  issue: '.project/plan-workflow-ui-2026-07-02/design/06-playwright-validation.md §4.3 (interim ref until Achse-02 issues exist)',
                },
              }
            : {},
        )
      })
    }
  }

  // Rail element baselines replace the former rail-<role>.png dumps.
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

// 1b. Blocks master list actually populates from the search fixture (resolves
// an earlier smoke finding where a dev-server-flake left the region blank).
test('blocks master list populates from search', async ({ page }) => {
  await seedSession(page, { role: 'server-admin', theme: 'dark' })
  await gotoArea(page, '/blocks')
  await expect(page.locator('main.content')).toContainText('Core Architecture')
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
    expect(dark, 'dark edge token wired into Sigma').toContain('#3a3a52')

    // Overview meta-nodes are categoryColor() = hsl(hue, nodeSat%, nodeLum%) — a
    // light, theme-aware fill (dark tokens 70%/68%), NOT the black that a misread
    // of the small dots once suggested. Mechanically resolves that smoke finding.
    expect(await readNodeColor(page), 'overview node uses dark node sat/lum').toMatch(/70% 68%/)

    await page.getByRole('radio', { name: 'Light theme' }).click()
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'light')

    // The re-color flows through the THEME_CHANGE_EVENT → palette $state → the
    // OverviewMap $effect (setSetting + refresh); poll past Svelte/Sigma flush.
    await expect.poll(() => readPalette(page), { timeout: 5000 }).not.toBe(dark)
    expect(await readPalette(page), 'light edge token after toggle').toContain('#c4c8d4')
  })
})
