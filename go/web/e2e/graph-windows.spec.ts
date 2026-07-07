import { test, expect, type Locator, type Page } from '@playwright/test'
import { seedSession, gotoArea } from './fixtures'

// G5b — floating-window manager over the Sigma canvas (design 07-§Wellen G5b).
// Desktop, production `vite preview`, VITE_E2E=1 (keeps the __ctxGraph hook).
//
// OPEN-SEAM (the critical trap, design 07): the Sigma node has NO DOM target, so
// Playwright cannot click it. Worse, `__ctxGraph` is set by TWO sources — the
// GraphView (focus stage) AND the OverviewMap (overview stage); in the overview,
// emit('clickNode') NAVIGATES (onpick→setFocus) instead of opening a window.
// Therefore every case MUST first enter the FOCUS stage (?focus=<ego-node>) so
// __ctxGraph holds the GraphView renderer and OverviewMap is unmounted, THEN open
// via a Sigma event re-dispatch: emit('clickNode',{node}) → onnodeclick →
// store.open(id, null). Highlight is read from Sigma DISPLAY-DATA
// (renderer.getNodeDisplayData(id).highlighted), NOT a graphology attribute
// (highlighted is reducer-only → graph.getNodeAttribute would be undefined).

const FOCUS = '550e8400-e29b-41d4-a716-446655440001' // egoFixture focus node
const NODE2 = '550e8400-e29b-41d4-a716-446655440002' // ego neighbour → window
const NODE3 = '550e8400-e29b-41d4-a716-446655440003' // ego neighbour → window

interface CtxGraph {
  renderer: {
    emit(e: string, p: unknown): unknown
    getNodeDisplayData(id: string): { highlighted?: boolean } | undefined
    getCamera(): { getState(): unknown }
  }
  graph: { hasNode(id: string): boolean }
}

/** Step 1 of the open-seam: enter the focus stage and wait until __ctxGraph is
 *  the GraphView (ego) renderer — the .wm-root overlay only mounts in the focus
 *  stage (sibling of GraphView), so its presence proves OverviewMap is gone. */
async function enterFocusStage(page: Page): Promise<void> {
  await gotoArea(page, `/graph?focus=${FOCUS}`)
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

/** Step 2: re-dispatch the Sigma clickNode event → onnodeclick → store.open. */
async function openWindow(page: Page, node: string): Promise<void> {
  await page.evaluate((id) => {
    ;(window as unknown as { __ctxGraph: CtxGraph }).__ctxGraph.renderer.emit('clickNode', { node: id })
  }, node)
}

/** Pointer-capture-safe drag of a window's titlebar grip (left side, clear of
 *  the min/close buttons). Real pointer events so setPointerCapture engages. */
async function dragTitlebar(page: Page, dialog: Locator, dx: number, dy: number): Promise<void> {
  const box = await dialog.locator('.titlebar').boundingBox()
  if (!box) throw new Error('titlebar has no box')
  const sx = box.x + 12
  const sy = box.y + box.height / 2
  await page.mouse.move(sx, sy)
  await page.mouse.down()
  await page.mouse.move(sx + dx, sy + dy, { steps: 8 })
  await page.mouse.up()
}

test.describe('graph floating windows (G5b)', () => {
  test('node-open spawns a visible window; two windows open at once', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await enterFocusStage(page)

    await openWindow(page, NODE2)
    await expect(page.getByRole('dialog')).toHaveCount(1)
    await openWindow(page, NODE3)
    await expect(page.getByRole('dialog')).toHaveCount(2)
    await expect(page.getByRole('dialog').first()).toBeVisible()
  })

  test('drag the titlebar moves the window (clampPos keeps it grabbable)', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await enterFocusStage(page)
    await openWindow(page, NODE2)

    const win = page.getByRole('dialog').first()
    const before = await win.boundingBox()
    await dragTitlebar(page, win, 110, 70)
    const after = await win.boundingBox()
    expect(before && after).toBeTruthy()
    // Moved a meaningful amount and never clamped off-canvas (still has a box).
    expect(Math.abs(after!.x - before!.x) + Math.abs(after!.y - before!.y)).toBeGreaterThan(40)
  })

  test('resizing wide keeps the prose body within --measure-prose', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await enterFocusStage(page)
    await openWindow(page, NODE2)

    const win = page.getByRole('dialog').first()
    // Wait for the lazy content (<pre.content>) to load from the manage-get mock.
    await expect(win.locator('pre.content')).toBeVisible()

    const grip = await win.locator('.resize').boundingBox()
    if (!grip) throw new Error('resize grip has no box')
    await page.mouse.move(grip.x + 4, grip.y + 4)
    await page.mouse.down()
    await page.mouse.move(grip.x + 700, grip.y + 220, { steps: 8 })
    await page.mouse.up()

    const winBox = await win.boundingBox()
    const contentBox = await win.locator('pre.content').boundingBox()
    expect(winBox!.width).toBeGreaterThan(700) // the host actually got wide
    // --measure-prose = 38rem @15px-root = 570px (content-box + ~17px pad/border).
    expect(contentBox!.width).toBeLessThanOrEqual(590)
  })

  test('clicking the back window raises it to the front (z-order)', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await enterFocusStage(page)
    await openWindow(page, NODE2) // z=1 (DOM order nth0)
    await openWindow(page, NODE3) // z=2 (front)

    const zIndexes = (): Promise<number[]> =>
      page
        .getByRole('dialog')
        .evaluateAll((els) => els.map((e) => Number.parseInt(getComputedStyle(e).zIndex, 10)))

    const before = await zIndexes()
    expect(before[0]).toBeLessThan(before[1]) // nth0 (NODE2) behind

    // Click NODE2's exposed top-left sliver (cascade offset = 24px → not covered).
    const back = page.getByRole('dialog').nth(0)
    const box = await back.boundingBox()
    await page.mouse.click(box!.x + 8, box!.y + 8)

    const after = await zIndexes()
    expect(after[0]).toBeGreaterThan(after[1]) // NODE2 now in front
  })

  test('close removes the window and returns focus to the graph region (never body)', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await enterFocusStage(page)
    await openWindow(page, NODE2)

    await page.getByRole('button', { name: 'close' }).first().click()
    await expect(page.getByRole('dialog')).toHaveCount(0)

    // Node-click window has triggerEl=null → fallbackFocusEl = the .viewport
    // (tabindex=-1). Focus must land there, NEVER silently on <body>.
    const active = await page.evaluate(() => ({
      tag: document.activeElement?.tagName ?? '',
      cls: document.activeElement?.className ?? '',
    }))
    expect(active.tag).not.toBe('BODY')
    expect(active.cls).toContain('viewport')
  })

  test('open nodes are highlighted via Sigma display-data', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await enterFocusStage(page)
    await openWindow(page, NODE2)

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

  test('dragging over a window does NOT pan the camera; gaps click through to the canvas', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await enterFocusStage(page)
    await openWindow(page, NODE2)
    // Let setFocus's animatedReset (300ms) settle so the baseline is stable.
    await page.waitForTimeout(500)

    const camState = (): Promise<string> =>
      page.evaluate(() => {
        const g = (window as unknown as { __ctxGraph: CtxGraph }).__ctxGraph
        return JSON.stringify(g.renderer.getCamera().getState())
      })

    const before = await camState()
    await dragTitlebar(page, page.getByRole('dialog').first(), 120, 90)
    const after = await camState()
    // setPointerCapture kept the pointer stream off the Sigma canvas → no pan.
    expect(after).toBe(before)

    // Click-through proof: a point in a gap (pointer-events:none on .wm-root)
    // resolves to the Sigma <canvas> below, NOT the overlay root.
    // Sample the bottom-right gap (0.9/0.9): after U04-W4 the default window is
    // 600×570 (was 420×330), so the dragged card now reaches ~(1185,675) on the
    // 1440×900 viewport and the former 0.72/0.75 sample sits INSIDE it. The point
    // just needs to be a real overlay gap; bottom-right clears the enlarged card
    // and the top-left chrome overlay alike.
    const vp = await page.locator('.viewport').boundingBox()
    const hit = await page.evaluate(
      ([x, y]) => {
        const el = document.elementFromPoint(x as number, y as number)
        return { tag: el?.tagName ?? '', cls: (el as HTMLElement)?.className ?? '' }
      },
      [vp!.x + vp!.width * 0.9, vp!.y + vp!.height * 0.9],
    )
    expect(hit.cls).not.toContain('wm-root')
    expect(hit.tag).toBe('CANVAS')
  })

  test('a11y: role=dialog/aria-modal=false, open-focus in window, Tab→close, Esc closes + focus return', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await enterFocusStage(page)
    await openWindow(page, NODE2)

    const win = page.getByRole('dialog').first()
    await expect(win).toHaveAttribute('aria-modal', 'false')

    // Focus lands in the window container on open (no <body> hole).
    const openFocusRole = await page.evaluate(() => document.activeElement?.getAttribute('role') ?? '')
    expect(openFocusRole).toBe('dialog')

    // Tab order inside the chrome: minimize → close (both reachable).
    await page.keyboard.press('Tab')
    await page.keyboard.press('Tab')
    await expect(win.getByRole('button', { name: 'close' })).toBeFocused()

    // Esc closes the focused window and returns focus to the graph region.
    await page.keyboard.press('Escape')
    await expect(page.getByRole('dialog')).toHaveCount(0)
    const afterTag = await page.evaluate(() => document.activeElement?.tagName ?? '')
    expect(afterTag).not.toBe('BODY')
    const afterCls = await page.evaluate(() => document.activeElement?.className ?? '')
    expect(afterCls).toContain('viewport')
  })

  // S4.3 — the ONLY security-relevant negative probe of this axis. The scope gate
  // stays server-authoritative: a cross-scope/cross-tenant READ returns the REAL
  // deny shape HTTP 200 + {success:false, error:'Block not found'} (context_manage.go
  // :465-469 — NEVER 403; GetBlock re-gates after the full-UUID resolve). apiFetch
  // collapses that 200/success:false to an ApiError → the window shows
  // <p role=alert>Block not found</p> and NO <pre.content>.
  test('scope-deny: window shows the alert, never pre.content (real 200 deny shape)', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    // Override ONLY the manage call to the real deny shape, BEFORE opening. The
    // /api/graph/ego route (focus navigation) stays untouched. Registered after
    // seedSession's '**/api/**' so it wins for /api/manage (last route first).
    await page.route('**/api/manage', (r) =>
      r.fulfill({ status: 200, json: { success: false, error: 'Block not found' } }),
    )
    await enterFocusStage(page)
    await openWindow(page, NODE2)

    await expect(page.getByRole('alert')).toHaveText(/Block not found/)
    await expect(page.locator('pre.content')).toHaveCount(0)
  })
})

// U04-W1 — Content-Guard: der Load-$effect in BlockDetailContent ist idempotent
// gegenüber WinState-Invalidierungen mit wertgleicher id (design 04-§4.1/§7-W1).
// Jede Fenster-Geste (focus/move/restore) ersetzt im Ist das WinState-Objekt via
// .map() → der Load-$effect feuerte erneut → detail=null/loading=true zerstörte
// <pre.content> → scrollTop kollabierte auf 0 + Refetch. Nach W1: Δ /api/manage=0,
// scrollTop erhalten, das <pre>-Element (samt DOM-Marker) überlebt jede Geste.
// vitest läuft node-only (kein DOM, vite.config.ts:60-64) → dieses Verhalten wird
// hier per Playwright gegated (design 04-§2 Test-Konvention, §7-W1).
test.describe('graph floating windows — W1 content-guard (U04-W1)', () => {
  // Der Fixture-'get' liefert nur einen kurzen Absatz — zu kurz, damit .body über
  // 150px scrollt. Hier ein langer, deterministischer Block, andere Aktionen
  // (list-meta/list-categories beim Graph-Boot) fallen per fallback() an die
  // seedSession-Mocks durch.
  const LONG_CONTENT = Array.from({ length: 80 }, (_, i) => `Zeile ${i + 1}: ctx-Volltext für den Scroll-Erhalt-Beweis.`).join('\n')

  async function longGetContent(page: Page): Promise<void> {
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
              tags: ['demo', 'design'],
              title: 'Core Architecture',
              content: LONG_CONTENT,
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
  }

  test('Scroll und Content überleben Fenster-Gesten (idempotenter Load)', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await longGetContent(page)

    // /api/manage-get-Zähler: nur die getBlock-Refetches nach dem Armen zählen
    // (list-meta/-categories tragen eine andere action und werden ausgefiltert).
    let manageGets = 0
    page.on('request', (r) => {
      if (r.method() === 'POST' && r.url().includes('/api/manage') && (r.postData() ?? '').includes('"action":"get"')) {
        manageGets += 1
      }
    })

    await enterFocusStage(page)
    await openWindow(page, NODE2)

    const win = page.getByRole('dialog').first()
    const body = win.locator('.body')
    const pre = win.locator('pre.content')
    await expect(pre).toBeVisible()

    // scrollTop setzen + DOM-Marker auf genau dieses <pre> heften. Ein Remount
    // ersetzt das Element → der Marker verschwindet (Rot-Kriterium).
    await body.evaluate((el) => {
      el.scrollTop = 150
    })
    await pre.evaluate((el) => {
      ;(el as HTMLElement).dataset.probe = 'W1-KEEP'
    })
    expect(await body.evaluate((el) => el.scrollTop)).toBe(150)

    // Nach JEDER Geste: kein Refetch, scrollTop erhalten, <pre>-Marker erhalten,
    // kein Ladezustand. 250ms Setzzeit, damit ein etwaiger Refetch/Remount landet.
    const assertSurvived = async (label: string, base: number): Promise<void> => {
      await page.waitForTimeout(250)
      expect(manageGets - base, `${label}: Δ /api/manage-get`).toBe(0)
      expect(await body.evaluate((el) => el.scrollTop), `${label}: scrollTop`).toBe(150)
      const marker = await pre.evaluate((el) => (el as HTMLElement).dataset.probe ?? '')
      expect(marker, `${label}: DOM-Marker auf <pre>`).toBe('W1-KEEP')
      await expect(win.getByText('loading content…'), `${label}: kein Ladezustand`).toHaveCount(0)
    }

    // (i) pointerdown+up auf der Titlebar → store.focus (WinState-.map im Ist).
    let base = manageGets
    {
      const tb = await win.locator('.titlebar').boundingBox()
      if (!tb) throw new Error('titlebar has no box')
      await page.mouse.move(tb.x + 12, tb.y + tb.height / 2)
      await page.mouse.down()
      await page.mouse.up()
    }
    await assertSurvived('Titlebar-Klick (focus)', base)

    // (ii) Re-Klick auf denselben Node über die emit-Seam → store.open → dedup →
    // restore (im Ist: restore-.map + focus-.map).
    base = manageGets
    await openWindow(page, NODE2)
    await assertSurvived('Re-Klick (dedup restore)', base)

    // (iii) pointerdown im .body → window onpointerdown → store.focus.
    base = manageGets
    {
      const bb = await body.boundingBox()
      if (!bb) throw new Error('body has no box')
      await page.mouse.move(bb.x + bb.width / 2, bb.y + 8)
      await page.mouse.down()
      await page.mouse.up()
    }
    await assertSurvived('Body-pointerdown (focus)', base)

    // (iv) Drag-Mikroszenario: pointerdown + 2 pointermoves + pointerup auf der
    // Titlebar → focus + 2× store.move (im Ist: 3 WinState-.map → 3 Remounts).
    base = manageGets
    {
      const tb = await win.locator('.titlebar').boundingBox()
      if (!tb) throw new Error('titlebar has no box')
      const sx = tb.x + 12
      const sy = tb.y + tb.height / 2
      await page.mouse.move(sx, sy)
      await page.mouse.down()
      await page.mouse.move(sx + 20, sy + 15) // pointermove #1 → store.move
      await page.mouse.move(sx + 40, sy + 30) // pointermove #2 → store.move
      await page.mouse.up()
    }
    await assertSurvived('Drag-Mikroszenario (2 moves)', base)

    // Zusatz: ein zweites Fenster (anderer Node) startet frisch bei scrollTop 0,
    // während Fenster 1 unberührt bei 150 bleibt (Node-Wechsel = eigenes Fenster,
    // eigener Mount — design 04-§1/§4.1).
    await openWindow(page, NODE3)
    await expect(page.getByRole('dialog')).toHaveCount(2)
    const win2 = page.getByRole('dialog').nth(1)
    await expect(win2.locator('pre.content')).toBeVisible()
    expect(await win2.locator('.body').evaluate((el) => el.scrollTop)).toBe(0)
    expect(await body.evaluate((el) => el.scrollTop)).toBe(150)
  })
})

// U04-W3 — Minimize/Restore ohne Remount (Graph-Host opt-in, design 04-§4.3/§7-W3).
// Der Graph-Host reicht keepMinimized → WindowManager rendert ALLE Fenster und
// versteckt minimierte per display:none (keep-mounted) statt sie aus dem keyed
// each zu werfen. Restore ist damit KEIN Remount: Scroll, DOM-Marker und der
// geladene Content überleben; kein Refetch. Der Mount-Autofokus bleibt (Open =
// frischer Mount), ein ADDITIVER Flanken-$effect trägt den Fokus beim Restore
// (true→false) zurück auf role=dialog. Rot gegen Ist (Voll-Remount, Messwert G):
// Marker weg, Δ /api/manage-get=1, scrollTop 0, kein Fokus.
test.describe('graph floating windows — W3 keep-mounted (U04-W3, Graph-Host)', () => {
  const LONG_CONTENT = Array.from({ length: 80 }, (_, i) => `Zeile ${i + 1}: ctx-Volltext für den W3-Restore-Erhalt-Beweis.`).join('\n')

  async function longGetContent(page: Page): Promise<void> {
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
              tags: ['demo', 'design'],
              title: 'Core Architecture',
              content: LONG_CONTENT,
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
  }

  test('Restore nach Minimize ohne Remount: Scroll, Content und Fokus überleben', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await longGetContent(page)

    let manageGets = 0
    page.on('request', (r) => {
      if (r.method() === 'POST' && r.url().includes('/api/manage') && (r.postData() ?? '').includes('"action":"get"')) {
        manageGets += 1
      }
    })

    await enterFocusStage(page)
    await openWindow(page, NODE2)

    const win = page.getByRole('dialog').first()
    const body = win.locator('.body')
    const pre = win.locator('pre.content')
    await expect(pre).toBeVisible()

    // Positiv-Assert: der Mount-Autofokus trägt den Open-Fokus auf role=dialog
    // (sichert, dass der Umbau den Open-Fokus NICHT bricht, design 04-§4.3).
    expect(await page.evaluate(() => document.activeElement?.getAttribute('role') ?? '')).toBe('dialog')

    // scrollTop + DOM-Marker auf genau dieses <pre>. Ein Remount ersetzt das
    // Element → Marker weg (Rot-Kriterium gegen Ist / gegen keepMinimized=false).
    await body.evaluate((el) => {
      el.scrollTop = 150
    })
    await pre.evaluate((el) => {
      ;(el as HTMLElement).dataset.probe = 'W3-KEEP'
    })
    expect(await body.evaluate((el) => el.scrollTop)).toBe(150)

    const base = manageGets
    // Minimize: Fenster verlässt den a11y-Tree (display:none), bleibt aber gemountet.
    await win.getByRole('button', { name: 'minimize' }).click()
    await expect(page.getByRole('dialog')).toHaveCount(0)
    await expect(page.locator('.window.minimized')).toHaveCount(1) // keep-mounted-Beleg

    // Restore über den Minbar-Chip → store.restore. KEIN Remount.
    await page.locator('.minbar .chip').first().click()
    await expect(page.getByRole('dialog')).toHaveCount(1)
    await page.waitForTimeout(250) // ein etwaiger Refetch/Remount hätte hier gelandet

    // Δ /api/manage-get == 0, scrollTop erhalten, DERSELBE <pre> (Marker), Fokus
    // zurück auf dem restaurierten Fenster (Flanken-$effect).
    expect(manageGets - base, 'Restore: Δ /api/manage-get').toBe(0)
    expect(await body.evaluate((el) => el.scrollTop), 'Restore: scrollTop').toBe(150)
    expect(await pre.evaluate((el) => (el as HTMLElement).dataset.probe ?? ''), 'Restore: DOM-Marker auf <pre>').toBe('W3-KEEP')
    expect(await page.evaluate(() => document.activeElement?.getAttribute('role') ?? ''), 'Restore: Fokus auf role=dialog').toBe('dialog')
  })

  test('minimiert: das Fenster verlässt den a11y-Tree und ist nicht per Tab erreichbar (negativ)', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await longGetContent(page)
    await enterFocusStage(page)
    await openWindow(page, NODE2)

    const win = page.getByRole('dialog').first()
    await expect(win).toHaveCount(1)
    await expect(page.getByRole('button', { name: 'close' })).toHaveCount(1)

    await win.getByRole('button', { name: 'minimize' }).click()

    // display:none ⇒ raus aus a11y-Tree und Tab-Order: keine role=dialog, kein
    // fokussierbarer close-Button — obwohl das DOM (keep-mounted) noch existiert.
    await expect(page.getByRole('dialog')).toHaveCount(0)
    await expect(page.getByRole('button', { name: 'close' })).toHaveCount(0)
    await expect(page.locator('.window.minimized')).toHaveCount(1)

    // Restore stellt beides wieder her.
    await page.locator('.minbar .chip').first().click()
    await expect(page.getByRole('dialog')).toHaveCount(1)
    await expect(page.locator('.window.minimized')).toHaveCount(0)
  })
})

// U04-W3 SSE-Gate (Board-Host, design 04-§4.3/§6/§7-W3). Der Board-Renderer
// IssueDetailContent hält je Instanz eine LiveSource (SSE-Bearer-fetch auf
// /api/project/events + 10s-Poll). Deshalb ist der Board-Host bewusst
// keepMinimized=false (BoardPage UNANGETASTET): Minimize ZERSTÖRT das Fenster ⇒
// live.stop() ⇒ AbortController schließt die SSE-Verbindung (sie SINKT). Die
// naive W3-Fassung (Board ebenfalls keep-mounted per display:none) hielte die
// Verbindung offen — das ist der Rot-Beleg.
test.describe('graph floating windows — W3 SSE-Gate (U04-W3, Board-Host)', () => {
  test('Minimize eines Board-Fensters schließt seine SSE-Verbindung (destroy-basiert)', async ({ page }) => {
    await seedSession(page, { role: 'member', theme: 'dark', state: 'board' })

    // Jede LiveSource öffnet EINE fetch-SSE-Verbindung auf /api/project/events.
    // Die Route wird GEHÄNGT (nie fulfillt), damit jede Verbindung als genau EIN
    // in-flight-Request steht (ein atomarer Body → clean-EOF → Reconnect ~1s →
    // Rauschen). Der App-AbortController (live.stop()) bricht sie ab →
    // Playwright feuert 'requestfailed' auf genau dieser Verbindung.
    await page.route('**/api/project/events**', () => new Promise<void>(() => {}))

    const isEvents = (u: string): boolean => u.includes('/api/project/events')
    let opens = 0
    let fails = 0
    page.on('request', (r) => {
      if (isEvents(r.url())) opens += 1
    })
    page.on('requestfailed', (r) => {
      if (isEvents(r.url())) fails += 1
    })

    await gotoArea(page, '/board?scope=acme:main')
    const firstCard = page.locator('[data-board-card]').first()
    await expect(firstCard).toBeVisible()
    // Board-eigene LiveSource verbunden (Verbindung #1).
    await expect.poll(() => opens, { timeout: 10_000 }).toBeGreaterThanOrEqual(1)

    // Board-Fenster öffnen → IssueDetailContent mountet → eigene LiveSource
    // verbindet (die Fenster-eigene Verbindung).
    const opensBeforeOpen = opens
    await firstCard.click()
    const win = page.getByRole('dialog').first()
    await expect(win).toBeVisible()
    await expect.poll(() => opens, { timeout: 10_000 }).toBe(opensBeforeOpen + 1)

    // Minimize → Board-Host keepMinimized=false ⇒ Fenster ZERSTÖRT ⇒ live.stop()
    // ⇒ die Fenster-SSE-Verbindung wird abgebrochen (sie SINKT). Kein keep-
    // mounted display:none-Fenster (destroy-Beleg gegen die naive Fassung).
    const failsBeforeMin = fails
    await win.getByRole('button', { name: 'minimize' }).click()
    await expect(page.getByRole('dialog')).toHaveCount(0)
    // Die Verbindung des minimierten Fensters SINKT (destroy → live.stop() →
    // AbortController). Rot gegen die naive Fassung: dort bleibt sie offen.
    await expect.poll(() => fails, { timeout: 10_000 }).toBe(failsBeforeMin + 1)
    // Destroy-Korroboration: kein keep-mounted display:none-Fenster.
    await expect(page.locator('.window.minimized')).toHaveCount(0)
  })
})

// U04-W7 — Board-Host Drag-Region-Contract (AM-4, design 04-§4.5). Der Board-
// Renderer IssueDetailContent nimmt am DOM-Vertrag teil: in einem FloatingWindow
// (titleId gesetzt → .in-window) ziehen die Kopf-Freiflächen .titlebar + .meta das
// Fenster; Titel-Edit/Status-Wechsel (button/input/select via DRAG_EXEMPT) und die
// kopier-relevanten Labels (data-window-drag-exempt) sind ausgenommen. Der Kern-
// Unterschied zu W5: dieser Renderer hat ZWEI Hosts — auf der /issues/:id-Route
// (kein titleId) ist der Marker ABWESEND und der Lese-Host bleibt selektierbar.
// Rot gegen Ist: dort trägt IssueDetailContent keine Marker/kein user-select:none.
test.describe('board window — W7 drag region (U04-W7, AM-4)', () => {
  const ISSUE = '11111111-1111-1111-1111-111111111111' // board.json first card

  /** Override the detail GET so the issue is in the caller home scope (writable →
   *  Status-Picker + Edit-Button + Composer render) with copy-relevant labels and
   *  a long body. Only the detail GET (…/issues/{id}) is intercepted; comments,
   *  /board and the issues list fall through to the seedSession mocks. */
  async function homeScopeDetail(page: Page): Promise<void> {
    await page.route('**/api/project/**', async (route) => {
      const url = route.request().url()
      if (route.request().method() === 'GET' && /\/issues\/[^/?]+(?:\?|$)/.test(url)) {
        return route.fulfill({
          status: 200,
          json: {
            success: true,
            render: 'untrusted',
            issue: {
              id: ISSUE,
              category: 'task',
              type: 'issue',
              type_source: 'manual',
              title: 'Drag region issue',
              content: '# Body\n\n' + Array.from({ length: 60 }, (_, i) => `Zeile ${i + 1} des Volltexts.`).join('\n\n'),
              scope: 'home',
              sensitivity: 'internal',
              sensitivity_source: 'manual',
              lifecycle_state: 'active',
              tags: ['bug', 'p1'],
              metadata: {},
              created_at: '2026-07-01T00:00:00Z',
              updated_at: '2026-07-03T00:00:00Z',
              workflow_status: 'open',
            },
            comments: [],
            comments_cursor: null,
          },
        })
      }
      return route.fallback()
    })
  }

  async function dragFrom(page: Page, sx: number, sy: number, dx: number, dy: number): Promise<void> {
    await page.mouse.move(sx, sy)
    await page.mouse.down()
    await page.mouse.move(sx + dx, sy + dy, { steps: 8 })
    await page.mouse.up()
  }

  async function openBoardWindow(page: Page): Promise<Locator> {
    await seedSession(page, { role: 'server-admin', theme: 'dark', state: 'board' })
    await homeScopeDetail(page)
    await gotoArea(page, '/board?scope=acme:main')
    const firstCard = page.locator('[data-board-card]').first()
    await expect(firstCard).toBeVisible()
    await firstCard.click()
    const win = page.getByRole('dialog').first()
    await expect(win).toBeVisible()
    // Detail geladen + writable (Status-Picker sichtbar).
    await expect(win.locator('.issue')).toBeVisible()
    await expect(win.locator('select[aria-label="Workflow status"]')).toBeVisible()
    return win
  }

  test('Drag von der Meta-Freifläche (.type) bewegt das Board-Fenster (>40px)', async ({ page }) => {
    const win = await openBoardWindow(page)
    const type = win.locator('.meta .type')
    await expect(type).toBeVisible()
    const box = await type.boundingBox()
    if (!box) throw new Error('type has no box')
    const before = await win.boundingBox()
    await dragFrom(page, box.x + box.width / 2, box.y + box.height / 2, 110, 70)
    const after = await win.boundingBox()
    // Rot gegen Ist: die Meta war keine Drag-Fläche → Delta ~0.
    expect(Math.abs(after!.x - before!.x) + Math.abs(after!.y - before!.y)).toBeGreaterThan(40)
  })

  test('Interaktive Kopf-Controls + kopier-relevante Labels ziehen NICHT; user-select gepinnt', async ({ page }) => {
    const win = await openBoardWindow(page)

    // (a) Der Status-<select> zieht NICHT (generisch DRAG_EXEMPT) und bleibt bedienbar.
    const select = win.locator('select[aria-label="Workflow status"]')
    {
      const box = await select.boundingBox()
      if (!box) throw new Error('select has no box')
      const b0 = await win.boundingBox()
      await dragFrom(page, box.x + box.width / 2, box.y + box.height / 2, 90, 60)
      const b1 = await win.boundingBox()
      expect(Math.abs(b1!.x - b0!.x) + Math.abs(b1!.y - b0!.y), 'select Drag').toBeLessThan(5)
    }
    await expect(select).toBeEnabled()

    // (b) Der Edit-Title-Button zieht NICHT und sein Klick öffnet die Titel-Edit.
    const editBtn = win.getByRole('button', { name: 'Edit title' })
    {
      const box = await editBtn.boundingBox()
      if (!box) throw new Error('edit button has no box')
      const b0 = await win.boundingBox()
      await dragFrom(page, box.x + box.width / 2, box.y + box.height / 2, 80, 50)
      const b1 = await win.boundingBox()
      expect(Math.abs(b1!.x - b0!.x) + Math.abs(b1!.y - b0!.y), 'edit-btn Drag').toBeLessThan(5)
    }
    await editBtn.click()
    const titleInput = win.locator('input[aria-label="Issue title"]')
    await expect(titleInput).toBeVisible()
    // Der Titel-Input ist trotz Drag-Region-Elter selektierbar (user-select:text).
    expect(await titleInput.evaluate((el) => getComputedStyle(el).userSelect), 'title input user-select').toBe('text')
    await win.getByRole('button', { name: 'Cancel' }).click()

    // (c) Die kopier-relevanten Labels (exempt) ziehen NICHT.
    const labels = win.locator('.meta .labels')
    {
      const box = await labels.boundingBox()
      if (!box) throw new Error('labels has no box')
      const b0 = await win.boundingBox()
      await dragFrom(page, box.x + box.width / 2, box.y + box.height / 2, 90, 60)
      const b1 = await win.boundingBox()
      expect(Math.abs(b1!.x - b0!.x) + Math.abs(b1!.y - b0!.y), 'labels (exempt) Drag').toBeLessThan(5)
    }

    // user-select: meta = none (Drag-Region), labels = text (selektierbar).
    expect(await win.locator('.meta').evaluate((el) => getComputedStyle(el).userSelect), '.meta user-select').toBe('none')
    expect(await labels.evaluate((el) => getComputedStyle(el).userSelect), '.labels user-select').toBe('text')
  })

  test('Der Volltext-Body zieht NICHT (scrollt/selektiert wie Ist)', async ({ page }) => {
    const win = await openBoardWindow(page)
    const body = win.locator('.issue-body')
    await expect(body).toBeVisible()
    const box = await body.boundingBox()
    if (!box) throw new Error('body has no box')
    const b0 = await win.boundingBox()
    await dragFrom(page, box.x + box.width / 2, box.y + 20, 90, 60)
    const b1 = await win.boundingBox()
    expect(Math.abs(b1!.x - b0!.x) + Math.abs(b1!.y - b0!.y), 'issue-body Drag').toBeLessThan(5)
    expect(await body.evaluate((el) => getComputedStyle(el).userSelect), '.issue-body user-select').not.toBe('none')
  })

  test('Host-aware: die /issues/:id-Route trägt KEINE Drag-Marker und bleibt selektierbar', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark', state: 'board' })
    await homeScopeDetail(page)
    await gotoArea(page, `/issues/${ISSUE}?scope=acme:main`)
    const meta = page.locator('.meta').first()
    await expect(meta).toBeVisible()
    // Kein data-window-drag auf der Route (inWindow=false).
    expect(await meta.getAttribute('data-window-drag'), 'route .meta marker').toBeNull()
    expect(await page.locator('.titlebar').first().getAttribute('data-window-drag'), 'route .titlebar marker').toBeNull()
    // Und kein user-select:none → der Leser kann Titel/Meta selektieren.
    expect(await meta.evaluate((el) => getComputedStyle(el).userSelect), 'route .meta user-select').not.toBe('none')
  })
})

// U04-W5 — Drag-Region-Contract: die Kopf-Freifläche (header + Meta) zieht das
// Fenster (design 04-§4.5/§7-W5). FloatingWindow delegiert den Body-pointerdown
// gegen den DOM-Vertrag (data-window-drag / data-window-drag-exempt); startDrag/
// startResize sind gegen Multi-Pointer gehärtet (activePointer-Re-Entry-Guard +
// pointerId-Filter) und räumen bei pointercancel denselben Cleanup wie bei
// pointerup ab. Rot gegen Ist: (a) Meta-Drag bewegt NICHT (Body war nicht drag-
// gebunden); Multi-Pointer/pointercancel siehe die dokumentierten Guard-losen
// Rot-Proben im Report (temporäre Guard-Entfernung auf derselben Fläche).
test.describe('graph floating windows — W5 drag region (U04-W5)', () => {
  const LONG_CONTENT = Array.from({ length: 80 }, (_, i) => `Zeile ${i + 1}: ctx-Volltext für den W5-Drag-Freifläche-Beweis.`).join('\n')

  async function longGetContent(page: Page): Promise<void> {
    await page.route('**/api/manage', async (route) => {
      const body = route.request().postDataJSON() as { action?: string } | null
      if (body?.action === 'get') {
        return route.fulfill({
          status: 200,
          json: {
            success: true,
            block: {
              id: NODE2,
              category: 'reference',
              tags: ['demo', 'design'],
              title: 'API Spec',
              content: LONG_CONTENT,
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
  }

  /** Erster ziehbarer, NICHT-exempter Punkt in der Meta (Label/Gap-Freifläche),
   *  in aktuellen Viewport-Koordinaten (respektiert den .body-Scroll-Clip). */
  async function metaDragPoint(page: Page, win: Locator): Promise<{ x: number; y: number }> {
    const box = await win.locator('dl.meta').boundingBox()
    if (!box) throw new Error('meta has no box')
    const pt = await page.evaluate(({ bx, by, width, height }) => {
      const exempt = 'a, button, input, select, textarea, [contenteditable], [data-window-drag-exempt]'
      const inDrag = (x: number, y: number): boolean => {
        const el = document.elementFromPoint(x, y) as HTMLElement | null
        return !!el && el.closest('[data-window-drag]') !== null && el.closest(exempt) === null
      }
      for (let gy = 0.12; gy <= 0.88; gy += 0.08) {
        for (let gx = 0.03; gx <= 0.6; gx += 0.04) {
          const x = bx + width * gx
          const y = by + height * gy
          if (inDrag(x, y)) return { x, y }
        }
      }
      return null
    }, { bx: box.x, by: box.y, width: box.width, height: box.height })
    if (!pt) throw new Error('no draggable non-exempt point found in meta')
    return pt
  }

  async function dragFrom(page: Page, sx: number, sy: number, dx: number, dy: number): Promise<void> {
    await page.mouse.move(sx, sy)
    await page.mouse.down()
    await page.mouse.move(sx + dx, sy + dy, { steps: 8 })
    await page.mouse.up()
  }

  test('Drag von der Meta-Freifläche bewegt das Fenster (kein Pan, kein Refetch, Scroll erhalten)', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await longGetContent(page)

    let manageGets = 0
    page.on('request', (r) => {
      if (r.method() === 'POST' && r.url().includes('/api/manage') && (r.postData() ?? '').includes('"action":"get"')) {
        manageGets += 1
      }
    })

    await enterFocusStage(page)
    await openWindow(page, NODE2)
    // setFocus animatedReset (300ms) abklingen lassen → stabile Kamera-Baseline.
    await page.waitForTimeout(500)

    const win = page.getByRole('dialog').first()
    const body = win.locator('.body')
    await expect(win.locator('pre.content')).toBeVisible()

    // Ein Nicht-Null-Scroll, der die Meta oben teilweise sichtbar lässt.
    await body.evaluate((el) => {
      el.scrollTop = 40
    })
    expect(await body.evaluate((el) => el.scrollTop)).toBe(40)

    const camState = (): Promise<string> =>
      page.evaluate(() => {
        const g = (window as unknown as { __ctxGraph: CtxGraph }).__ctxGraph
        return JSON.stringify(g.renderer.getCamera().getState())
      })

    const before = await win.boundingBox()
    const camBefore = await camState()
    const base = manageGets

    const pt = await metaDragPoint(page, win)
    await dragFrom(page, pt.x, pt.y, 110, 70)

    const after = await win.boundingBox()
    // Rot gegen Ist: dort ist die Meta keine Drag-Fläche → Delta ~0.
    expect(Math.abs(after!.x - before!.x) + Math.abs(after!.y - before!.y)).toBeGreaterThan(40)
    // setPointerCapture hält den Stream weg vom Sigma-Canvas → kein Kamera-Pan.
    expect(await camState()).toBe(camBefore)
    // Kein Remount → Scroll erhalten, kein zusätzlicher manage-get.
    expect(await body.evaluate((el) => el.scrollTop)).toBe(40)
    expect(manageGets - base, 'Δ /api/manage-get').toBe(0)
  })

  test('Negativ: pre.content und der exempt id-<dd> ziehen NICHT; user-select stimmt', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await longGetContent(page)
    await enterFocusStage(page)
    await openWindow(page, NODE2)
    await page.waitForTimeout(500)

    const win = page.getByRole('dialog').first()
    const pre = win.locator('pre.content')
    await expect(pre).toBeVisible()

    // Drag auf dem Volltext bewegt das Fenster NICHT (nicht markiert → scrollt/selektiert).
    {
      const box = await pre.boundingBox()
      if (!box) throw new Error('pre has no box')
      const b0 = await win.boundingBox()
      await dragFrom(page, box.x + box.width / 2, box.y + 20, 90, 60)
      const b1 = await win.boundingBox()
      expect(Math.abs(b1!.x - b0!.x) + Math.abs(b1!.y - b0!.y), 'pre.content Drag').toBeLessThan(5)
    }

    // Drag auf dem exempt id-<dd> (kopier-relevante UUID) bewegt NICHT.
    const idDd = win.locator('dl.meta dd[data-window-drag-exempt]').first()
    {
      const box = await idDd.boundingBox()
      if (!box) throw new Error('id-dd has no box')
      const b0 = await win.boundingBox()
      await dragFrom(page, box.x + box.width / 2, box.y + box.height / 2, 90, 60)
      const b1 = await win.boundingBox()
      expect(Math.abs(b1!.x - b0!.x) + Math.abs(b1!.y - b0!.y), 'id-dd (exempt) Drag').toBeLessThan(5)
    }

    // user-select: Meta = none (Drag-Region), exempt id-<dd> = text (selektierbar).
    const metaUserSelect = await win.locator('dl.meta').evaluate((el) => getComputedStyle(el).userSelect)
    expect(metaUserSelect, 'dl.meta user-select').toBe('none')
    const idUserSelect = await idDd.evaluate((el) => getComputedStyle(el).userSelect)
    expect(idUserSelect, 'id-dd user-select').toBe('text')
  })

  test('Multi-Pointer: das Fenster folgt NUR dem ersten Finger (Guard gegen Doppel-Delta)', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await longGetContent(page)
    await enterFocusStage(page)
    await openWindow(page, NODE2)
    await page.waitForTimeout(500)

    const win = page.getByRole('dialog').first()
    await expect(win.locator('pre.content')).toBeVisible()

    // Zwei Punkte weit auseinander auf der Header-Drag-Fläche (scrollTop 0 → h2
    // voll sichtbar und breit). Ohne Guard rechnet die zweite onMove-Closure ihr
    // Delta gegen die Start-Koordinate des ANDEREN Pointers → Sprung; mit Guard
    // folgt das Fenster nur dem ERSTEN Finger.
    const hb = await win.locator('header').boundingBox()
    if (!hb) throw new Error('header has no box')
    const y = hb.y + hb.height / 2
    const p1 = { x: hb.x + hb.width * 0.2, y }
    const p2 = { x: hb.x + hb.width * 0.75, y }

    const client = await page.context().newCDPSession(page)
    await client.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [p1, p2] })
    const before = await win.boundingBox()
    // beide Finger um dasselbe Delta bewegen (2 Schritte)
    await client.send('Input.dispatchTouchEvent', {
      type: 'touchMove',
      touchPoints: [{ x: p1.x + 20, y: y + 12 }, { x: p2.x + 20, y: y + 12 }],
    })
    await client.send('Input.dispatchTouchEvent', {
      type: 'touchMove',
      touchPoints: [{ x: p1.x + 40, y: y + 25 }, { x: p2.x + 40, y: y + 25 }],
    })
    await client.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] })
    const after = await win.boundingBox()

    const ddx = after!.x - before!.x
    const ddy = after!.y - before!.y
    // Folgt dem ersten Finger (~+40/+25). Rot gegen die Guard-lose Fassung: dort
    // ein erratischer Sprung (Delta gegen die 2. Start-Koordinate, |Δ| ≫).
    expect(Math.abs(ddx - 40), `x-Delta ${ddx}`).toBeLessThanOrEqual(22)
    expect(Math.abs(ddy - 25), `y-Delta ${ddy}`).toBeLessThanOrEqual(20)
  })

  test('pointercancel mitten im Drag: der Folge-Drag hat ein einfaches Delta (kein Leck)', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await longGetContent(page)
    await enterFocusStage(page)
    await openWindow(page, NODE2)
    await page.waitForTimeout(500)

    const win = page.getByRole('dialog').first()
    await expect(win.locator('pre.content')).toBeVisible()

    const hb = await win.locator('header').boundingBox()
    if (!hb) throw new Error('header has no box')
    const y = hb.y + hb.height / 2
    const start = { x: hb.x + hb.width * 0.3, y }

    const client = await page.context().newCDPSession(page)
    // Drag #1: start → move → CANCEL (Browser übernimmt den Touch). Ohne
    // pointercancel-Cleanup bliebe die onMove-Closure am Handle hängen.
    await client.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [start] })
    await client.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x: start.x + 30, y: y + 20 }] })
    await client.send('Input.dispatchTouchEvent', { type: 'touchCancel', touchPoints: [] })

    const afterCancel = await win.boundingBox()

    // Drag #2: frischer, sauberer Ein-Finger-Drag um (+30/+20). Bei einem Leck
    // feuerte jedes pointermove ZWEI Closures → doppeltes Delta. Header-Box NEU
    // messen — Drag #1 hat das Fenster (samt Header) verschoben.
    const hb2 = await win.locator('header').boundingBox()
    if (!hb2) throw new Error('header has no box (drag2)')
    const y2 = hb2.y + hb2.height / 2
    const start2 = { x: hb2.x + hb2.width * 0.3, y: y2 }
    await client.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [start2] })
    await client.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x: start2.x + 15, y: y2 + 10 }] })
    await client.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x: start2.x + 30, y: y2 + 20 }] })
    await client.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] })
    const afterDrag2 = await win.boundingBox()

    const d2x = afterDrag2!.x - afterCancel!.x
    const d2y = afterDrag2!.y - afterCancel!.y
    // Einfaches Delta (~+30/+20). Rot gegen die Cancel-lose Fassung: ~doppelt.
    expect(Math.abs(d2x - 30), `Folge-Drag x-Delta ${d2x}`).toBeLessThanOrEqual(16)
    expect(Math.abs(d2y - 20), `Folge-Drag y-Delta ${d2y}`).toBeLessThanOrEqual(14)
  })
})

// G6 — Mobile full-bleed sheet + minimize-bar (design 07-§Wellen G6, §D mobile).
// Reuses the same open-seam (enterFocusStage → emit clickNode). Below SM=640 the
// WindowManager renders ONLY store.topId as a `sheet` FloatingWindow (position:
// fixed; inset:0; NO drag-handle/resize — the sheet prop suppresses `.grip`/
// `.resize`) and ALL other windows (design §D: "Chips für alle übrigen") as chips
// in the minbar. The sheet-z (--z-window:350) covers the nav-drawer toggle
// (z 200/250) by design, so the titlebar minimize/close are the documented ONLY
// way back — the gate below proves they stay reachable & clickable.
test.describe('graph floating windows — mobile sheet (G6)', () => {
  test.beforeEach(async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 }) // phone (< SM=640)
  })

  test('top window renders as a full-bleed sheet (inset:0); minbar shows chips', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await enterFocusStage(page)
    await openWindow(page, NODE2)
    await openWindow(page, NODE3)

    // Mobile: EXACTLY one window is rendered (the top one), as a sheet. The other
    // is a chip, not a dialog.
    const sheet = page.locator('.window.sheet')
    await expect(sheet).toHaveCount(1)
    await expect(page.getByRole('dialog')).toHaveCount(1)
    await expect(sheet).toHaveAttribute('aria-modal', 'false')

    // Computed full-bleed: position:fixed + inset 0 on every edge.
    const inset = await sheet.evaluate((el) => {
      const cs = getComputedStyle(el)
      return { position: cs.position, top: cs.top, right: cs.right, bottom: cs.bottom, left: cs.left }
    })
    expect(inset.position).toBe('fixed')
    expect(inset.top).toBe('0px')
    expect(inset.right).toBe('0px')
    expect(inset.bottom).toBe('0px')
    expect(inset.left).toBe('0px')
    const box = await sheet.boundingBox()
    expect(box!.x).toBeLessThanOrEqual(1)
    expect(box!.y).toBeLessThanOrEqual(1)
    expect(box!.width).toBeGreaterThanOrEqual(388) // fills the 390-wide viewport

    // Minbar visible with a chip for the other (non-top) window.
    const minbar = page.locator('.minbar')
    await expect(minbar).toBeVisible()
    await expect(minbar.locator('.chip')).toHaveCount(1)
  })

  test('sheet has NO drag-handle and NO resize affordance (negative)', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await enterFocusStage(page)
    await openWindow(page, NODE2)

    const sheet = page.locator('.window.sheet')
    await expect(sheet).toHaveCount(1)
    // The `sheet` prop drops both out of the DOM ({#if !sheet}).
    await expect(sheet.locator('.grip')).toHaveCount(0)
    await expect(sheet.locator('.resize')).toHaveCount(0)
  })

  test('minimize and close stay reachable in the sheet titlebar (the only way back)', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await enterFocusStage(page)
    await openWindow(page, NODE2)

    const sheet = page.locator('.window.sheet')
    // Both affordances are in the titlebar, above the sheet-z that covers the
    // drawer toggle — they must be visible and actually clickable.
    await expect(sheet.getByRole('button', { name: 'minimize' })).toBeVisible()
    await sheet.getByRole('button', { name: 'close' }).click()
    await expect(page.getByRole('dialog')).toHaveCount(0)
  })

  test('minimize collapses the sheet to a chip; tapping the chip restores it as the sheet', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await enterFocusStage(page)
    await openWindow(page, NODE2) // 'API Spec', z=1
    await openWindow(page, NODE3) // 'Findings', z=2 → top → sheet

    const sheet = page.locator('.window.sheet')
    const chips = page.locator('.minbar .chip')
    // Sheet = NODE3 ('Findings'); the lone chip = NODE2 ('API Spec').
    await expect(chips).toHaveText(['API Spec'])

    // Minimize the sheet (NODE3) → topId falls back to NODE2, which becomes the
    // new sheet; NODE3 ('Findings') drops to a chip. A sheet is still present.
    await sheet.getByRole('button', { name: 'minimize' }).click()
    await expect(page.locator('.window.sheet')).toHaveCount(1)
    await expect(chips).toHaveText(['Findings'])

    // Tap the 'Findings' chip → restore raises NODE3 back to the top → it is the
    // sheet again, and NODE2 ('API Spec') is now the chip.
    await chips.filter({ hasText: 'Findings' }).click()
    await expect(page.locator('.window.sheet')).toHaveCount(1)
    await expect(chips).toHaveText(['API Spec'])
  })
})
