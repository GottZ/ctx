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
