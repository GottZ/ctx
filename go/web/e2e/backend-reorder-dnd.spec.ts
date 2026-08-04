import { test, expect, type Page, type Route } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors } from './fixtures'

// Backend-pool reorder regression suite (Bug 2026-08-04: "Backends alle
// ausgeblendet bis F5"). Drei belegte Fehlerklassen:
//
// (1) Wire-Vertragsbruch in einer NACHBAR-Karte (disable-profile-list mit
//     roles_blacked_out:null — Go-nil-Slice) crashte die ProfilesCard mitten
//     im Flush und riss die Backend-Tabelle im selben {:else}-Ast mit: initial
//     blieb "loading backend pool…" stehen, nach jedem Reorder-Reload blieb
//     die Tabelle verschwunden. Serverseitig fixt emptyNotNull (render()),
//     clientseitig begrenzt <svelte:boundary> den Blast-Radius — dieser Spec
//     probt die Boundary mit dem ROHEN Live-Payload von damals.
//
// (2) Pointer-Capture-Verlust im Drag: der FLIP-Resort hängt das gezogene
//     <tbody> mid-drag um, das Capture auf dem Handle riss ab und der Drop
//     erreichte die Handle-Listener nie — Ordnung angezeigt, nie committet
//     (Geister-Drag). Fix: move/up/cancel auf window solange ein Drag läuft.
//
// (3) ▲▼-Reorder-Roundtrip über alle Antwort-Timings (Reconcile-load()
//     unmountet die Tabelle via status='loading' — sie muss zurückkommen).

interface PoolMockOpts {
  listDelayMs?: number
  reorderDelayMs?: number
  /** Serve ONE profile whose impact carries null arrays (the live wire break). */
  brokenProfile?: boolean
}

function row(id: string, name: string, priority: number): Record<string, unknown> {
  return {
    id,
    name,
    base_url: `http://10.0.0.1/${name}`,
    protocol: 'openai',
    provider_class: 'generic',
    api_key_ref: '',
    trust: 'trusted',
    locality: 'local',
    scope: '_global',
    roles: ['chat'],
    model_map: {},
    timeouts: {},
    num_ctx: 8192,
    priority,
    enabled: true,
    extra_headers: {},
    extra_body: {},
    limits: {},
    metadata: {},
    disable_profiles: [],
    effective_state: 'active',
    cooldown_remaining_s: 0,
    consecutive_fails: 0,
  }
}

const NAMES: Record<string, string> = {
  'b-alpha': 'alpha-chat',
  'b-beta': 'beta-rerank',
  'b-gamma': 'gamma-embed',
}

const delay = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms))

/** Stateful pool mock: reorder rewrites the order, list serves the ladder. */
async function installPool(page: Page, opts: PoolMockOpts = {}): Promise<{ order: () => string[] }> {
  let order = ['b-alpha', 'b-beta', 'b-gamma']
  await page.route('**/api/manage', async (route: Route) => {
    let body: { action?: string; data?: Record<string, unknown> } | null = null
    try {
      body = route.request().postDataJSON()
    } catch {
      /* fall through */
    }
    if (body?.action === 'backend-list') {
      if (opts.listDelayMs) await delay(opts.listDelayMs)
      return route.fulfill({
        json: {
          success: true,
          backends: order.map((id, i) => row(id, NAMES[id], (order.length - i) * 10)),
        },
      })
    }
    if (body?.action === 'backend-reorder') {
      order = (body.data?.order as string[]) ?? order
      if (opts.reorderDelayMs) await delay(opts.reorderDelayMs)
      return route.fulfill({ json: { success: true } })
    }
    if (opts.brokenProfile && body?.action === 'disable-profile-list') {
      // Byte-shape of the 2026-08-04 live payload: an INACTIVE profile whose
      // append-built impact arrays marshalled as null (Go nil slices).
      return route.fulfill({
        json: {
          success: true,
          profiles: [
            {
              name: 'eject',
              scope: '_global',
              label: 'Eject-Modus',
              description: '',
              active: false,
              reserved: true,
              impact: { backends: null, roles_affected: null, roles_blacked_out: null },
            },
          ],
        },
      })
    }
    return route.fallback()
  })
  return { order: () => order }
}

async function dragRow(page: Page, card: ReturnType<Page['locator']>, name: string, targetId: string, settleMs: number): Promise<void> {
  const handle = card.getByRole('button', { name: `drag to reorder: ${name}` })
  const from = await handle.boundingBox()
  const tbox = await card.locator(`tbody[data-id="${targetId}"]`).boundingBox()
  if (!from || !tbox) throw new Error(`drag geometry missing for ${name}→${targetId}`)
  const hx = from.x + from.width / 2
  const hy = from.y + from.height / 2
  const ty = tbox.y + tbox.height / 2
  await page.mouse.move(hx, hy)
  await page.mouse.down()
  for (let i = 1; i <= 8; i++) {
    await page.mouse.move(hx, hy + ((ty - hy) * i) / 8)
    await page.waitForTimeout(15)
  }
  if (settleMs > 0) await page.waitForTimeout(settleMs)
  await page.mouse.up()
}

test.describe('backend reorder — table survival + drag commit', () => {
  test('broken neighbour card (null impact arrays) never takes the table down', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    const pool = await installPool(page, { brokenProfile: true })
    await gotoArea(page, '/settings/backends')

    const card = page.locator('section.card[aria-label="backend pool editor"]')
    // The table renders DESPITE the neighbour crash (was: loading-stuck).
    await expect(card).toContainText('alpha-chat')
    await expect(card.locator('tbody[data-id]')).toHaveCount(3)
    // The failure is VISIBLE, not silent (boundary band), and it is the
    // profiles band, not the pool one.
    await expect(page.locator('.error[role="alert"]')).toContainText('Abschaltprofile')

    // The reported symptom: reorder → reconcile load() → table must return.
    await card.getByRole('button', { name: 'raise priority: beta-rerank' }).click()
    await expect(card).toContainText('beta-rerank', { timeout: 5000 })
    await expect(card.locator('tbody[data-id]')).toHaveCount(3)
    expect(pool.order()).toEqual(['b-beta', 'b-alpha', 'b-gamma'])
  })

  test('fast drop commits the drag (capture-loss ghost regression)', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    const pool = await installPool(page)
    await gotoArea(page, '/settings/backends')

    const card = page.locator('section.card[aria-label="backend pool editor"]')
    await expect(card).toContainText('alpha-chat')

    // Fast drop: up right after the last crossing, FLIP still animating —
    // exactly the window where the captured handle used to lose the drop.
    await dragRow(page, card, 'beta-rerank', 'b-alpha', 0)

    await expect
      .poll(() => pool.order(), { message: 'drop must commit the reorder', timeout: 5000 })
      .toEqual(['b-beta', 'b-alpha', 'b-gamma'])
    await expect(card.locator('tbody[data-id]')).toHaveCount(3)
    expect(errors, errors.join('\n')).toEqual([])
  })

  for (const [label, opts] of [
    ['fast responses (LAN, inside the FLIP window)', { listDelayMs: 0, reorderDelayMs: 0 }],
    ['slow list (loading window visible)', { listDelayMs: 300, reorderDelayMs: 0 }],
    ['slow reorder (commit lands after FLIP)', { listDelayMs: 0, reorderDelayMs: 300 }],
  ] as const) {
    test(`▲▼ reorder round-trip — ${label}`, async ({ page }) => {
      const errors = trackPageErrors(page)
      await seedSession(page, { role: 'server-admin', theme: 'dark' })
      const pool = await installPool(page, opts)
      await gotoArea(page, '/settings/backends')

      const card = page.locator('section.card[aria-label="backend pool editor"]')
      await expect(card).toContainText('alpha-chat')

      await card.getByRole('button', { name: 'raise priority: beta-rerank' }).click()

      await expect(card).toContainText('beta-rerank', { timeout: 5000 })
      await expect(card.locator('tbody[data-id]')).toHaveCount(3)
      expect(pool.order(), 'wire order committed').toEqual(['b-beta', 'b-alpha', 'b-gamma'])
      expect(errors, errors.join('\n')).toEqual([])
    })
  }
})
