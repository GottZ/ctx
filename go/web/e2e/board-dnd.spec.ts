import { test, expect, type Page, type Route } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors } from './fixtures'

// Behavioural gates for the WRITABLE /board — the drag-and-drop + keyboard status
// transition (design 04 §4.5, wave U08). The halves the visual/contract baselines
// cannot carry:
//   - drag happy: a dragTo lands a PATCH whose body carries the target status (B5);
//   - 409 fault: the optimistic move rolls back (card returns) AND the board wire
//     is re-read (§4.8 registry staleness);
//   - the full KEYBOARD transition (Move button → dialog → target → PATCH), no mouse;
//   - 422 invalid transition: the error is visible, the state stays consistent;
//   - writable:false (default session, foreign project scope): NO drop targets,
//     NO Move affordance, a drag lands NO PATCH (the read-only gate, §5.3).
//
// Writable requires the caller home_scope to equal the project scope (N3
// derivation, lib/workflow/writable.ts). seedSession's tenant-A member is
// home_scope 'home' (≠ the acme:main project) → read-only by default; a per-test
// whoami override raises home_scope to 'acme:main' to unlock the write path.

const PROJECT_ID = '33333333-3333-3333-3333-333333333333' // acme:main (project-list freeze)

function boardCard(status: string, i: number): Record<string, unknown> {
  return {
    id: `${status.replace(/[^a-z]/g, '').padEnd(8, '0').slice(0, 8)}-0000-0000-0000-${String(i).padStart(12, '0')}`,
    scope: 'acme:main',
    type_name: 'issue',
    title: `${status} card ${i}`,
    workflow_status: status,
    updated_at: '2026-07-03T00:00:00Z',
  }
}

/** Raise the member home_scope to acme:main so the project scope is writable
 * (N3). Registered AFTER seedSession so it wins the whoami route. */
async function makeWritable(page: Page): Promise<void> {
  await page.route('**/api/whoami', (route) =>
    route.fulfill({
      status: 200,
      json: {
        success: true,
        label: 'smoke-key',
        home_scope: 'acme:main',
        read_scopes: ['acme:main', 'shared'],
        api_key_id: '0190000000007000800000000000ke7',
        tenant_id: '550e8400-e29b-41d4-a716-446655440aaa',
        tenant_slug: 'acme',
        tenant_display_name: 'Acme Corp',
        admin: false,
        role: 'member',
      },
    }),
  )
}

/** GET /api/types with a workflow config (states + terminal). */
async function routeTypes(route: Route, states: string[], terminal: string[]): Promise<void> {
  if (route.request().method() !== 'GET') return route.fallback()
  return route.fulfill({
    status: 200,
    json: {
      success: true,
      types: [
        {
          id: '77777777-7777-7777-7777-777777777777',
          name: 'issue',
          scope: '_global',
          display_name: 'Issue',
          description: '',
          builtin: true,
          is_default: false,
          source: 'builtin',
          created_at: '2026-07-03T00:00:00Z',
          updated_at: '2026-07-03T00:00:00Z',
          config: { v: 1, workflow: { states, initial: states[0], terminal } },
        },
      ],
    },
  })
}

/** Fulfil GET …/board with the given columns; counts board GETs so the wire
 * re-read (§4.8) is assertable. Returns the counter. */
function routeBoardCounting(page: Page, columns: unknown[]): { count(): number } {
  const state = { n: 0 }
  void page.route(`**/api/project/${PROJECT_ID}/board*`, async (route) => {
    if (route.request().method() !== 'GET') return route.fallback()
    state.n += 1
    return route.fulfill({ status: 200, json: { success: true, render: 'untrusted', columns } })
  })
  return { count: () => state.n }
}

/** Two open cards, an empty review column, a terminal done column. */
function threeColumns(): unknown[] {
  return [
    { status: 'open', count: 2, cursor: null, issues: [boardCard('open', 0), boardCard('open', 1)] },
    { status: 'review', count: 0, cursor: null, issues: [] },
    { status: 'done', count: 0, cursor: null, issues: [] },
  ]
}

const OPEN_CARD_0 = boardCard('open', 0).id as string

/** An issue-mutate echo for a PATCH that lands `status`. */
function mutateEcho(blockId: string, status: string): Record<string, unknown> {
  return {
    success: true,
    render: 'untrusted',
    issue: {
      id: blockId,
      category: 'issue',
      tags: [],
      title: 'open card 0',
      content: '',
      metadata: {},
      scope: 'acme:main',
      type: 'issue',
      workflow_status: status,
      created_at: '2026-07-03T00:00:00Z',
      updated_at: '2026-07-04T00:00:00Z',
    },
  }
}

test.describe('board DnD — drag happy path (U08)', () => {
  test('a drop onto another column PATCHes with the target status (B5)', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark' })
    await makeWritable(page)
    await page.route('**/api/types', (r) => routeTypes(r, ['open', 'review', 'done'], ['done']))
    routeBoardCounting(page, threeColumns())

    let patchBody: unknown = null
    await page.route(`**/api/project/${PROJECT_ID}/issues/*`, async (route) => {
      if (route.request().method() !== 'PATCH') return route.fallback()
      patchBody = route.request().postDataJSON()
      return route.fulfill({ status: 200, json: mutateEcho(OPEN_CARD_0, 'review') })
    })

    await gotoArea(page, '/board?scope=acme:main')

    const open = page.locator('[data-board-column][data-status="open"]')
    const review = page.locator('[data-board-column][data-status="review"]')
    // Writable board: the columns are drop targets, the cards carry a Move grip.
    await expect(open).toHaveAttribute('data-droppable', '')
    const card = open.locator(`[data-board-card]`).first()
    await card.dragTo(review, { targetPosition: { x: 40, y: 40 } })

    // The PATCH landed with the target status in the body (B5).
    await expect.poll(() => patchBody).toEqual({ status: 'review' })
    // Optimistic move: the card now lives in review.
    await expect(review.locator('[data-board-card]')).toHaveCount(1)
    expect(errors, errors.join('\n')).toEqual([])
  })
})

test.describe('board DnD — 409 rollback + wire re-read (U08)', () => {
  test('a 409 rolls the card back AND re-reads the board wire', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark' })
    await makeWritable(page)
    await page.route('**/api/types', (r) => routeTypes(r, ['open', 'review', 'done'], ['done']))
    const board = routeBoardCounting(page, threeColumns())

    await page.route(`**/api/project/${PROJECT_ID}/issues/*`, async (route) => {
      if (route.request().method() !== 'PATCH') return route.fallback()
      return route.fulfill({ status: 409, json: { success: false, error: 'transition not allowed' } })
    })

    await gotoArea(page, '/board?scope=acme:main')
    expect(board.count()).toBe(1) // initial load

    const open = page.locator('[data-board-column][data-status="open"]')
    const review = page.locator('[data-board-column][data-status="review"]')
    await open.locator('[data-board-card]').first().dragTo(review, { targetPosition: { x: 40, y: 40 } })

    // Error surfaced.
    await expect(page.locator('[data-transition-error]')).toContainText('transition not allowed')
    // Rolled back: the card is back in open, review is empty again.
    await expect(open.locator('[data-board-card]')).toHaveCount(2)
    await expect(review.locator('[data-board-card]')).toHaveCount(0)
    // Wire re-read fired (§4.8): the board GET ran a second time.
    await expect.poll(() => board.count()).toBe(2)
    expect(errors, errors.join('\n')).toEqual([])
  })
})

test.describe('board DnD — keyboard transition (U08)', () => {
  test('a complete status change with the keyboard only (Move dialog → target → PATCH)', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark' })
    await makeWritable(page)
    await page.route('**/api/types', (r) => routeTypes(r, ['open', 'review', 'done'], ['done']))
    routeBoardCounting(page, threeColumns())

    let patchBody: unknown = null
    await page.route(`**/api/project/${PROJECT_ID}/issues/*`, async (route) => {
      if (route.request().method() !== 'PATCH') return route.fallback()
      patchBody = route.request().postDataJSON()
      return route.fulfill({ status: 200, json: mutateEcho(OPEN_CARD_0, 'review') })
    })

    await gotoArea(page, '/board?scope=acme:main')

    const open = page.locator('[data-board-column][data-status="open"]')
    // Focus the first card's Move button and drive the whole flow by keyboard.
    await open.locator('[data-move-trigger]').first().focus()
    await page.keyboard.press('Enter') // opens the Move dialog
    const dialog = page.locator('[data-move-dialog]')
    await expect(dialog).toBeVisible()
    // The first target (review) is autofocused; Enter selects it.
    await expect(dialog.locator('[data-move-target="review"]')).toBeVisible()
    await page.keyboard.press('Enter')

    await expect.poll(() => patchBody).toEqual({ status: 'review' })
    const review = page.locator('[data-board-column][data-status="review"]')
    await expect(review.locator('[data-board-card]')).toHaveCount(1)
    expect(errors, errors.join('\n')).toEqual([])
  })
})

test.describe('board DnD — invalid transition 422 (U08)', () => {
  test('a 422 keeps the error visible and the state consistent', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark' })
    await makeWritable(page)
    await page.route('**/api/types', (r) => routeTypes(r, ['open', 'review', 'done'], ['done']))
    routeBoardCounting(page, threeColumns())

    await page.route(`**/api/project/${PROJECT_ID}/issues/*`, async (route) => {
      if (route.request().method() !== 'PATCH') return route.fallback()
      return route.fulfill({ status: 422, json: { success: false, error: 'not a valid transition' } })
    })

    await gotoArea(page, '/board?scope=acme:main')
    const open = page.locator('[data-board-column][data-status="open"]')
    await open.locator('[data-move-trigger]').first().focus()
    await page.keyboard.press('Enter')
    await page.locator('[data-move-target="review"]').click()

    await expect(page.locator('[data-transition-error]')).toContainText('not a valid transition')
    // Consistent: the card is still in open (rolled back), review still empty.
    await expect(open.locator('[data-board-card]')).toHaveCount(2)
    await expect(page.locator('[data-board-column][data-status="review"]').locator('[data-board-card]')).toHaveCount(0)
    expect(errors, errors.join('\n')).toEqual([])
  })
})

test.describe('board DnD — writable:false read-only gate (U08)', () => {
  test('a foreign-scope board offers NO drop targets and accepts NO drop', async ({ page }) => {
    const errors = trackPageErrors(page)
    // Default session: home_scope 'home' ≠ the acme:main project ⇒ read-only.
    await seedSession(page, { role: 'member', theme: 'dark' })
    await page.route('**/api/types', (r) => routeTypes(r, ['open', 'review', 'done'], ['done']))
    routeBoardCounting(page, threeColumns())

    let patched = false
    await page.route(`**/api/project/${PROJECT_ID}/issues/*`, async (route) => {
      if (route.request().method() !== 'PATCH') return route.fallback()
      patched = true
      return route.fulfill({ status: 200, json: mutateEcho(OPEN_CARD_0, 'review') })
    })

    await gotoArea(page, '/board?scope=acme:main')
    // No drop targets, no Move affordance — the U07 read-only board.
    await expect(page.locator('[data-board-column][data-droppable]')).toHaveCount(0)
    await expect(page.locator('[data-move-trigger]')).toHaveCount(0)

    // A drag attempt lands NO PATCH (the board does not accept the drop).
    const open = page.locator('[data-board-column][data-status="open"]')
    const review = page.locator('[data-board-column][data-status="review"]')
    await open.locator('[data-board-card]').first().dragTo(review, { targetPosition: { x: 40, y: 40 } })
    await page.waitForTimeout(200)
    expect(patched).toBe(false)
    expect(errors, errors.join('\n')).toEqual([])
  })
})
