import { test, expect, type Page } from '@playwright/test'
import { colorToArray } from 'sigma/utils'
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

    // 401 error band from the /auth/login mock — the real handler's uniform
    // sessionError string (auth_session.go, R3/R4).
    await expect(page.getByRole('alert')).toContainText('authentication failed')
    // NEVER the shell: no .shell, no rail, still the login screen.
    await expect(page.locator('.shell')).toHaveCount(0)
    await expect(page.locator('nav.rail')).toHaveCount(0)
    // R4 invariant: the raw key touches NO client storage — neither on
    // failure nor ever (login exchanges it for httpOnly cookies).
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

    // Overview-Meta-Nodes sind categoryColor() → seit U02-W2 ein Hex-Wert
    // (#rrggbb), nicht mehr hsl(): sigmas WebGL-Parser (colorToArray) kollabiert
    // hsl()-Strings auf [0,0,0,·] — schwarz. Der frühere „misread"-Vermerk
    // (der damalige Schwarz-Augenschein sei eine Fehldeutung der kleinen Punkte
    // gewesen) wird hiermit EXPLIZIT umgekehrt: der Augenschein war KORREKT, das
    // Attribut-FORMAT war der Defekt (W21 — geprüfter Augenschein trägt, bequeme
    // Umdeutung nicht). Assertion daher auf der Parse-Ebene: das Node-color-Attr
    // liegt im Hex-Format und parst zu einer NICHT-schwarzen Farbe.
    const nodeColor = await readNodeColor(page)
    expect(nodeColor, 'Node-Farbe ist Hex (#rrggbb), nicht hsl()').toMatch(/^#[0-9a-f]{6}$/)
    const [nr, ng, nb] = colorToArray(nodeColor)
    expect([nr, ng, nb], `Node-Farbe ${nodeColor} parst zu nicht-schwarz`).not.toEqual([0, 0, 0])

    await page.getByRole('radio', { name: 'Light theme' }).click()
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'light')

    // The re-color flows through the THEME_CHANGE_EVENT → palette $state → the
    // OverviewMap $effect (setSetting + refresh); poll past Svelte/Sigma flush.
    await expect.poll(() => readPalette(page), { timeout: 5000 }).not.toBe(dark)
    expect(await readPalette(page), 'light edge token after toggle').toContain('#767b91')
  })

  // G4c (U02-W3, design 02-graph-darkmode §4.6): Boot-Smoke des theme-festen
  // Hover-Renderers. Nach dem Mount ist `defaultDrawNodeHover` die eigene
  // Factory-Closure (makeDrawNodeHover) — eine Funktion, KEIN undefined. Bewusst
  // KEINE Function.prototype.toString().includes('#FFF')-Quelltext-Inspektion:
  // die wäre gegen sigma-/Minifier-Drift brittle. Die Regressions-Garantie
  // (Fill=hoverBg + Stroke=hoverStroke) trägt allein der Unit-Gate G4b
  // (node-hover.test.ts, Recording-Context). Erreicht wird hier der OverviewMap-
  // Mount (der __ctxGraph exponiert); GraphView verdrahtet identisch (Konstruktor
  // + Theme-$effect, makeDrawNodeHover(palette)).
  test('defaultDrawNodeHover ist die eigene Factory-Closure (nicht sigmas #FFF-Default)', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/graph')
    await waitForShell(page)
    await page.waitForFunction(() => '__ctxGraph' in window, null, { timeout: 10_000 })
    const hoverType = await page.evaluate(() => {
      const g = (window as unknown as { __ctxGraph?: { renderer: { getSetting(k: string): unknown } } }).__ctxGraph
      return typeof g?.renderer.getSetting('defaultDrawNodeHover')
    })
    expect(hoverType, 'defaultDrawNodeHover ist eine Funktion (theme-fester Drawer gesetzt)').toBe('function')
  })

  // G5 (U02-W4, design 02-graph-darkmode §7 + §4.6): der Ego-Canvas-Host bezieht
  // seinen Hintergrund aus DERSELBEN Quelle wie die Kontrast-Gates (--graph-bg),
  // nicht mehr aus --surface-0. Beide Tokens sind heute wertgleich (#0b0b0f/
  // #e9eaf0), aber alle Gates (G1a–G1c, G4d) rechnen gegen --graph-bg — läge der
  // echte Host auf --surface-0, ließe eine künftige Token-Divergenz die Gates
  // grün, während der sichtbare Kontrast bricht. Override-Probe: --graph-bg auf
  // einen Sentinel-Wert setzen und die computed background-color des GraphView-
  // Hosts lesen; sie MUSS dem Sentinel folgen.
  //
  // Der Selektor ist bewusst auf `.viewport .canvas` verankert (NICHT bloß
  // `.canvas`): die OverviewMap folgt --graph-bg schon vor dieser Welle — säße
  // das Gate auf ihr, wäre es fail-open. Nur GraphView liegt in `.viewport`
  // (GraphPage {#if focus !== null}); die OverviewMap ({:else}) ist bei aktivem
  // Fokus gar nicht gemountet. Erreicht wird der Ego-Host über den Deep-Link
  // ?focus=<uuid> → GraphPage.onMount → setFocus → fetchEgo (egoFixture).
  test('Ego-Canvas-Host bezieht die Hintergrundfarbe aus --graph-bg (nicht --surface-0)', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/graph?focus=550e8400-e29b-41d4-a716-446655440001')
    await waitForShell(page)

    // Der GraphView-Host mountet erst, wenn setFocus/fetchEgo aufgelöst hat.
    const host = page.locator('.viewport .canvas')
    await host.waitFor({ state: 'attached', timeout: 10_000 })

    const bg = await host.evaluate((el) => {
      document.documentElement.style.setProperty('--graph-bg', 'rgb(1, 2, 3)')
      return getComputedStyle(el).backgroundColor
    })
    expect(bg, 'Host folgt --graph-bg (Kontrast-Gate-Quelle), nicht --surface-0').toBe('rgb(1, 2, 3)')
  })
})
