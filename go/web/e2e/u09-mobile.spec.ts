import { test, expect, type Route } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors } from './fixtures'

// Behavioural gates for U09 (design 04 §7-U09 / §5.5-Mobile): the three seams
// the visual/ARIA baselines cannot carry on their own —
//   1. /issues/:id renders as a G6-sheet below SM (inset:0 BEHAVIOUR, not mere
//      visibility — the negative probe is the desktop in-flow render, which is
//      position:static and fails the fixed+inset assert; Muster graph-windows G6).
//   2. the board becomes a single-column PAGER at 390 (one column visible,
//      prev/next navigate, the indicator tracks position).
//   3. DESKTOP: a board card click opens the issue detail as a floating window
//      (lib/windows content-snippet) with NO navigation loss.
//
// page.route registered AFTER seedSession wins; route.fallback() delegates the rest.

const PROJECT_ID = '33333333-3333-3333-3333-333333333333' // acme:main (project-list freeze)
const ISSUE_ID = '11111111-1111-1111-1111-111111111111'

function detailBody(over: { title?: string; scope?: string } = {}): Record<string, unknown> {
  return {
    success: true,
    render: 'untrusted',
    comments_cursor: null,
    comments: [],
    issue: {
      id: over.title === undefined ? ISSUE_ID : 'open0000-0000-0000-0000-000000000000',
      category: 'task',
      tags: ['bug'],
      title: over.title ?? 'Example issue',
      content: '# Example\n\nBody markdown.',
      metadata: {},
      scope: over.scope ?? 'acme:main',
      type: 'issue',
      workflow_status: 'open',
      created_at: '2026-07-01T00:00:00Z',
      updated_at: '2026-07-03T00:00:00Z',
    },
  }
}

/** Route only the detail GET; delegate the rest (comments, board, types…). */
function onlyDetailGet(body: Record<string, unknown>) {
  return async (route: Route) => {
    const u = new URL(route.request().url())
    if (route.request().method() !== 'GET' || u.pathname.endsWith('/comments')) return route.fallback()
    return route.fulfill({ status: 200, json: body })
  }
}

// ---- Gate 1: /issues/:id G6-sheet < SM --------------------------------------
test.describe('U09 — issue detail G6-sheet (390)', () => {
  test.use({ viewport: { width: 390, height: 844 } })

  test('the detail surface renders as a full-bleed sheet (position:fixed; inset:0)', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark' })
    await page.route(`**/api/project/${PROJECT_ID}/issues/*`, onlyDetailGet(detailBody()))

    await gotoArea(page, `/issues/${ISSUE_ID}?scope=acme:main`)
    const root = page.locator('[data-detail-root]')
    await expect(page.locator('main.content').getByRole('heading', { name: 'Example issue' })).toBeVisible()

    // The behavioural contract (Muster graph-windows G6): computed full-bleed —
    // position:fixed + inset 0 on EVERY edge. A desktop in-flow render (the RED
    // baseline before the @media sheet rule) is position:static and fails here.
    const inset = await root.evaluate((el) => {
      const cs = getComputedStyle(el)
      return { position: cs.position, top: cs.top, right: cs.right, bottom: cs.bottom, left: cs.left }
    })
    expect(inset.position).toBe('fixed')
    expect(inset.top).toBe('0px')
    expect(inset.right).toBe('0px')
    expect(inset.bottom).toBe('0px')
    expect(inset.left).toBe('0px')

    const box = await root.boundingBox()
    expect(box!.x).toBeLessThanOrEqual(1)
    expect(box!.y).toBeLessThanOrEqual(1)
    expect(box!.width).toBeGreaterThanOrEqual(388) // fills the 390-wide viewport
    expect(errors, errors.join('\n')).toEqual([])
  })
})

// ---- Gate 2: board column pager @390 ----------------------------------------
test.describe('U09 — board column pager (390)', () => {
  test.use({ viewport: { width: 390, height: 844 } })

  test('one column at a time; prev/next navigate; the indicator tracks position', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark', state: 'board' })
    await gotoArea(page, '/board?scope=acme:main')

    const pager = page.locator('[data-column-pager]')
    await expect(pager).toBeVisible()
    // Exactly ONE column is mounted (the pager page), not the 4-column row.
    await expect(page.locator('[data-board-column]')).toHaveCount(1)
    await expect(page.locator('[data-board-column]')).toHaveAttribute('data-status', 'open')

    const indicator = page.locator('[data-pager-indicator]')
    await expect(indicator).toContainText('1 / 4')
    // At the first page prev is disabled, next is live.
    await expect(page.locator('[data-pager-prev]')).toBeDisabled()

    // Next → the SECOND column (in_progress), indicator advances.
    await page.locator('[data-pager-next]').click()
    await expect(page.locator('[data-board-column]')).toHaveAttribute('data-status', 'in_progress')
    await expect(indicator).toContainText('2 / 4')
    await expect(page.locator('[data-board-column]')).toHaveCount(1)

    // Prev → back to the first column.
    await page.locator('[data-pager-prev]').click()
    await expect(page.locator('[data-board-column]')).toHaveAttribute('data-status', 'open')
    await expect(indicator).toContainText('1 / 4')
    expect(errors, errors.join('\n')).toEqual([])
  })
})

// ---- Gate 3: desktop board card → floating window (interop) -----------------
test.describe('U09 — board card opens a detail window (desktop interop)', () => {
  test('a card click opens the issue detail as a window; the board stays mounted', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark', state: 'board' })
    // The window loads the clicked card via getIssue — serve it.
    await page.route(
      `**/api/project/${PROJECT_ID}/issues/*`,
      onlyDetailGet(detailBody({ title: 'Windowed issue' })),
    )
    await gotoArea(page, '/board?scope=acme:main')

    const open = page.locator('[data-board-column][data-status="open"]')
    const card = open.locator('[data-board-card]').first()
    await expect(card).toBeVisible()
    await card.click()

    // A floating window (role=dialog) opens with the detail content …
    const win = page.getByRole('dialog')
    await expect(win).toHaveCount(1)
    await expect(win.getByRole('heading', { name: 'Windowed issue' })).toBeVisible()
    // … and the board did NOT navigate away (no navigation loss — still /board).
    await expect(page).toHaveURL(/\/board/)
    await expect(page.getByRole('heading', { name: 'Board' })).toBeVisible()

    // Close returns to the board (window gone).
    await win.getByRole('button', { name: 'close' }).click()
    await expect(page.getByRole('dialog')).toHaveCount(0)
    expect(errors, errors.join('\n')).toEqual([])
  })
})
