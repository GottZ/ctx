import { test, expect, type Page } from '@playwright/test'
import { seedSession, gotoArea } from './fixtures'

// U03-W1 — Such-Pick zentriert UND öffnet das Node-Fenster (design 03-§4.1/§4.2/
// §5.2). Der Such-Pick hat ein ECHTES DOM-Target (Playwright klickt den Treffer-
// Button), stärker als der __ctxGraph-Emit-Seam.
//
// STAGE-ZUGANG (design 03-§7, W2-Immunität): diese Spec erreicht die Focus-Stage
// AUSSCHLIESSLICH über den Overview-Stage-Pick (Overview-Node → setFocus → Focus-
// Stage), NIEMALS über ?focus=. W2 ändert später die ?focus-Semantik global (jede
// ?focus-Navigation bringt danach ein Auto-Fenster mit) — eine ?focus-eintretende
// Spec erbte das und verfälschte die Dialog-Counts. Über Overview→Pick bleibt die
// Spec W2-immun. Der in (b)/(e) auf Fokus-Rückgabe geprüfte Node ist NIE der
// Landungs-Node (kein Dedup-Re-Pick).

const LANDING = '550e8400-e29b-41d4-a716-446655440001' // overview cluster-0 reprId = ego focus
const NODE2 = '550e8400-e29b-41d4-a716-446655440002' // search hit 'API Spec' — ego neighbour
const NODE3 = '550e8400-e29b-41d4-a716-446655440003' // search hit 'Retrieval Findings' — ego neighbour

interface CtxGraph {
  renderer: {
    emit(e: string, p: unknown): unknown
    getNodeDisplayData(id: string): { highlighted?: boolean } | undefined
  }
  graph: { hasNode(id: string): boolean }
}

/** Enter the FOCUS stage via the OVERVIEW-stage pick (NOT ?focus=). In the
 *  overview stage __ctxGraph is the OverviewMap renderer whose node keys are the
 *  cluster ordinals ('0'..); cluster '0' carries reprId = LANDING. Re-dispatching
 *  its clickNode fires onpick(reprId) → setFocus(LANDING) → focus stage. Overview-
 *  pick stays byte-gleich (no window opens on landing, design 03-§4.3). */
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
  // Focus stage mounts: .wm-root overlay is a sibling of GraphView (proves the
  // OverviewMap is gone), and __ctxGraph is now the ego renderer with the neighbours.
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

/** Submit the FTS form (the mock ignores the query text, always returns the
 *  tenant-A corpus) and leave the result list rendered for the caller to pick. */
async function submitSearch(page: Page): Promise<void> {
  const input = page.locator('input[type="search"]')
  await input.fill('block')
  await input.press('Enter')
  await expect(page.locator('.results button').first()).toBeVisible()
}

/** Full search pick: submit, then CLICK the hit whose row carries `title`. */
async function searchAndPick(page: Page, title: string): Promise<void> {
  await submitSearch(page)
  await page.locator('.results button', { hasText: title }).click()
}

async function activeInfo(page: Page): Promise<{ tag: string; type: string; cls: string }> {
  return page.evaluate(() => ({
    tag: document.activeElement?.tagName ?? '',
    type: (document.activeElement as HTMLInputElement | null)?.type ?? '',
    cls: document.activeElement?.className ?? '',
  }))
}

test.describe('graph search-open (U03-W1)', () => {
  // (a) Such-Pick öffnet das Fenster + der Node ist Sigma-gehighlightet.
  test('Such-Pick öffnet das Node-Fenster und highlightet den Node', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await enterFocusViaOverview(page)

    await searchAndPick(page, 'API Spec') // NODE2
    await expect(page.getByRole('dialog')).toHaveCount(1)

    // Highlight aus Sigma-DISPLAY-Data (reducer-only), farbunabhängig (design 03-§9.3).
    await expect
      .poll(
        () =>
          page.evaluate((n) => {
            const g = (window as unknown as { __ctxGraph: CtxGraph }).__ctxGraph
            return g.renderer.getNodeDisplayData(n)?.highlighted ?? false
          }, NODE2),
        { timeout: 5000 },
      )
      .toBe(true)
  })

  // (b) Fenster schließen (Escape) → Fokus zurück auf das Such-Input. Geprüfter
  //     Node (NODE2) ≠ Landungs-Node (LANDING) → kein Dedup-Re-Pick.
  test('Escape auf dem Such-Fenster gibt den Fokus ans Such-Input zurück', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await enterFocusViaOverview(page)

    await searchAndPick(page, 'API Spec') // NODE2
    await expect(page.getByRole('dialog')).toHaveCount(1)
    // Autofokus zieht den Fokus in den frisch gemounteten Fenster-Container.
    await expect.poll(() => page.evaluate(() => document.activeElement?.getAttribute('role') ?? '')).toBe('dialog')

    await page.keyboard.press('Escape')
    await expect(page.getByRole('dialog')).toHaveCount(0)

    const active = await activeInfo(page)
    expect(active.tag).toBe('INPUT')
    expect(active.type).toBe('search')
  })

  // (c) §5.2-Mechanismus: Such-Pick AUS DER OVERVIEW-STAGE (Fenster öffnet im
  //     selben Zug wie der Stage-Wechsel) auf 800×600 → Dialog-BoundingBox
  //     VOLLSTÄNDIG im Viewport. Keine harten Spawn-Maße gepinnt (Achse 04 ändert
  //     die noch). Die Negativ-Probe (Pre-Open-Messung aus → off-screen) läuft im
  //     Build-Schritt gegen den auskommentierten setSurface-Call.
  test('Overview-Stage-Pick platziert das Fenster vollständig im Viewport (§5.2)', async ({ page }) => {
    await page.setViewportSize({ width: 800, height: 600 })
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/graph')
    // Bewusst NICHT erst in die Focus-Stage eintreten: der Pick aus der Overview-
    // Stage ist der neue §5.2-Pfad (Fenster + Stage-Wechsel in einem Zug).
    await searchAndPick(page, 'API Spec') // NODE2

    const dialog = page.getByRole('dialog')
    await expect(dialog).toHaveCount(1)
    const box = await dialog.boundingBox()
    const vp = page.viewportSize()
    expect(box).toBeTruthy()
    expect(vp).toBeTruthy()
    // spawnRect clampt die LOGISCHE Rect (content-box) voll in die gemessene
    // Surface; die 2px-Fensterrahmen (border-box) liegen minimal außerhalb —
    // ein content-box-vs-border-Effekt, unabhängig von dieser Achse. Kleine
    // Toleranz dafür. Der Trennabstand zur Negativ-Probe (Pre-Open-Messung aus →
    // Store-Default 1280 → rechte Kante ~935px, >130px off-screen) bleibt riesig,
    // die Toleranz verwässert das Gate also nicht. KEINE harten Spawn-Maße gepinnt.
    const BORDER_TOL = 4
    expect(box!.x).toBeGreaterThanOrEqual(-BORDER_TOL)
    expect(box!.y).toBeGreaterThanOrEqual(-BORDER_TOL)
    expect(box!.x + box!.width).toBeLessThanOrEqual(vp!.width + BORDER_TOL)
    expect(box!.y + box!.height).toBeLessThanOrEqual(vp!.height + BORDER_TOL)
  })

  // (d) Re-Pick desselben Treffers → genau EIN Fenster (store.open-Dedup nach UUID).
  test('Re-Pick desselben Treffers erzeugt kein zweites Fenster (Dedup)', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await enterFocusViaOverview(page)

    await searchAndPick(page, 'API Spec') // NODE2
    await expect(page.getByRole('dialog')).toHaveCount(1)
    // In W1 leert der Pick die Liste → für den Re-Pick neu submitten.
    await searchAndPick(page, 'API Spec')
    await expect(page.getByRole('dialog')).toHaveCount(1)
  })

  // (e) triggerEl-Update: Node per Canvas-Klick-Seam öffnen (triggerEl=null),
  //     denselben Node aus der Suche re-picken (aktualisiert triggerEl auf das
  //     Such-Input), dann Fenster schließen → Fokus auf das Such-Input.
  //     Ohne den Store-Change fiele der Fokus auf .viewport (rot).
  test('Re-Pick eines Canvas-geöffneten Fensters zieht das Close-Fokus-Ziel aufs Such-Input', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await enterFocusViaOverview(page)

    // Canvas-Klick-Seam (GraphView-Renderer): store.open(NODE3, null).
    await page.evaluate((id) => {
      ;(window as unknown as { __ctxGraph: CtxGraph }).__ctxGraph.renderer.emit('clickNode', { node: id })
    }, NODE3)
    await expect(page.getByRole('dialog')).toHaveCount(1)

    await searchAndPick(page, 'Retrieval Findings') // NODE3 → dedup + triggerEl-Update
    await expect(page.getByRole('dialog')).toHaveCount(1)

    // Der Re-Pick holt das sichtbare Fenster nicht in den DOM-Fokus (kein Remount
    // → kein Autofokus, design 03-§4.1). Für den Escape-Close den Fenster-
    // Container über die Titelleiste (linker Grip, kein Button) fokussieren.
    await page.locator('.window .titlebar').first().click({ position: { x: 8, y: 8 } })
    await page.keyboard.press('Escape')
    await expect(page.getByRole('dialog')).toHaveCount(0)

    const active = await activeInfo(page)
    expect(active.tag, `close-focus fiel auf .${active.cls || active.tag}`).toBe('INPUT')
    expect(active.type).toBe('search')
  })

  // (f) Tastatur-Loop (design 03-§4.5-Zweig B): Pick per Enter → Fokus im Fenster
  //     → Escape → Fokus zurück auf das Such-Input. (Der gleichzeitige Tastatur-
  //     Multi-Stack ist explizit KEIN Ziel dieser Achse.)
  test('Tastatur-Loop: Enter-Pick → Fokus im Fenster → Escape → Fokus zurück im Such-Input', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await enterFocusViaOverview(page)

    await submitSearch(page)
    // locator.press fokussiert den Button und löst per Enter die native Aktivierung
    // (= Klick = pick) aus — ein echter Tastatur-Pick.
    await page.locator('.results button', { hasText: 'API Spec' }).press('Enter') // NODE2
    await expect(page.getByRole('dialog')).toHaveCount(1)
    await expect.poll(() => page.evaluate(() => document.activeElement?.getAttribute('role') ?? '')).toBe('dialog')

    await page.keyboard.press('Escape')
    await expect(page.getByRole('dialog')).toHaveCount(0)
    const active = await activeInfo(page)
    expect(active.tag).toBe('INPUT')
    expect(active.type).toBe('search')
  })

  // (g, §5.5) XSS-Invariante des gerenderten Block-Contents: der Such-Pick ist der
  //     Haupt-Renderweg fremden Contents. Ein Payload im Content wird als Text-Node
  //     gerendert (<pre>{content}</pre>, nie {@html}) → sichtbar, nie ausgeführt.
  test('Gepickter Block-Content rendert als Text und wird nie ausgeführt (XSS)', async ({ page }) => {
    const XSS = '<img src=x onerror="window.__xssFired=1"><' + 'script>window.__xssFired=1</' + 'script> plain-tail-marker'
    const dialogs: string[] = []
    page.on('dialog', (d) => {
      dialogs.push(d.message())
      void d.dismiss()
    })

    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    // manage-get liefert den XSS-Payload als Content; alle anderen Actions fallen
    // per fallback() an die seedSession-Mocks durch (last route first gewinnt).
    await page.route('**/api/manage', async (route) => {
      const body = route.request().postDataJSON() as { action?: string } | null
      if (body?.action === 'get') {
        return route.fulfill({
          status: 200,
          json: {
            success: true,
            block: {
              id: NODE2,
              category: 'design',
              tags: ['xss'],
              title: 'XSS Probe',
              content: XSS,
              scope: 'home',
              sensitivity: 'internal',
              created_at: '2026-06-01T08:00:00Z',
              updated_at: '2026-06-28T10:00:00Z',
            },
          },
        })
      }
      return route.fallback()
    })

    await enterFocusViaOverview(page)
    await searchAndPick(page, 'API Spec') // NODE2

    const pre = page.locator('pre.content')
    await expect(pre).toBeVisible()
    await expect(pre).toContainText('plain-tail-marker') // Payload als Text sichtbar
    await expect(pre).toContainText('onerror') // die Markup-Zeichen sind Text, kein DOM
    expect(await page.evaluate(() => (window as unknown as { __xssFired?: number }).__xssFired)).toBeUndefined()
    expect(dialogs).toHaveLength(0)
  })
})
