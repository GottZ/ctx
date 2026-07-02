import { test, expect } from '@playwright/test'
import { seedSession, gotoArea, sseRoute } from './fixtures'

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

  // PV2 self-probe (a), apiFetch class (design 06 §4.6): an un-mocked /api/**
  // ENDPOINT (not just a manage action) must fail loudly. Before PV2 the
  // catch-all answered 200 {success:true} (seam S5) — proven red in the wave
  // gate. Status 599 is non-2xx, so apiFetch throws via !res.ok (api.ts:99-101)
  // before it ever reaches the envelope check.
  test('unmocked /api/** endpoint hard-fails with 599; mocked endpoint stays green', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/admin')

    const res = await page.evaluate(async () => {
      const get = async (path: string) => {
        const r = await fetch(path)
        return { ok: r.ok, status: r.status, body: (await r.json()) as Record<string, unknown> }
      }
      return { nope: await get('/api/__nope__'), known: await get('/api/status') }
    })

    // Invented endpoint → loud non-2xx failure with the diagnostic envelope.
    expect(res.nope.ok).toBe(false)
    expect(res.nope.status).toBe(599)
    expect(res.nope.body.__unmocked).toBe(true)
    expect(res.nope.body.success).toBe(false)
    expect(String(res.nope.body.error)).toContain('unmocked endpoint: GET /api/__nope__')

    // A genuinely mocked endpoint is untouched — the hard-fail is surgical.
    expect(res.known.ok).toBe(true)
    expect(res.known.body.success).toBe(true)
  })

  // PV2 self-probe (b), streaming class (design 06 §4.6): an un-mocked stream
  // POST must surface as a THROWN pre-stream error, not resolve as an empty
  // turn. Before PV2 the 200 {success:true} catch-all passed streamTurn's only
  // gate (!res.ok || !res.body, stream.ts:44-48), the eventsource-parser
  // dropped the JSON body frame-less and the turn resolved silently empty —
  // proven red in the wave gate. The probe replicates that exact gate and
  // asserts on the ApiError path, not on event absence.
  test('unmocked stream POST is a loud pre-stream error, not an empty turn', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/admin')

    const res = await page.evaluate(async () => {
      const r = await fetch('/api/chat/stream', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Accept: 'text/event-stream' },
        body: JSON.stringify({ message: 'probe' }),
      })
      // Exactly streamTurn's pre-stream gate (stream.ts:44-48): non-ok → read
      // the JSON error body and throw ApiError; ok → the turn would stream.
      if (!r.ok || !r.body) {
        const body = (await r.json().catch(() => null)) as { error?: string } | null
        return { threw: true, status: r.status, message: body?.error ?? `chat stream failed (HTTP ${r.status})` }
      }
      return { threw: false, status: r.status, message: '' }
    })

    expect(res.threw).toBe(true) // the ApiError path, never the empty-turn path
    expect(res.status).toBe(599)
    expect(res.message).toContain('unmocked endpoint: POST /api/chat/stream')
  })

  // PV2 building block: sseRoute serves deterministic text/event-stream frames
  // and takes precedence over the 599 catch-all (later page.route registrations
  // win), so streaming flows become mock-testable (chat primaryFlow in PV7,
  // board events for Achse 03).
  test('sseRoute overrides the catch-all with parseable event-stream frames', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await sseRoute(page, '**/api/chat/stream', [
      { event: 'token', data: { text: 'Hal' } },
      { event: 'token', data: { text: 'lo' } },
      { event: 'done', data: { finish: 'stop' } },
    ])
    await gotoArea(page, '/admin')

    const res = await page.evaluate(async () => {
      const r = await fetch('/api/chat/stream', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', Accept: 'text/event-stream' },
        body: JSON.stringify({ message: 'probe' }),
      })
      return { ok: r.ok, contentType: r.headers.get('content-type') ?? '', text: await r.text() }
    })

    expect(res.ok).toBe(true) // passes streamTurn's pre-stream gate
    expect(res.contentType).toContain('text/event-stream')
    // Frame format is what eventsource-parser consumes: event: + data: + blank line.
    expect(res.text).toBe(
      'event: token\ndata: {"text":"Hal"}\n\n' +
        'event: token\ndata: {"text":"lo"}\n\n' +
        'event: done\ndata: {"finish":"stop"}\n\n',
    )
  })
})
