import { test, expect, type Page, type Route } from '@playwright/test'
import { categoryColor, hslToHex } from '../src/lib/graph/graph-client'
import { readGraphPalette } from '../src/lib/graph/graph-theme'
import { gotoArea, seedSession, trackPageErrors } from './fixtures'

// Kategorie-Farb-Overrides (AM-2, Web-UX U02-W6; design 02a §A4-W6). Freie
// Verhaltens-Spec NEBEN dem PageContract (settings-hues) — sie trägt die zwei
// Halbseiten, die der Contract-primaryFlow nicht abdeckt: die DELETE-Rücksetzung
// mit optimistischer Vorschau UND die RENDER-Wirkung im Graph (Node-color-Attr
// folgt dem Override-Hue, Parse-Ebene wie G3 in smoke.spec.ts).

// dark-Palette wie sie der Browser nach Theme=dark liefert; readGraphPalette()
// gibt im Node den Dark-Fallback (sat70/lum68) — wertgleich zu den Dark-Tokens
// (durch den G2-Pin in graph-theme.test.ts bestätigt).
const DARK = readGraphPalette()

/** Node-0-color-Attr aus dem __ctxGraph-Hook lesen (WebGL-Farbe ist nicht im DOM). */
const readNodeColor = (page: Page): Promise<string> =>
  page.evaluate(() => {
    const g = (window as unknown as { __ctxGraph?: { graph: { nodes(): string[]; getNodeAttribute(n: string, k: string): unknown } } })
      .__ctxGraph
    const n = g?.graph.nodes()[0]
    return n ? String(g!.graph.getNodeAttribute(n, 'color')) : ''
  })

test.describe('category-hue overrides (AM-2, U02-W6)', () => {
  test('wheel route: set → PUT + optimistische Vorschau, zurücksetzen → DELETE', async ({ page }) => {
    const errors = trackPageErrors(page)
    const session = await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/settings/hues')

    await expect(page.getByRole('heading', { name: 'kategorie-farben' })).toBeVisible()
    const content = page.locator('main.content')
    // list-categories-Fixture: design/reference/learnings → sortiert design, learnings, reference.
    const rows = content.locator('ul[aria-label="kategorien"] .cat')
    await expect(rows).toHaveCount(3)

    const design = rows.filter({ hasText: 'design' }).first()
    // Vor der Wahl trägt die Zeile den Seed-Marker.
    await expect(design.locator('.cat-meta')).toContainText('seed')

    // Kategorie wählen → Regler erscheint → Hue setzen.
    await design.locator('.row').click()
    const slider = page.getByRole('slider')
    await expect(slider).toBeVisible()
    await slider.fill('200')

    // PUT auf den Draht …
    await expect
      .poll(() => session.calls.some((c) => c.method === 'PUT' && c.path === '/api/graph/category-hues/design'))
      .toBe(true)
    // … und die optimistische Vorschau: die Zeile trägt jetzt den Override-Marker + 200°.
    await expect(design.locator('.cat-meta')).toContainText('override')
    await expect(design.locator('.cat-meta')).toContainText('200')

    // Zurücksetzen → DELETE + Marker zurück auf Seed (fällt auf den Hash-Seed).
    await design.getByRole('button', { name: 'zurücksetzen' }).click()
    await expect
      .poll(() => session.calls.some((c) => c.method === 'DELETE' && c.path === '/api/graph/category-hues/design'))
      .toBe(true)
    await expect(design.locator('.cat-meta')).toContainText('seed')

    expect(errors, errors.join('\n')).toEqual([])
  })

  test('member sieht das Banner (Fläche self-gated), kein PUT-Pfad', async ({ page }) => {
    const session = await seedSession(page, { role: 'member', theme: 'dark' })
    await gotoArea(page, '/settings/hues')
    await expect(page.getByRole('status')).toContainText('nur-lese-schlüssel')
    // Kein aussichtsloser Schreib-Request von der Member-Ansicht.
    expect(session.calls.some((c) => c.path.startsWith('/api/graph/category-hues/'))).toBe(false)
  })

  test('Graph: Node-color-Attr folgt dem gemockten Override-Hue', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    // Override-Map NACH seedSession registrieren (spätere page.route gewinnt):
    // 'design' (top-Kategorie von Overview-Cluster 0) auf Hue 200.
    await page.route('**/api/graph/category-hues', (route: Route) =>
      route.fulfill({ json: { success: true, hues: { design: 200 } } }),
    )
    await gotoArea(page, '/graph')
    await page.waitForFunction(() => '__ctxGraph' in window, null, { timeout: 10_000 })

    // Recolor-on-arrival (fire-and-forget): das Node-0-color-Attr konvergiert auf
    // die Override-Farbe = hslToHex(200, sat, lum). Parse-Ebene wie G3.
    await expect.poll(() => readNodeColor(page), { timeout: 5000 }).toBe(hslToHex(200, DARK.nodeSat, DARK.nodeLum))
  })

  test('Graph: ohne Override rendert der Hash-Seed (Regression-Anker)', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    // Default-Fixture: GET category-hues = leere Map → alles auf Seed.
    await gotoArea(page, '/graph')
    await page.waitForFunction(() => '__ctxGraph' in window, null, { timeout: 10_000 })

    // Cluster 0 top-Kategorie = 'design' → Seed-Farbe categoryColor('design', dark).
    const seed = categoryColor('design', DARK)
    await expect.poll(() => readNodeColor(page), { timeout: 5000 }).toBe(seed)
    // Und NICHT die Override-Farbe aus dem anderen Test.
    expect(seed).not.toBe(hslToHex(200, DARK.nodeSat, DARK.nodeLum))
  })
})
