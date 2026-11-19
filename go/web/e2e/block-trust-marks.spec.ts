import { test, expect, type Page, type Route } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors } from './fixtures'

// #295, web part: a block of a type marked retrieval.untrusted carries
// `untrusted` (V-11), a block re-typed out of such a type carries
// `untrusted_origin` (#280) — /api/search and `manage get` answer both. The
// block workbench shows them on the hit row and in the detail, the graph
// search box on its hits, the graph node window next to the text and the
// guard review on both blocks of a pair; a block without them shows no mark.

const ORIGIN = '550e8400-e29b-41d4-a716-446655440001' // the graph fixture's focus node
const FOREIGN = '550e8400-e29b-41d4-a716-4466554402a1'
const PLAIN = '550e8400-e29b-41d4-a716-4466554402a2'
const BOTH = '550e8400-e29b-41d4-a716-4466554402a3' // re-typed back into an untrusted type

const TRUST: Record<string, { untrusted?: boolean; untrusted_origin?: string }> = {
  [ORIGIN]: { untrusted_origin: 'session' },
  [FOREIGN]: { untrusted: true },
  [PLAIN]: {},
  [BOTH]: { untrusted: true, untrusted_origin: 'session' },
}
const TITLES: Record<string, string> = {
  [ORIGIN]: 'Re-typed distillate',
  [FOREIGN]: 'Captured tool output',
  [PLAIN]: 'First-party note',
  [BOTH]: 'Re-typed back',
}
const IDS = [ORIGIN, FOREIGN, PLAIN, BOTH]

function hit(id: string): Record<string, unknown> {
  return {
    id,
    category: 'learnings',
    tags: [],
    title: TITLES[id],
    content_preview: `preview of ${TITLES[id]}`,
    content_length: 42,
    scope: 'home',
    sensitivity: 'internal',
    updated_at: '2026-11-18T10:00:00Z',
    created_at: '2026-11-18T09:00:00Z',
    ...TRUST[id],
  }
}

/** Serve the four blocks on /api/search and `manage get`; everything else falls through. */
async function installTrustCorpus(page: Page): Promise<void> {
  await page.route('**/api/search', (route: Route) =>
    route.fulfill({
      json: { success: true, count: IDS.length, compact: true, results: IDS.map(hit), next_after: null },
    }),
  )
  await page.route('**/api/manage', (route: Route) => {
    let body: { action?: string; id?: string } | null = null
    try {
      body = route.request().postDataJSON()
    } catch {
      /* fall through */
    }
    if (body?.action === 'guard-list') {
      // One flagged block (foreign text) whose duplicate is the re-typed one.
      return route.fulfill({
        json: {
          success: true,
          count: 1,
          blocks: [
            {
              id: FOREIGN,
              title: TITLES[FOREIGN],
              category: 'learnings',
              scope: 'home',
              type_name: 'session',
              guard_status: 'needs_review',
              similarity: '0.97',
              matched_id: ORIGIN,
              matched_title: TITLES[ORIGIN],
              checked_at: '2026-11-18T10:00:00Z',
              updated_at: '2026-11-18T10:00:00Z',
            },
          ],
        },
      })
    }
    if (body?.action === 'get' && body.id !== undefined && body.id in TRUST) {
      return route.fulfill({
        json: {
          success: true,
          graph_visible: true,
          block: {
            id: body.id,
            category: 'learnings',
            tags: [],
            title: TITLES[body.id],
            content: `body of ${TITLES[body.id]}`,
            metadata: null,
            scope: 'home',
            sensitivity: 'internal',
            created_at: '2026-11-18T09:00:00Z',
            updated_at: '2026-11-18T10:00:00Z',
            ...TRUST[body.id],
          },
        },
      })
    }
    return route.fallback()
  })
}

test.describe('#295 foreign-text marks on blocks and hits', () => {
  test('the block workbench marks the hit rows and the detail', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await installTrustCorpus(page)
    await gotoArea(page, '/blocks')

    const row = (id: string) => page.locator('ul.results li', { hasText: TITLES[id] })
    await expect(row(PLAIN)).toBeVisible()

    const origin = row(ORIGIN).locator('[data-trust="origin"]')
    await expect(origin).toHaveText('untrusted origin: session')
    await expect(origin).toHaveAttribute(
      'title',
      'untrusted_origin:session (agent-produced text: re-typed out of a type marked untrusted)',
    )
    await expect(row(ORIGIN).locator('[data-trust="untrusted"]')).toHaveCount(0)
    await expect(row(FOREIGN).locator('[data-trust="untrusted"]')).toHaveText('untrusted')
    await expect(row(FOREIGN).locator('[data-trust="origin"]')).toHaveCount(0)
    await expect(row(PLAIN).locator('[data-trust]')).toHaveCount(0)

    await row(ORIGIN).getByRole('button').first().click()
    const detail = page.locator('dl.meta', { hasText: ORIGIN })
    await expect(detail.locator('[data-trust="origin"]')).toHaveText('untrusted origin: session')

    await row(PLAIN).getByRole('button').first().click()
    await expect(page.locator('dl.meta', { hasText: PLAIN })).toBeVisible()
    await expect(page.locator('dl.meta', { hasText: PLAIN }).locator('[data-trust]')).toHaveCount(0)

    await row(FOREIGN).getByRole('button').first().click()
    await expect(page.locator('dl.meta', { hasText: FOREIGN }).locator('[data-trust="untrusted"]')).toHaveText('untrusted')

    // Both marks, untrusted first, apart from each other: a gap on the same
    // line, or a row gap when the narrow detail column wraps the second one.
    await row(BOTH).getByRole('button').first().click()
    const marks = page.locator('dl.meta', { hasText: BOTH }).locator('[data-trust]')
    await expect(marks).toHaveText(['untrusted', 'untrusted origin: session'])
    const [a, b] = [await marks.nth(0).boundingBox(), await marks.nth(1).boundingBox()]
    if (!a || !b) throw new Error('mark geometry missing')
    const apart = b.x - (a.x + a.width) > 0 || b.y - (a.y + a.height) > 0
    expect(apart, `the two marks touch: ${JSON.stringify([a, b])}`).toBe(true)
    expect(errors, errors.join('\n')).toEqual([])
  })

  test('the graph node window and the graph search hits mark foreign text', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await installTrustCorpus(page)
    // ?focus= opens the landing node's window (U03-W2), which loads the block
    // through `manage get`.
    await gotoArea(page, `/graph?focus=${ORIGIN}`)
    const win = page.getByRole('dialog').first()
    await expect(win).toBeVisible()
    await expect(win.locator('[data-trust="origin"]')).toHaveText('untrusted origin: session')

    // The search box lists a content preview per hit — the marks go with it.
    const input = page.locator('input[type="search"]')
    await input.fill('block')
    await input.press('Enter')
    const hitRow = (id: string) => page.locator('.searchbox .results li', { hasText: TITLES[id] })
    await expect(hitRow(PLAIN)).toBeVisible()
    await expect(hitRow(ORIGIN).locator('[data-trust="origin"]')).toHaveText('untrusted origin: session')
    await expect(hitRow(FOREIGN).locator('[data-trust="untrusted"]')).toHaveText('untrusted')
    await expect(hitRow(PLAIN).locator('[data-trust]')).toHaveCount(0)
    expect(errors, errors.join('\n')).toEqual([])
  })

  test('the guard review marks both blocks of a flagged pair', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await installTrustCorpus(page)
    await gotoArea(page, '/guard')

    await page.getByRole('button', { name: TITLES[FOREIGN] }).click()
    const flagged = page.locator('.pane', { hasText: `flagged: ${TITLES[FOREIGN]}` })
    const matched = page.locator('.pane', { hasText: `matched: ${TITLES[ORIGIN]}` })
    await expect(flagged.locator('[data-trust="untrusted"]')).toHaveText('untrusted')
    await expect(flagged.locator('[data-trust="origin"]')).toHaveCount(0)
    await expect(matched.locator('[data-trust="origin"]')).toHaveText('untrusted origin: session')
    await expect(matched.locator('[data-trust="untrusted"]')).toHaveCount(0)
    expect(errors, errors.join('\n')).toEqual([])
  })
})
