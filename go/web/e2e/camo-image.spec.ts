import { test, expect, type Route } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors } from './fixtures'

// Camo image-proxy behavioural gates (design 07-camo §7.7). The Go security
// policy (SSRF / size-cap / content-type / sig-forgery) is proven in
// internal/camo + internal/handler Go tests; here we prove the FRONTEND wiring
// that the visual baselines cannot carry:
//
//   (a) a foreign image whose URL the server signs is swapped to a proxied
//       same-origin <img src="/api/img/…"> and renders (no placeholder);
//   (b) the viewer's browser NEVER issues a request to the foreign origin — the
//       deanonymization guarantee (E04-9), the whole point of the proxy;
//   (c) when the sign endpoint fails (proxy disabled / rate-limited), the
//       placeholder fallback stays — covered by the existing U06 remote-image
//       test in issue-detail.spec.ts (no sign mock ⇒ signImages resolves empty).
//
// Everything is served from localhost fixtures — no test leaves the host
// (repo doctrine): page.route intercepts /api/img/sign, /api/img/<sig>, and the
// foreign origin (to assert it is never hit).

const PROJECT_ID = '33333333-3333-3333-3333-333333333333' // acme:main (project-list freeze)
const ISSUE_ID = '11111111-1111-1111-1111-111111111111'
const DETAIL_GLOB = `**/api/project/${PROJECT_ID}/issues/*`

const FOREIGN = 'https://images.example/cat.png'
const SIGNED_PATH = '/api/img/testsig?url=https%3A%2F%2Fimages.example%2Fcat.png&exp=9999999999'

// A 1×1 PNG fixture the proxy "fetches" on the server's behalf.
const PNG = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNgAAACAAFUok9rAAAAAElFTkSuQmCC',
  'base64',
)

function detailWithForeignImage(): Record<string, unknown> {
  return {
    success: true,
    render: 'untrusted',
    comments_cursor: null,
    comments: [],
    issue: {
      id: ISSUE_ID,
      category: 'task',
      tags: ['bug'],
      title: 'Example issue',
      content: `Look at this: ![cat](${FOREIGN})`,
      metadata: {},
      scope: 'acme:main',
      type: 'issue',
      workflow_status: 'open',
      created_at: '2026-07-01T00:00:00Z',
      updated_at: '2026-07-03T00:00:00Z',
    },
  }
}

function onlyDetailGet(body: Record<string, unknown>) {
  return async (route: Route) => {
    const u = new URL(route.request().url())
    if (route.request().method() !== 'GET' || u.pathname.endsWith('/comments')) return route.fallback()
    return route.fulfill({ status: 200, json: body })
  }
}

test.describe('camo image proxy — proxied render + deanonymization (design 07 §7.7)', () => {
  test('a signed foreign image renders from /api/img and never hits the foreign origin', async ({ page }) => {
    const errors = trackPageErrors(page)

    // Deanonymization gate: record any request whose HOST is the foreign origin.
    // (A substring match would false-positive on the proxy path, which carries
    // the foreign URL percent-encoded in its ?url= query — that request goes to
    // localhost, which is exactly the point.)
    const foreignHits: string[] = []
    page.on('request', (r) => {
      let host = ''
      try {
        host = new URL(r.url()).host
      } catch {
        /* ignore unparseable */
      }
      if (host === 'images.example') foreignHits.push(r.url())
    })

    await seedSession(page, { role: 'member', theme: 'dark' })

    // MINT: the server signs the foreign URL the renderer asks about.
    let signBody: { urls?: string[] } = {}
    await page.route('**/api/img/sign', async (route) => {
      signBody = JSON.parse(route.request().postData() ?? '{}')
      return route.fulfill({ status: 200, json: { success: true, signatures: { [FOREIGN]: SIGNED_PATH } } })
    })

    // FETCH: the proxied same-origin path serves the (server-fetched) PNG bytes.
    let fetchHits = 0
    await page.route('**/api/img/testsig*', async (route) => {
      fetchHits++
      return route.fulfill({
        status: 200,
        contentType: 'image/png',
        headers: { 'X-Content-Type-Options': 'nosniff', 'Cache-Control': 'public, max-age=86400, immutable' },
        body: PNG,
      })
    })

    await page.route(DETAIL_GLOB, onlyDetailGet(detailWithForeignImage()))

    await gotoArea(page, `/issues/${ISSUE_ID}?scope=acme:main`)
    const content = page.locator('main.content')
    await expect(content.getByRole('heading', { name: 'Example issue' })).toBeVisible()

    // (a) the placeholder is replaced by a proxied same-origin <img> that loads.
    const img = content.locator('img.md-img')
    await expect(img).toBeVisible()
    await expect(img).toHaveAttribute('src', SIGNED_PATH)
    await expect(content.locator('.md-img-blocked')).toHaveCount(0)
    // The browser actually loaded the proxied bytes (naturalWidth > 0).
    await expect
      .poll(async () => img.evaluate((el: HTMLImageElement) => el.complete && el.naturalWidth > 0))
      .toBe(true)
    expect(fetchHits).toBeGreaterThan(0)

    // The renderer asked to sign exactly the foreign URL.
    expect(signBody.urls).toContain(FOREIGN)

    // (b) NO request ever went to the foreign origin.
    await page.waitForTimeout(200)
    expect(foreignHits, `leaked to the foreign origin: ${foreignHits.join(', ')}`).toEqual([])
    expect(errors, errors.join('\n')).toEqual([])
  })
})
