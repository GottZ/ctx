import { test, expect } from '@playwright/test'
import { seedSession, gotoArea, sseRoute, trackPageErrors } from './fixtures'

// Live-update wire gates (design 04 §7-U13, wave U13) — the REAL-transport half
// the DOM-free vitest LiveSource gates cannot carry: a member SSE domain-event
// frame off /api/project/events drives an actual refetch GET, and an SSE outage
// leaves the surface green (the poll fallback path). Frame SEMANTICS (bulk = one
// refetch, debounce = one refetch, poll cadence) are the deterministic vitest
// gates (src/lib/workflow/live.test.ts); THIS spec pins the browser round-trip.
//
// Wire truth: the domain frame arrives under SSE event name 'project' with an
// ids-only JSON body {kind:'issue', project_id, op, block_ids} (project_hub.go);
// sseRoute serves it atomically, the app SseClient (Bearer fetch stream) parses
// it, LiveSource debounces, and the page reloads its list head.

const PROJECT_ID = '33333333-3333-3333-3333-333333333333' // acme:main (project-list freeze)

test.describe('live updates — SSE wire (U13)', () => {
  test('a project frame off /api/project/events drives a list refetch GET', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark' })

    // Count the list GETs: the mount fetches page 1 (#1); the SSE frame must
    // trigger a SECOND list GET (#2) — the live refetch. RED: a page that never
    // consumes the stream stays at one GET forever.
    let listGets = 0
    page.on('request', (r) => {
      const u = new URL(r.url())
      if (u.pathname === `/api/project/${PROJECT_ID}/issues` && r.method() === 'GET') listGets += 1
    })

    // Serve one ids-only domain frame on the member event stream (event name
    // 'project', kind = block type). sseRoute wins over the seedSession mocks
    // (later page.route registration takes precedence).
    await sseRoute(page, '**/api/project/events*', [
      { event: 'project', data: { kind: 'issue', project_id: PROJECT_ID, op: 'update', block_ids: ['id-x'] } },
    ])

    await gotoArea(page, '/issues?scope=acme:main')
    // Mount GET landed.
    await expect.poll(() => listGets, { timeout: 10_000 }).toBeGreaterThanOrEqual(1)
    // The SSE frame → debounced (500ms) → a second list GET fires.
    await expect.poll(() => listGets, { timeout: 10_000 }).toBeGreaterThanOrEqual(2)

    expect(errors, errors.join('\n')).toEqual([])
  })

  test('an aborted event stream leaves the list green (poll-fallback path)', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark' })

    // The SSE endpoint dies immediately (network abort) — the SseClient goes to
    // 'error' and the LiveSource poll fallback owns refetching from here. The
    // surface must still render its list; the stream failure never breaks it.
    await page.route('**/api/project/events*', (route) => route.abort())

    await gotoArea(page, '/issues?scope=acme:main')
    await expect(page.locator('main.content .list')).toBeVisible({ timeout: 10_000 })
    expect(errors, errors.join('\n')).toEqual([])
  })
})
