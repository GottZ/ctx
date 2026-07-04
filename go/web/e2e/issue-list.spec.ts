import { test, expect, type Route } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors } from './fixtures'

// Behavioural gates for the /issues list (design 04 §4.2/§5.5, wave U05) — the
// halves the visual/contract baselines cannot carry: the keyset-append wire
// round-trip, the URL filter round-trip across a reload (incl. ?scope=), the
// search-mode append suppression, and the N-project picker write-back. Each
// overrides the workflow route with a purpose-built body (later page.route wins;
// route.fallback() delegates everything else to the seedSession mocks).

const PROJECT_ID = '33333333-3333-3333-3333-333333333333' // acme:main (project-list freeze)

function issueRow(i: number, page: number): Record<string, unknown> {
  return {
    id: `${page}0000000-0000-0000-0000-${String(i).padStart(12, '0')}`,
    scope: 'acme:main',
    type_name: 'issue',
    title: `P${page} Issue ${i}`,
    workflow_status: i % 2 === 0 ? 'open' : 'in_progress',
    updated_at: new Date(Date.UTC(2026, 6, 3, 12, 0, 0) - (page * 1000 + i) * 60_000).toISOString(),
  }
}

/** Two keyset pages: page 1 (cursor 'CURSOR_P2'), page 2 (cursor null). */
async function routeTwoPages(route: Route): Promise<void> {
  const u = new URL(route.request().url())
  if (!u.pathname.endsWith('/issues') || route.request().method() !== 'GET') return route.fallback()
  const after = u.searchParams.get('after')
  if (after === null) {
    const issues = Array.from({ length: 100 }, (_, k) => issueRow(k, 1))
    return route.fulfill({ status: 200, json: { success: true, render: 'untrusted', issues, cursor: 'CURSOR_P2' } })
  }
  const issues = Array.from({ length: 100 }, (_, k) => issueRow(k, 2))
  return route.fulfill({ status: 200, json: { success: true, render: 'untrusted', issues, cursor: null } })
}

test.describe('issue list — keyset append (U05)', () => {
  test('scrolling to the end loads the next page WITH the keyset cursor', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark' })
    await page.route(`**/api/project/${PROJECT_ID}/issues*`, routeTwoPages)

    await gotoArea(page, '/issues?scope=acme:main')
    await expect(page.getByRole('link', { name: 'P1 Issue 0' })).toBeVisible()

    // Scroll the list to the bottom → onScroll fires the keyset loadMore. The
    // follow-up request MUST carry the opaque cursor as ?after= (the wire proof;
    // RED: a loadMore that drops the cursor re-fetches page 1 forever).
    const p2 = page.waitForRequest(
      (r) => r.url().includes(`/api/project/${PROJECT_ID}/issues`) && r.url().includes('after=CURSOR_P2'),
    )
    await page.locator('main.content .list').evaluate((el) => el.scrollTo(0, el.scrollHeight))
    await p2

    // Page-2 content is now reachable (appended, not replaced): scroll to the end
    // again and a page-2 row renders through the window.
    await page.locator('main.content .list').evaluate((el) => el.scrollTo(0, el.scrollHeight))
    await expect(page.getByRole('link', { name: 'P2 Issue 99' })).toBeVisible()
    expect(errors, errors.join('\n')).toEqual([])
  })
})

test.describe('issue list — URL filter round-trip (U05)', () => {
  test('a reload restores the scope + status filter from the URL', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark' })

    // Deep link carrying scope + status. The status select must reflect it, and
    // the URL must still carry it after a full reload (the reload gate, §5.5).
    await gotoArea(page, '/issues?scope=acme:main&status=open')
    const statusSelect = page.getByRole('combobox', { name: 'Filter by status' })
    await expect(statusSelect).toHaveValue('open')
    await expect(page).toHaveURL(/status=open/)
    await expect(page).toHaveURL(/scope=acme(%3A|:)main/)

    await page.reload()
    await page.locator('.shell').waitFor({ state: 'visible' })
    await expect(page.getByRole('combobox', { name: 'Filter by status' })).toHaveValue('open')
    await expect(page).toHaveURL(/status=open/)
    await expect(page).toHaveURL(/scope=acme(%3A|:)main/)
    expect(errors, errors.join('\n')).toEqual([])
  })
})

test.describe('issue list — search mode (U05)', () => {
  test('search mode renders no load-more affordance, even with a rogue cursor', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark' })
    // Malicious server: returns a NON-null cursor even on the search (q) path.
    await page.route(`**/api/project/${PROJECT_ID}/issues*`, async (route) => {
      const u = new URL(route.request().url())
      if (!u.pathname.endsWith('/issues') || route.request().method() !== 'GET') return route.fallback()
      const issues = Array.from({ length: 8 }, (_, k) => issueRow(k, 1))
      // rogue cursor on BOTH modes — the client must still suppress append in search.
      return route.fulfill({ status: 200, json: { success: true, render: 'untrusted', issues, cursor: 'ROGUE' } })
    })

    await gotoArea(page, '/issues?scope=acme:main')
    // Browse mode with the rogue cursor DOES offer load-more (control).
    await expect(page.getByRole('button', { name: 'Load more' })).toBeVisible()

    // Enter search mode → the affordance must vanish (append is impossible on a
    // ranked result set, §6.1); RED: a list that trusts the cursor keeps it.
    await page.getByRole('searchbox', { name: 'Search issues' }).fill('flaky')
    await page.getByRole('button', { name: 'Search' }).click()
    await expect(page.locator('main.content')).toContainText('Top matches')
    await expect(page.getByRole('button', { name: 'Load more' })).toHaveCount(0)
    expect(errors, errors.join('\n')).toEqual([])
  })
})

test.describe('issue list — project picker (U05)', () => {
  test('N projects: nothing auto-selects; a pick writes ?scope= and loads', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark' })
    // Two projects → the multi picker (no auto-select).
    await page.route('**/api/project', async (route) => {
      if (route.request().method() !== 'GET') return route.fallback()
      return route.fulfill({
        status: 200,
        json: {
          success: true,
          projects: [
            { id: 'id-a', tenant_id: 't', scope: 'acme:main', identity: 'github:acme/main', display_name: 'Acme Main', forge: null, webhook_secret_ref: null, sync_status: 'idle', sync_enabled: true, push_enabled: false, last_sync_at: null, sync_cursor: null, created_at: '2026-07-01T00:00:00Z', metadata: {} },
            { id: 'id-b', tenant_id: 't', scope: 'globex:main', identity: 'github:globex/main', display_name: 'Globex Main', forge: null, webhook_secret_ref: null, sync_status: 'idle', sync_enabled: true, push_enabled: false, last_sync_at: null, sync_cursor: null, created_at: '2026-07-01T00:00:00Z', metadata: {} },
          ],
        },
      })
    })

    await gotoArea(page, '/issues')
    // No scope in the URL + N projects ⇒ nothing selected, the prompt shows.
    await expect(page.locator('main.content')).toContainText('Select a project')
    await expect(page).not.toHaveURL(/scope=/)

    // Picking a project writes ?scope= (URL is the single source of truth, §4.1.5).
    await page.getByRole('combobox', { name: 'Select project' }).selectOption('globex:main')
    await expect(page).toHaveURL(/scope=globex(%3A|:)main/)
    expect(errors, errors.join('\n')).toEqual([])
  })
})
