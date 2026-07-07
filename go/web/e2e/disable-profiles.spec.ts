import { test, expect, type Page, type Route } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors } from './fixtures'

// Abschaltprofile-Karte (092, Web-UX U01-W6; design/01 §4.6 + AM-5). Three
// gates, each red against HEAD 33c31a8 (the card did not exist; disable-profile-
// list hit the 599 hard-default):
//   (a) the card shows member NAMES + the role impact BEFORE any click — the
//       Erstnutzer axis ("fraglich für neue nutzer, wie er funktioniert").
//   (b) a role-blackout activation raises a Klartext confirm step with full
//       focus management: focus lands on the confirm button, Escape cancels and
//       returns focus to the switch, the blackout roles sit in the alertdialog
//       (§5.1/§5.5) — the keyboard/screenreader user provably escapes.
//   (c) a non-blackout toggle flips the switch state without a confirm.
//
// The rich scenario (members, blackout) is injected per-test via a route
// override that falls back to the shared fixture layer for every other action.

interface ProfileState {
  name: string
  scope: string
  label: string
  description: string
  active: boolean
  reserved: boolean
  members: { id: string; name: string; scope: string; roles: string[] }[]
  rolesAffected: string[]
  // roles that activating THIS profile would take fully dark (dry-run truth).
  blackoutOnActivate: string[]
}

function memberView(m: ProfileState['members'][number], active: boolean) {
  return {
    id: m.id,
    name: m.name,
    scope: m.scope,
    roles: m.roles,
    enabled: true,
    effective_state: active ? 'profile-disabled' : 'active',
  }
}

function listView(p: ProfileState) {
  return {
    name: p.name,
    scope: p.scope,
    label: p.label,
    description: p.description,
    active: p.active,
    reserved: p.reserved,
    impact: {
      backends: p.members.map((m) => memberView(m, p.active)),
      roles_affected: p.rolesAffected,
      // list impact is at the CURRENT state (active profile shows its live
      // blackout; inactive shows none — the activation blackout comes on dry-run).
      roles_blacked_out: p.active ? p.blackoutOnActivate : [],
    },
  }
}

/** Install a stateful disable-profile-* mock; everything else falls back. */
async function installProfiles(page: Page, profiles: ProfileState[]): Promise<void> {
  const byName = new Map(profiles.map((p) => [p.name, p]))
  await page.route('**/api/manage', async (route: Route) => {
    let body: { action?: string; data?: Record<string, unknown> } | null = null
    try {
      body = route.request().postDataJSON()
    } catch {
      /* fall through */
    }
    const action = body?.action
    const data = body?.data ?? {}
    if (action === 'disable-profile-list') {
      return route.fulfill({ json: { success: true, profiles: profiles.map(listView) } })
    }
    if (action === 'disable-profile-toggle') {
      const p = byName.get(data.name as string)
      if (!p) return route.fulfill({ status: 404, json: { success: false, error: 'not found' } })
      const wantActive = data.active === true
      const dryRun = data.dry_run === true
      const blackout = wantActive ? p.blackoutOnActivate : []
      if (!dryRun) p.active = wantActive // stateful flip → next list reflects it
      return route.fulfill({
        json: {
          success: true,
          profile: { name: p.name, scope: p.scope, label: p.label, description: p.description, active: p.active, reserved: p.reserved },
          impact: {
            backends: p.members.map((m) => memberView(m, wantActive)),
            roles_affected: p.rolesAffected,
            roles_blacked_out: blackout,
            ...(blackout.includes('embed') ? { embed_degraded: true } : {}),
          },
          as_of: '2026-06-29T12:00:01Z',
          note: 'ok',
          ...(dryRun ? { dry_run: true } : {}),
        },
      })
    }
    return route.fallback() // backend-list, whoami-adjacent, everything else
  })
}

const EJECT: ProfileState = {
  name: 'eject',
  scope: '_global',
  label: 'Eject-Modus',
  description: 'Nimmt die GPU-Backends aus jeder Chain.',
  active: false,
  reserved: true,
  members: [
    { id: 'b-chat', name: 'herbert-chat', scope: '_global', roles: ['chat'] },
    { id: 'b-rerank', name: 'herbert-rerank', scope: '_global', roles: ['rerank'] },
  ],
  rolesAffected: ['chat', 'rerank'],
  blackoutOnActivate: ['rerank'], // herbert-rerank is the only rerank backend
}

const WARTUNG: ProfileState = {
  name: 'gpu-wartung',
  scope: '_global',
  label: 'GPU-Wartung',
  description: 'Einzel-Host-Wartung.',
  active: false,
  reserved: false,
  members: [{ id: 'b-chat', name: 'herbert-chat', scope: '_global', roles: ['chat'] }],
  rolesAffected: ['chat'],
  blackoutOnActivate: [], // chat still served elsewhere → no blackout
}

test.describe('disable-profiles card (U01-W6)', () => {
  test('(a) shows member names + role impact BEFORE any click', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await installProfiles(page, [structuredClone(EJECT), structuredClone(WARTUNG)])
    await gotoArea(page, '/settings/backends')

    const card = page.locator('section.card[aria-label="Abschaltprofile"]')
    await expect(card).toBeVisible()
    // Member backend NAMES are rendered as text (not hover-only).
    await expect(card).toContainText('herbert-chat')
    await expect(card).toContainText('herbert-rerank')
    // The role impact is visible pre-click.
    await expect(card).toContainText('trifft 2 Backends')
    await expect(card).toContainText('Rollen: chat, rerank')
    expect(errors, errors.join('\n')).toEqual([])
  })

  test('(b) role-blackout confirm: focus management + alertdialog roles', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await installProfiles(page, [structuredClone(EJECT), structuredClone(WARTUNG)])
    await gotoArea(page, '/settings/backends')

    // Keyboard path: focus the eject switch, toggle it on.
    const ejectSwitch = page.getByRole('checkbox', { name: 'Eject-Modus aktiv' })
    await ejectSwitch.focus()
    await expect(ejectSwitch).toBeFocused()
    await page.keyboard.press('Space')

    // The confirm step is an alertdialog carrying the blacked-out role.
    const confirm = page.getByRole('alertdialog')
    await expect(confirm).toBeVisible()
    await expect(confirm).toContainText('rerank')
    // Focus moved onto the confirm button (not the container edge).
    const confirmBtn = confirm.getByRole('button', { name: 'Trotzdem aktivieren' })
    await expect(confirmBtn).toBeFocused()

    // Escape aborts AND returns focus to the switch that opened it.
    await page.keyboard.press('Escape')
    await expect(page.getByRole('alertdialog')).toHaveCount(0)
    await expect(ejectSwitch).toBeFocused()
    // Aborted → the profile stays inactive (no write happened).
    await expect(ejectSwitch).not.toBeChecked()
  })

  test('(c) non-blackout toggle flips the switch without a confirm', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await installProfiles(page, [structuredClone(EJECT), structuredClone(WARTUNG)])
    await gotoArea(page, '/settings/backends')

    const wartungSwitch = page.getByRole('checkbox', { name: 'GPU-Wartung aktiv' })
    await expect(wartungSwitch).not.toBeChecked()
    await wartungSwitch.click()
    // No confirm dialog (no role goes dark), and the reload reflects the flip.
    await expect(page.getByRole('alertdialog')).toHaveCount(0)
    await expect(wartungSwitch).toBeChecked()
  })

  test('(d) confirmed blackout write flips the switch', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await installProfiles(page, [structuredClone(EJECT), structuredClone(WARTUNG)])
    await gotoArea(page, '/settings/backends')

    const ejectSwitch = page.getByRole('checkbox', { name: 'Eject-Modus aktiv' })
    await ejectSwitch.click()
    const confirmBtn = page.getByRole('alertdialog').getByRole('button', { name: 'Trotzdem aktivieren' })
    await expect(confirmBtn).toBeFocused()
    await confirmBtn.click()
    await expect(page.getByRole('alertdialog')).toHaveCount(0)
    await expect(ejectSwitch).toBeChecked()
  })
})
