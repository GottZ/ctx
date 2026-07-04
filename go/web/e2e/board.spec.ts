import { test, expect, type Route } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors } from './fixtures'

// Behavioural gates for the read-only /board (design 04 §4.2/§6.2, wave U07) —
// the halves the visual/contract baselines cannot carry: the unmapped negative
// (a wire status outside the registry survives as a read-only column, never
// crashes / never lost), the wire-order preservation against a PERMUTED fixture,
// the closed-collapse toggle, and the per-column keyset window (count from the
// wire, cards appended). Each overrides the board + registry reads with a
// purpose-built body (later page.route wins; route.fallback() delegates the rest
// to the seedSession mocks).

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

/** Fulfil GET /api/types with a workflow config (states + terminal). The freeze
 * type-list.json has none — the board reads THIS for the open/closed verdict. */
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

/** Fulfil GET …/board with the given columns verbatim (order preserved). */
function routeBoard(columns: unknown[]): (route: Route) => Promise<void> {
  return async (route) => {
    if (route.request().method() !== 'GET') return route.fallback()
    return route.fulfill({ status: 200, json: { success: true, render: 'untrusted', columns } })
  }
}

test.describe('board — unmapped negative gate (U07)', () => {
  test('a wire status outside the registry renders as a read-only unmapped column, no crash', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark' })
    // Registry knows open/done only; the board wire ALSO carries 'on_hold'.
    await page.route('**/api/types', (r) => routeTypes(r, ['open', 'done'], ['done']))
    await page.route(
      `**/api/project/${PROJECT_ID}/board*`,
      routeBoard([
        { status: 'open', count: 1, cursor: null, issues: [boardCard('open', 0)] },
        { status: 'on_hold', count: 2, cursor: null, issues: [boardCard('on_hold', 0), boardCard('on_hold', 1)] },
        { status: 'done', count: 5, cursor: null, issues: [boardCard('done', 0)] },
      ]),
    )

    await gotoArea(page, '/board?scope=acme:main')

    const onHold = page.locator('[data-board-column][data-status="on_hold"]')
    // The unknown status is NOT dropped — it is its own column, badged unmapped.
    await expect(onHold).toHaveAttribute('data-category', 'unmapped')
    await expect(onHold.locator('[data-unmapped]')).toBeVisible()
    // Unmapped is NOT collapsed (open by default) — its (verwaiste) cards show.
    await expect(onHold.locator('[data-board-card]')).toHaveCount(2)
    expect(errors, errors.join('\n')).toEqual([])
  })
})

test.describe('board — wire order gate (U07)', () => {
  test('columns render in the WIRE order even when it is not alphabetical', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark' })
    await page.route('**/api/types', (r) => routeTypes(r, ['open', 'in_progress', 'review', 'done'], ['done']))
    // Deliberately permuted (review, done, open, in_progress) — the DOM must
    // match this exact sequence (RED: a board that sorts columns fails here).
    await page.route(
      `**/api/project/${PROJECT_ID}/board*`,
      routeBoard([
        { status: 'review', count: 0, cursor: null, issues: [] },
        { status: 'done', count: 0, cursor: null, issues: [] },
        { status: 'open', count: 0, cursor: null, issues: [] },
        { status: 'in_progress', count: 0, cursor: null, issues: [] },
      ]),
    )

    await gotoArea(page, '/board?scope=acme:main')
    const statuses = await page
      .locator('[data-board-column]')
      .evaluateAll((els) => els.map((e) => e.getAttribute('data-status')))
    expect(statuses).toEqual(['review', 'done', 'open', 'in_progress'])
    expect(errors, errors.join('\n')).toEqual([])
  })
})

test.describe('board — closed-collapse toggle (U07)', () => {
  test('a terminal column starts collapsed (count visible), expands on toggle', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark' })
    await page.route('**/api/types', (r) => routeTypes(r, ['open', 'done'], ['done']))
    await page.route(
      `**/api/project/${PROJECT_ID}/board*`,
      routeBoard([
        { status: 'open', count: 1, cursor: null, issues: [boardCard('open', 0)] },
        { status: 'done', count: 3, cursor: null, issues: [boardCard('done', 0), boardCard('done', 1), boardCard('done', 2)] },
      ]),
    )

    await gotoArea(page, '/board?scope=acme:main')
    const done = page.locator('[data-board-column][data-status="done"]')
    // Collapsed: count shown, zero cards, toggle aria-expanded=false.
    await expect(done.locator('[data-count]')).toHaveText('3')
    await expect(done.locator('[data-board-card]')).toHaveCount(0)
    await expect(done.getByRole('button')).toHaveAttribute('aria-expanded', 'false')
    // Toggle expands: the three cards now render.
    await done.getByRole('button').click()
    await expect(done.getByRole('button')).toHaveAttribute('aria-expanded', 'true')
    await expect(done.locator('[data-board-card]')).toHaveCount(3)
    expect(errors, errors.join('\n')).toEqual([])
  })
})

test.describe('board — per-column keyset window (U07)', () => {
  test('load-more appends the next page of ONE column with the opaque cursor', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark' })
    await page.route('**/api/types', (r) => routeTypes(r, ['open', 'done'], ['done']))
    // open: count 60, a 2-card first page + a resume cursor.
    await page.route(
      `**/api/project/${PROJECT_ID}/board*`,
      routeBoard([
        { status: 'open', count: 60, cursor: 'idx-1', issues: [boardCard('open', 0), boardCard('open', 1)] },
      ]),
    )
    // The per-column list endpoint (state=open&after=idx-1) serves page 2.
    await page.route(`**/api/project/${PROJECT_ID}/issues*`, async (route) => {
      const u = new URL(route.request().url())
      if (route.request().method() !== 'GET' || !u.pathname.endsWith('/issues')) return route.fallback()
      expect(u.searchParams.get('state'), 'load-more filters by the column status').toBe('open')
      expect(u.searchParams.get('after'), 'load-more carries the opaque cursor').toBe('idx-1')
      return route.fulfill({
        status: 200,
        json: { success: true, render: 'untrusted', issues: [boardCard('open', 2), boardCard('open', 3)], cursor: null },
      })
    })

    await gotoArea(page, '/board?scope=acme:main')
    const open = page.locator('[data-board-column][data-status="open"]')
    await expect(open.locator('[data-board-card]')).toHaveCount(2)
    // The wire count stays 60 (B7), independent of the loaded card length.
    await expect(open.locator('[data-count]')).toHaveText('60')

    await open.getByRole('button', { name: /Load more/ }).click()
    await expect(open.locator('[data-board-card]')).toHaveCount(4)
    await expect(open.locator('[data-count]')).toHaveText('60')
    expect(errors, errors.join('\n')).toEqual([])
  })
})
