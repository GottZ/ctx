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
    const vp = await page.locator('.viewport').boundingBox()
    const hit = await page.evaluate(
      ([x, y]) => {
        const el = document.elementFromPoint(x as number, y as number)
        return { tag: el?.tagName ?? '', cls: (el as HTMLElement)?.className ?? '' }
      },
      [vp!.x + vp!.width * 0.72, vp!.y + vp!.height * 0.75],
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
