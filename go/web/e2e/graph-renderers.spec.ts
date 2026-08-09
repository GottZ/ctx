import { test, expect, type Page } from '@playwright/test'
import { gotoArea, seedSession, trackPageErrors } from './fixtures'

// RV1 — Renderer-Pluralismus: vier Engines (sigma/cosmos/deck/three) über
// derselben graphology-Instanz, umschaltbar per Meta-Row-Select. Diese Spec
// ist der LAUFZEIT-Beweis der Nicht-Default-Renderer (svelte-check deckt nur
// die Typ-Ebene): jeder Switch muss (a) einen lebenden Canvas mounten,
// (b) fehlerfrei bleiben (pageerror-Tracking — WebGL-/GL-Init-Fehler landen
// dort), (c) seine Capability-Affordanz zeigen. Pixel-Asserts bleiben beim
// @visual-Gate (Default sigma) — hier zählt „lebt und stirbt nicht".
//
// STAGE-ZUGANG wie graph-search-open (W2-Immunität): über den Overview-Pick,
// nie über ?focus=.

const NODE2 = '550e8400-e29b-41d4-a716-446655440002' // Ego-Nachbar aus dem Fixture-Korpus

interface CtxGraph {
  renderer: { emit(e: string, p: unknown): unknown }
  graph: { hasNode(id: string): boolean }
}

async function enterFocusViaOverview(page: Page): Promise<void> {
  await gotoArea(page, '/graph')
  await page.waitForFunction(
    () => {
      const g = (window as unknown as { __ctxGraph?: CtxGraph }).__ctxGraph
      return !!g && g.graph.hasNode('0')
    },
    undefined,
    { timeout: 10_000 },
  )
  await page.evaluate(() => {
    ;(window as unknown as { __ctxGraph: CtxGraph }).__ctxGraph.renderer.emit('clickNode', { node: '0' })
  })
  await page.locator('.wm-root').waitFor({ state: 'attached', timeout: 10_000 })
  await page.waitForFunction(
    (n) => {
      const g = (window as unknown as { __ctxGraph?: CtxGraph }).__ctxGraph
      return !!g && g.graph.hasNode(n)
    },
    NODE2,
    { timeout: 10_000 },
  )
}

const select = (page: Page) => page.locator('select.renderer-select')
const labelsBtn = (page: Page) => page.locator('button', { hasText: 'labels' })

/** Renderer umschalten und warten, bis sein Canvas im Viewport lebt. Der
 *  Chunk lädt lazy (dynamic import) — der Canvas ist das Mounted-Signal. */
async function switchRenderer(page: Page, id: string): Promise<void> {
  await select(page).selectOption(id)
  await expect(page.locator('.viewport canvas').first()).toBeVisible({ timeout: 15_000 })
}

test.describe('graph renderer pluralism (RV1)', () => {
  test('meta-row select carries all four engines, sigma is default', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await enterFocusViaOverview(page)
    await expect(select(page)).toHaveValue('sigma')
    const options = await select(page).locator('option').allTextContents()
    expect(options).toEqual(['sigma', 'cosmos', 'deck', 'three'])
    // sigma: labels-Toggle aktiv (caps.labels), kein 3D-Toggle.
    await expect(labelsBtn(page)).toBeEnabled()
    await expect(page.locator('button', { hasText: '3D' })).toHaveCount(0)
  })

  test('cosmos mounts its GL canvas and stays error-free', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    const errors = trackPageErrors(page)
    await enterFocusViaOverview(page)
    await switchRenderer(page, 'cosmos')
    // caps.labels=false → der Toggle deaktiviert sich (Capability-Schiene).
    await expect(labelsBtn(page)).toBeDisabled()
    // Ein Simulations-Frame Zeit lassen — GL-Init-Fehler feuern asynchron.
    await page.waitForTimeout(500)
    expect(errors).toEqual([])
  })

  test('deck mounts, offers the 3D orbit toggle and survives flipping it', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    const errors = trackPageErrors(page)
    await enterFocusViaOverview(page)
    await switchRenderer(page, 'deck')
    const btn = page.locator('button', { hasText: '3D' })
    await expect(btn).toBeVisible()
    await btn.click() // Orthographic → Orbit: View-Klasse + ViewState-Reset
    await expect(btn).toHaveAttribute('aria-pressed', 'true')
    await btn.click() // und zurück — Orbit-State darf nicht in 2D lecken
    await expect(btn).toHaveAttribute('aria-pressed', 'false')
    await page.waitForTimeout(500)
    expect(errors).toEqual([])
  })

  test('three mounts with the semantic z-axis select (hop | time | flat)', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    const errors = trackPageErrors(page)
    await enterFocusViaOverview(page)
    await switchRenderer(page, 'three')
    const zmode = page.locator('select.zmode')
    await expect(zmode).toHaveValue('hop')
    // Jeder Z-Modus ist ein reiner Positions-Nachzug — kein Remount, kein Fehler.
    await zmode.selectOption('time')
    await zmode.selectOption('flat')
    await page.waitForTimeout(500)
    expect(errors).toEqual([])
  })

  test('switch back to sigma restores the e2e hook; choice persists', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    const errors = trackPageErrors(page)
    await enterFocusViaOverview(page)
    await switchRenderer(page, 'three')
    await switchRenderer(page, 'sigma')
    // {#key}-Remount: der sigma-Renderer registriert __ctxGraph frisch.
    await page.waitForFunction(
      (n) => {
        const g = (window as unknown as { __ctxGraph?: CtxGraph }).__ctxGraph
        return !!g && g.graph.hasNode(n)
      },
      NODE2,
      { timeout: 10_000 },
    )
    // Persistenz: die Wahl überlebt den Seitenbesuch (loadRendererPref).
    expect(await page.evaluate(() => localStorage.getItem('ctx-graph-renderer'))).toBe('sigma')
    expect(errors).toEqual([])
  })
})
