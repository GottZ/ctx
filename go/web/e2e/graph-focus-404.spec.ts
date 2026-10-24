import { test, expect, type Page } from '@playwright/test'
import { seedSession, gotoArea } from './fixtures'

// Fix C — ?focus= auf einen Block, der nicht graph-sichtbar ist (archiviert,
// retrieval-excluded Typ wie system-meta, fremder Scope): Der Ego-Handler
// antwortet für alle drei Fälle mit EINEM identischen 404 (kein Existence-
// Oracle), die GraphPage kann den Grund also nicht unterscheiden und muss
// einheitlich reagieren: zur Topic-Map zurückfallen + dezenten Hinweis statt
// hartem Fehlerbanner. Ein Share-Link /graph?focus=<uuid> darf nie in einer
// Sackgasse enden.
//
// Der Server-Mock beantwortet /api/graph/ego für den MISSING-Block mit 404 —
// registriert NACH seedSession (später gewinnt, fixtures.ts:875-Muster), alle
// anderen ego-Requests laufen zur Standard-Fixture weiter.

const MISSING = '550e8400-e29b-41d4-a716-4466554400ff' // in keiner Fixture
const KNOWN = '550e8400-e29b-41d4-a716-446655440001' // ego-Fixture-Fokus

const HIT_404 = '550e8400-e29b-41d4-a716-446655440003' // Suchtreffer 'Retrieval Findings'

interface CtxGraph {
  renderer: { emit(e: string, p: unknown): unknown }
  graph: { hasNode(id: string): boolean }
}

/** /api/graph/ego antwortet für `id` mit dem Graph-404, sonst Standard-Fixture. */
async function egoNotFoundFor(page: Page, id: string): Promise<void> {
  await page.route('**/api/graph/ego**', (route) => {
    const url = new URL(route.request().url())
    if (url.searchParams.get('block') === id) {
      return route.fulfill({ status: 404, json: { success: false, error: 'Block not found' } })
    }
    return route.fallback()
  })
}

test('?focus= auf nicht graph-sichtbaren Block: Topic-Map + Notice statt Fehlerbanner', async ({ page }) => {
  await seedSession(page, { role: 'server-admin', theme: 'dark' })
  await egoNotFoundFor(page, MISSING)
  await gotoArea(page, `/graph?focus=${MISSING}`)

  // Dezente Notice sichtbar — das ist der sichtbare Teil des Fallbacks.
  const notice = page.locator('.notice')
  await notice.waitFor({ state: 'visible', timeout: 10_000 })
  await expect(notice).toContainText('not visible in the graph')

  // Kein hartes Fehlerbanner …
  await expect(page.locator('.error')).toHaveCount(0)
  // … und KEINE Focus-Stage (kein WindowManager-Fenster): die GraphPage ist
  // auf der Topic-Map gelandet (Overview-Stage).
  await expect(page.locator('.wm-root')).toHaveCount(0)

  // Die OverviewMap ist tatsächlich aktiv (Cluster-Node '0' existiert).
  await page.waitForFunction(
    () => {
      const g = (window as unknown as { __ctxGraph?: CtxGraph }).__ctxGraph
      return !!g && g.graph.hasNode('0')
    },
    undefined,
    { timeout: 10_000 },
  )
})

test('?focus= auf sichtbaren Block bleibt unverändert (kein Fallback)', async ({ page }) => {
  await seedSession(page, { role: 'server-admin', theme: 'dark' })
  await gotoArea(page, `/graph?focus=${KNOWN}`)

  // Focus-Stage erreicht: das Fenster-Overlay (.wm-root) mountet.
  await page.locator('.wm-root').waitFor({ state: 'attached', timeout: 10_000 })
  await expect(page.locator('.notice')).toHaveCount(0)
  await expect(page.locator('.error')).toHaveCount(0)
})

// PR-#48-Review: Die SearchBox sucht ohne Typfilter, liefert also auch nicht
// graph-sichtbare Treffer. Ein 404 beim Such-Pick aus einer bestehenden
// Ego-Ansicht darf weder den Fokus noch die offenen Fenster wegwerfen — nur
// die Notice erscheint. Eintritt über den Overview-Pick (graph-search-open-Muster).
test('Such-Pick mit 404 aus bestehender Ego-Ansicht behält Fokus und Fenster', async ({ page }) => {
  await seedSession(page, { role: 'server-admin', theme: 'dark' })
  await egoNotFoundFor(page, HIT_404)
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

  // Erster Pick (sichtbar) öffnet ein Fenster — das muss den 404 überleben.
  const input = page.locator('input[type="search"]')
  await input.fill('block')
  await input.press('Enter')
  await page.locator('.results button', { hasText: 'API Spec' }).click()
  await expect(page.getByRole('dialog')).toHaveCount(1)

  // Zweiter Pick trifft den 404.
  await input.fill('block')
  await input.press('Enter')
  await page.locator('.results button', { hasText: 'Retrieval Findings' }).click()

  const notice = page.locator('.notice')
  await notice.waitFor({ state: 'visible', timeout: 10_000 })
  await expect(notice).toContainText('keeping the current view')
  await expect(page.locator('.error')).toHaveCount(0)
  // Focus-Stage und Fenster bestehen weiter, ?focus zeigt noch auf den alten Fokus.
  await expect(page.locator('.wm-root')).toHaveCount(1)
  await expect(page.getByRole('dialog')).toHaveCount(1)
  expect(new URL(page.url()).searchParams.get('focus')).not.toBe(HIT_404)
  expect(new URL(page.url()).searchParams.get('focus')).not.toBeNull()
})
