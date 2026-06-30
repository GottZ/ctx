import { test, expect } from '@playwright/test'
import { seedSession, gotoArea } from './fixtures'

// E0 meta-probe — the fixtures FOUNDATION asserts on itself (design 06 §2.3 / Wellen
// E0 gate). It proves the HARD default: an un-mocked manage action no longer absorbs
// silently into {success:true} (the single highest false-positive risk, Inventur 06
// §1). An invented action `__nope__` must come back as a FAILING envelope so apiFetch
// raises an ApiError (api.ts:103) — while a genuinely mocked action stays green, i.e.
// the default is surgical and does not regress the 34 existing specs.
test.describe('meta: fixtures foundation (E0)', () => {
  test('unmocked manage action surfaces an error; mocked stays green', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    // Land the app so the page-context fetch resolves same-origin THROUGH the route mock.
    await gotoArea(page, '/admin')

    const res = await page.evaluate(async () => {
      const post = async (action: string) => {
        const r = await fetch('/api/manage', {
          method: 'POST',
          headers: { 'content-type': 'application/json' },
          body: JSON.stringify({ action }),
        })
        return { status: r.status, body: (await r.json()) as Record<string, unknown> }
      }
      return { nope: await post('__nope__'), known: await post('tenant-list') }
    })

    // Invented action → loud failure (point 3): success:false + diagnostic error.
    expect(res.nope.body.success).toBe(false)
    expect(res.nope.body.__unmocked).toBe(true)
    expect(String(res.nope.body.error)).toContain('unmocked manage action: __nope__')

    // A genuinely mocked action is untouched — the hard default is surgical.
    expect(res.known.body.success).toBe(true)
    expect(Array.isArray(res.known.body.tenants)).toBe(true)
  })
})
