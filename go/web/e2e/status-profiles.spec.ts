import { test, expect, type Page, type Route } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors } from './fixtures'

// StatusPage profile quick-toggles (092, Web-UX U01-W7; design/01 §4.5/§4.6).
// Two gates, each red against the pre-W7 hardcoded "gaming lock" button:
//   (a) a profile toggle flips the display WITHOUT a reload — GET /api/status is
//       pinned to active:false, so an ON switch after the click can only come
//       from the mutation-answer splice into the held status (§4.5-1).
//   (b) a role-blackout activation raises the shared Klartext confirm with full
//       focus management (focus on the confirm button, Escape cancels + returns
//       focus to the switch, blackout roles in the alertdialog) — §5.1/§5.5.
//
// SSE (/api/events) is aborted by the shared fixture layer, so the page runs on
// the GET /api/status poll path; these overrides sit on top of it.

interface StatusProfileState {
  name: string
  scope: string
  label: string
  active: boolean
  member_count: number
  // roles that activating THIS profile takes fully dark (dry-run truth).
  blackoutOnActivate: string[]
}

function statusBody(profiles: StatusProfileState[], asOf: string): Record<string, unknown> {
  return {
    success: true,
    as_of: asOf,
    health: { status: 'ok', services: { db: 'ok', embed: 'ok', chat: 'ok' } },
    backends: [],
    dream: {
      mode: 'on',
      throttle_interval_s: 0,
      pickable_now: 0,
      in_cooldown: 0,
      never_dreamed: 0,
      awaiting_embed: 0,
      incoming_1h: 0,
      incoming_6h: 0,
      next_pending_at: null,
      last_cycle_at: null,
    },
    llm_24h: [],
    llm_24h_complete: true,
    profiles: profiles.map((p) => ({
      name: p.name,
      scope: p.scope,
      label: p.label,
      active: p.active,
      member_count: p.member_count,
    })),
    activity: null,
  }
}

/**
 * Install a status view whose GET /api/status is FROZEN at active:false (so a
 * live-looking ON can only be the splice) and whose disable-profile-toggle is
 * stateful + returns a NEWER as_of than the poll (floor keeps the splice fresh).
 */
async function installStatus(page: Page, profiles: StatusProfileState[]): Promise<void> {
  const byName = new Map(profiles.map((p) => [p.name, p]))
  // The poll is intentionally frozen at the initial (all-inactive) state.
  const POLL_ASOF = '2026-07-07T12:00:00Z'
  const MUTATION_ASOF = '2026-07-07T12:00:05Z' // newer → floor rejects the stale poll

  await page.route('**/api/status', (route: Route) => route.fulfill({ json: statusBody(profiles, POLL_ASOF) }))

  await page.route('**/api/manage', async (route: Route) => {
    let body: { action?: string; data?: Record<string, unknown> } | null = null
    try {
      body = route.request().postDataJSON()
    } catch {
      /* fall through */
    }
    if (body?.action !== 'disable-profile-toggle') return route.fallback()
    const data = body.data ?? {}
    const p = byName.get(data.name as string)
    if (!p) return route.fulfill({ status: 404, json: { success: false, error: 'not found' } })
    const wantActive = data.active === true
    const dryRun = data.dry_run === true
    const blackout = wantActive ? p.blackoutOnActivate : []
    return route.fulfill({
      json: {
        success: true,
        profile: { name: p.name, scope: p.scope, label: p.label, description: '', active: wantActive, reserved: p.name === 'eject' },
        impact: { backends: [], roles_affected: [], roles_blacked_out: blackout },
        as_of: MUTATION_ASOF,
        note: 'ok',
        ...(dryRun ? { dry_run: true } : {}),
      },
    })
  })
}

const EJECT: StatusProfileState = {
  name: 'eject',
  scope: '_global',
  label: 'Eject-Modus',
  active: false,
  member_count: 2,
  blackoutOnActivate: ['rerank'], // herbert-rerank is the only rerank backend
}

const WARTUNG: StatusProfileState = {
  name: 'gpu-wartung',
  scope: '_global',
  label: 'GPU-Wartung',
  active: false,
  member_count: 1,
  blackoutOnActivate: [], // no role goes fully dark
}

test.describe('status profile quick-toggles (U01-W7)', () => {
  test('(a) a non-blackout toggle flips the display WITHOUT a reload', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await installStatus(page, [structuredClone(WARTUNG)])
    await gotoArea(page, '/status')

    const sw = page.getByRole('button', { name: 'GPU-Wartung umschalten' })
    await expect(sw).toBeVisible()
    await expect(sw).toHaveText('OFF')
    await sw.click()
    // GET /api/status stays active:false; an ON switch is the splice, not a reload.
    await expect(sw).toHaveText('ON')
    await expect(sw).toHaveAttribute('aria-pressed', 'true')
    await expect(page.getByRole('alertdialog')).toHaveCount(0)
    expect(errors, errors.join('\n')).toEqual([])
  })

  test('(b) role-blackout confirm: focus management + alertdialog roles', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await installStatus(page, [structuredClone(EJECT)])
    await gotoArea(page, '/status')

    const sw = page.getByRole('button', { name: 'Eject-Modus umschalten' })
    await sw.focus()
    await expect(sw).toBeFocused()
    await page.keyboard.press('Space')

    // Blackout confirm appears as an alertdialog carrying the dark role.
    const confirm = page.getByRole('alertdialog')
    await expect(confirm).toBeVisible()
    await expect(confirm).toContainText('rerank')
    const confirmBtn = confirm.getByRole('button', { name: 'Trotzdem aktivieren' })
    await expect(confirmBtn).toBeFocused()

    // Escape aborts AND returns focus to the switch; the profile stays OFF.
    await page.keyboard.press('Escape')
    await expect(page.getByRole('alertdialog')).toHaveCount(0)
    await expect(sw).toBeFocused()
    await expect(sw).toHaveText('OFF')
  })

  test('(c) confirmed blackout write flips the display', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await installStatus(page, [structuredClone(EJECT)])
    await gotoArea(page, '/status')

    const sw = page.getByRole('button', { name: 'Eject-Modus umschalten' })
    await sw.click()
    const confirmBtn = page.getByRole('alertdialog').getByRole('button', { name: 'Trotzdem aktivieren' })
    await expect(confirmBtn).toBeFocused()
    await confirmBtn.click()
    await expect(page.getByRole('alertdialog')).toHaveCount(0)
    await expect(sw).toHaveText('ON')
  })
})
