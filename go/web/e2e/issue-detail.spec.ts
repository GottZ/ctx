import { test, expect, type Route } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors } from './fixtures'

// Behavioural gates for /issues/:id (design 04 §4.1/§4.5/§5.5, wave U06) — the
// halves the visual/ARIA/axe baselines cannot carry: the XSS-render WIRING (the
// pipeline itself is proven in lib/markdown/markdown.test.ts incl. the html:true
// gate-self-check — here we prove the DETAIL surface uses it), remote-image
// interception, the uniform-404, the sync-badge ×5+unknown matrix, the deep-link
// read-only gate, the writable composer/mutation happy paths, the 422-visible
// status transition and the 500-comment render budget.
//
// page.route registered AFTER seedSession wins; route.fallback() delegates the
// rest (comment POST → the seedSession commentCreate fixture, etc.).

const PROJECT_ID = '33333333-3333-3333-3333-333333333333' // acme:main (project-list freeze)
const ISSUE_ID = '11111111-1111-1111-1111-111111111111'
const DETAIL_GLOB = `**/api/project/${PROJECT_ID}/issues/*`

/** A detail body. `scope` 'home' = writable for the tenant-A member (home 'home');
 * 'acme:main' = read-only (the deep-link default). */
function detailBody(over: {
  scope?: string
  content?: string
  title?: string
  metadata?: Record<string, unknown>
  comments?: Array<Record<string, unknown>>
}): Record<string, unknown> {
  return {
    success: true,
    render: 'untrusted',
    comments_cursor: null,
    comments: over.comments ?? [],
    issue: {
      id: ISSUE_ID,
      category: 'task',
      tags: ['bug'],
      title: over.title ?? 'Example issue',
      content: over.content ?? '# Example\n\nBody markdown.',
      metadata: over.metadata ?? {},
      scope: over.scope ?? 'home',
      type: 'issue',
      workflow_status: 'open',
      created_at: '2026-07-01T00:00:00Z',
      updated_at: '2026-07-03T00:00:00Z',
    },
  }
}

function comment(content: string, i = 0): Record<string, unknown> {
  return {
    id: `22222222-2222-2222-2222-${String(i).padStart(12, '0')}`,
    category: 'comment',
    content,
    created_at: '2026-07-02T00:00:00Z',
    lifecycle_state: 'active',
    metadata: {},
    scope: 'acme:main',
    sensitivity: 'internal',
    sensitivity_source: 'auto',
    tags: [],
    title: '',
    type: 'comment',
    type_source: 'manual',
    updated_at: '2026-07-02T00:00:00Z',
  }
}

/** Route only the detail GET; delegate everything else (comments, board, PATCH…). */
function onlyDetailGet(body: Record<string, unknown>) {
  return async (route: Route) => {
    const u = new URL(route.request().url())
    if (route.request().method() !== 'GET' || u.pathname.endsWith('/comments')) return route.fallback()
    return route.fulfill({ status: 200, json: body })
  }
}

test.describe('issue detail — markdown XSS wiring (U06 / §5.1)', () => {
  test('an XSS body + comment render as text — no script sink, no handler fires', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark' })
    await page.route(
      DETAIL_GLOB,
      onlyDetailGet(
        detailBody({
          scope: 'acme:main',
          content: 'Body <script>window.__pwned=1</script> [x](javascript:alert(1)) end',
          comments: [comment('Comment <img src=x onerror="window.__pwned=2"> here')],
        }),
      ),
    )

    await gotoArea(page, `/issues/${ISSUE_ID}?scope=acme:main`)
    const content = page.locator('main.content')
    await expect(content.getByRole('heading', { name: 'Example issue' })).toBeVisible()

    // No executable sink: no <script> element, no javascript: anchor, no on* handler
    // fired (the DOMPurify path in lib/markdown neutralised all three).
    expect(await content.locator('script').count()).toBe(0)
    expect(await content.locator('a[href^="javascript:" i]').count()).toBe(0)
    const pwned = await page.evaluate(() => (window as unknown as { __pwned?: number }).__pwned ?? null)
    expect(pwned, 'an event handler / script from the issue body executed').toBeNull()
    // The neutralised markup survives as visible text.
    await expect(content).toContainText('end')
    expect(errors, errors.join('\n')).toEqual([])
  })
})

test.describe('issue detail — remote-image interception (U06 / §4.4.3 / E04-9)', () => {
  test('a foreign image becomes a placeholder and never hits the foreign origin', async ({ page }) => {
    const errors = trackPageErrors(page)
    const foreign: string[] = []
    page.on('request', (r) => {
      if (r.url().includes('evil.example')) foreign.push(r.url())
    })
    await seedSession(page, { role: 'member', theme: 'dark' })
    await page.route(
      DETAIL_GLOB,
      onlyDetailGet(detailBody({ scope: 'acme:main', content: '![shot](https://evil.example/pixel.png)' })),
    )

    await gotoArea(page, `/issues/${ISSUE_ID}?scope=acme:main`)
    const content = page.locator('main.content')
    const ph = content.locator('.md-img-blocked')
    await expect(ph).toBeVisible()
    await expect(ph).toContainText('https://evil.example/pixel.png')
    expect(await content.locator('img').count(), 'no img element must reach the DOM').toBe(0)

    await page.waitForTimeout(300)
    expect(foreign, `a request leaked to the foreign origin: ${foreign.join(', ')}`).toEqual([])
    expect(errors, errors.join('\n')).toEqual([])
  })
})

test.describe('issue detail — uniform 404 (U06 / §5.5)', () => {
  test('an unknown / foreign issue id renders the EmptyState, no crash, no redirect', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark' })
    await page.route(DETAIL_GLOB, async (route) => {
      const u = new URL(route.request().url())
      if (route.request().method() !== 'GET' || u.pathname.endsWith('/comments')) return route.fallback()
      return route.fulfill({ status: 404, json: { success: false, error: 'issue not found' } })
    })

    await gotoArea(page, `/issues/${ISSUE_ID}?scope=acme:main`)
    const content = page.locator('main.content')
    await expect(content).toContainText('Issue not found')
    // No redirect loop: the URL stays on the detail route.
    await expect(page).toHaveURL(/\/issues\/11111111/)
    expect(errors, errors.join('\n')).toEqual([])
  })
})

test.describe('issue detail — sync badge ×5 + unknown fallback (U06 / §5.4)', () => {
  const CASES: Array<[string, string]> = [
    ['local', 'local'],
    ['in_sync', 'in_sync'],
    ['ctx_ahead', 'ctx_ahead'],
    ['forge_ahead', 'forge_ahead'],
    ['conflict', 'conflict'],
    ['garbage', 'unknown'], // off-union → unknown badge in conflict optics (Fixture probe)
  ]
  for (const [value, expected] of CASES) {
    test(`sync_state '${value}' renders the '${expected}' badge`, async ({ page }) => {
      const errors = trackPageErrors(page)
      await seedSession(page, { role: 'member', theme: 'dark' })
      await page.route(
        DETAIL_GLOB,
        onlyDetailGet(detailBody({ scope: 'acme:main', metadata: { sync_state: value } })),
      )

      await gotoArea(page, `/issues/${ISSUE_ID}?scope=acme:main`)
      const badge = page.locator('main.content [data-sync-state]')
      await expect(badge).toHaveAttribute('data-sync-state', expected)
      expect(errors, errors.join('\n')).toEqual([])
    })
  }
})

test.describe('issue detail — deep-link read-only (U06 / §5.3)', () => {
  test('a foreign-scope deep link shows no composer and no mutation control', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark' })
    // Default freeze: the issue lives in acme:main; the member home is 'home' →
    // fail-closed read-only (no wire writable flag; derived from the N3 politik).
    await gotoArea(page, `/issues/${ISSUE_ID}?scope=acme:main`)
    const content = page.locator('main.content')
    await expect(content.getByRole('heading', { name: 'Example issue' })).toBeVisible()
    await expect(content).toContainText('Read-only in this scope')
    await expect(content.getByRole('textbox', { name: 'Add a comment' })).toHaveCount(0)
    await expect(content.getByRole('button', { name: 'Change status' })).toHaveCount(0)
    await expect(content.getByRole('button', { name: 'Edit title' })).toHaveCount(0)
    expect(errors, errors.join('\n')).toEqual([])
  })
})

test.describe('issue detail — writable composer + comment create (U06 / E04-6)', () => {
  test('a writable issue shows the composer; submit POSTs and appends the comment', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark' })
    // Writable: the issue lives in the caller home scope ('home').
    await page.route(DETAIL_GLOB, onlyDetailGet(detailBody({ scope: 'home', comments: [] })))

    await gotoArea(page, `/issues/${ISSUE_ID}?scope=acme:main`)
    const content = page.locator('main.content')
    const composer = content.getByRole('textbox', { name: 'Add a comment' })
    await expect(composer).toBeVisible()

    const post = page.waitForRequest(
      (r) => r.url().includes(`/issues/${ISSUE_ID}/comments`) && r.method() === 'POST',
    )
    await composer.fill('looks good to me')
    await content.getByRole('button', { name: 'Comment' }).click()
    await post
    // The commentCreate fixture body is appended to the thread.
    await expect(content.locator('li[data-comment]')).toContainText('A comment on the issue.')
    expect(errors, errors.join('\n')).toEqual([])
  })
})

test.describe('issue detail — status transition (U06 / §4.5)', () => {
  test('a policy-violating transition (422) stays visible and keeps the selection', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark' })
    await page.route(DETAIL_GLOB, async (route) => {
      const u = new URL(route.request().url())
      const m = route.request().method()
      if (u.pathname.endsWith('/comments')) return route.fallback()
      if (m === 'GET') return route.fulfill({ status: 200, json: detailBody({ scope: 'home' }) })
      if (m === 'PATCH') {
        return route.fulfill({
          status: 422,
          json: { success: false, error: 'transition open→done not allowed', code: 'invalid_transition' },
        })
      }
      return route.fallback()
    })

    await gotoArea(page, `/issues/${ISSUE_ID}?scope=acme:main`)
    const content = page.locator('main.content')
    const select = content.getByRole('combobox', { name: 'Workflow status' })
    await expect(select).toBeVisible()
    await select.selectOption('closed')
    await content.getByRole('button', { name: 'Change status' }).click()

    const dialog = page.getByRole('dialog')
    await expect(dialog).toBeVisible()
    await dialog.getByRole('button', { name: 'Move' }).click()

    // 422 surfaces IN the dialog (role=alert), the dialog stays open, and the
    // page selection is retained (§4.5).
    await expect(dialog).toContainText('not allowed')
    await expect(dialog).toBeVisible()
    await expect(select).toHaveValue('closed')
    expect(errors, errors.join('\n')).toEqual([])
  })

  test('a valid transition applies and closes the dialog', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark' })
    await page.route(DETAIL_GLOB, async (route) => {
      const u = new URL(route.request().url())
      const m = route.request().method()
      if (u.pathname.endsWith('/comments')) return route.fallback()
      if (m === 'GET') return route.fulfill({ status: 200, json: detailBody({ scope: 'home' }) })
      if (m === 'PATCH') {
        return route.fulfill({
          status: 200,
          json: { success: true, render: 'untrusted', issue: detailBody({ scope: 'home' }).issue },
        })
      }
      return route.fallback()
    })

    await gotoArea(page, `/issues/${ISSUE_ID}?scope=acme:main`)
    const content = page.locator('main.content')
    await content.getByRole('combobox', { name: 'Workflow status' }).selectOption('closed')
    await content.getByRole('button', { name: 'Change status' }).click()
    const dialog = page.getByRole('dialog')
    await dialog.getByRole('button', { name: 'Move' }).click()
    await expect(dialog).toHaveCount(0)
    expect(errors, errors.join('\n')).toEqual([])
  })
})

test.describe('issue detail — title edit (U06)', () => {
  test('editing the title PATCHes and renders the new title', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark' })
    await page.route(DETAIL_GLOB, async (route) => {
      const u = new URL(route.request().url())
      const m = route.request().method()
      if (u.pathname.endsWith('/comments')) return route.fallback()
      if (m === 'GET') return route.fulfill({ status: 200, json: detailBody({ scope: 'home' }) })
      if (m === 'PATCH') {
        const body = route.request().postDataJSON() as { title?: string }
        return route.fulfill({
          status: 200,
          json: { success: true, render: 'untrusted', issue: detailBody({ scope: 'home', title: body.title }).issue },
        })
      }
      return route.fallback()
    })

    await gotoArea(page, `/issues/${ISSUE_ID}?scope=acme:main`)
    const content = page.locator('main.content')
    await content.getByRole('button', { name: 'Edit title' }).click()
    const input = content.getByRole('textbox', { name: 'Issue title' })
    await input.fill('Renamed issue')
    const patch = page.waitForRequest(
      (r) => r.url().includes(`/issues/${ISSUE_ID}`) && r.method() === 'PATCH',
    )
    await content.getByRole('button', { name: 'Save title' }).click()
    await patch
    await expect(content.getByRole('heading', { name: 'Renamed issue' })).toBeVisible()
    expect(errors, errors.join('\n')).toEqual([])
  })
})

test.describe('issue detail — 500-comment render budget (U06 / §5.5)', () => {
  test('the surface stays usable with 500 comments; the thread render stays bounded', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark' })
    const many = Array.from({ length: 500 }, (_, k) => comment(`bulk comment ${k}`, k))
    await page.route(DETAIL_GLOB, onlyDetailGet(detailBody({ scope: 'acme:main', comments: many })))

    await gotoArea(page, `/issues/${ISSUE_ID}?scope=acme:main`)
    const content = page.locator('main.content')
    await expect(content.getByRole('heading', { name: 'Example issue' })).toBeVisible()

    // Render budget: 500 loaded, far fewer rendered (progressive-reveal cap).
    const rendered = await content.locator('li[data-comment]').count()
    expect(rendered, 'the thread renders a bounded window, not all 500').toBeLessThanOrEqual(150)
    expect(rendered).toBeGreaterThan(0)
    // The count reflects the full loaded thread.
    await expect(content.getByRole('heading', { name: /Comments \(500\)/ })).toBeVisible()

    // Still bedienbar: "show more" reveals the next batch.
    await content.getByRole('button', { name: /Show more comments/ }).click()
    expect(await content.locator('li[data-comment]').count()).toBeGreaterThan(rendered)
    expect(errors, errors.join('\n')).toEqual([])
  })
})

test.describe('issue detail — status dialog ARIA (U06 / Q9 baseline)', () => {
  test('the open status-transition dialog has a stable accessibility structure', async ({ page }) => {
    await seedSession(page, { role: 'member', theme: 'dark' })
    await page.route(DETAIL_GLOB, onlyDetailGet(detailBody({ scope: 'home', metadata: { sync_state: 'in_sync' } })))

    await gotoArea(page, `/issues/${ISSUE_ID}?scope=acme:main`)
    const content = page.locator('main.content')
    await content.getByRole('combobox', { name: 'Workflow status' }).selectOption('closed')
    await content.getByRole('button', { name: 'Change status' }).click()
    const dialog = page.getByRole('dialog')
    await expect(dialog).toBeVisible()
    // Committed ARIA snapshot of the open dialog (the Q9 dialog-open ARIA
    // baseline; the matching Screenshot is the contract dialog-status state).
    await expect(dialog).toMatchAriaSnapshot({ name: 'issue-detail-dialog-status--aria.yml' })
  })
})
