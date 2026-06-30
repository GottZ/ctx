import { test, expect } from '@playwright/test'
import type { Page, Route } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors } from './fixtures'

// FE-TK7b — role + activation controls on the tenant key table (design
// 05-frontend-a3-selfservice §3). Per row: a tenant_role <select> (promote/demote
// → api-key-update {tenant_role}) and an activate/deactivate toggle (→ {active}).
//
// Sichtbarkeits-Guards are COMFORT, the server stays authoritative (last-owner Tx +
// owner-only delegation, 03-be6-roles): (a) self-row — the caller's own key — has
// its role/active controls disabled (no self-elevation / self-lockout); (b) the
// last ACTIVE owner cannot be demoted/deactivated (would orphan the tenant). The
// negative proof shows the FE renders the server's 409 even when the client guard
// permitted the demote (a concurrent-owner-removal race): enforcement is server-side.
//
// These specs override only api-key-list (stateful, so a promote is visible on the
// reload) and — in the negative case — let api-key-update fall through to the
// fixtures fault seam. Everything else (whoami/status/…) defers via route.fallback().

// Identity ids — SELF mirrors fixtures.whoamiFor's api_key_id so isOwnKey matches.
const TENANT_A = '550e8400-e29b-41d4-a716-446655440aaa'
const SELF = '0190000000007000800000000000ke7'
const K8 = '0190000000007000800000000000ke8'
const K9 = '0190000000007000800000000000ke9'

interface KeyRow {
  id: string
  label: string
  tenant_role: 'owner' | 'admin' | 'member'
  active: boolean
}

/** Expand minimal rows into full ApiKeyView wire shapes (types.ts:583). */
function listKeys(rows: KeyRow[]): Record<string, unknown>[] {
  return rows.map((r) => ({
    id: r.id,
    label: r.label,
    home_scope: 'home',
    allowed_scopes: ['home'],
    active: r.active,
    last_used_at: '2026-06-29T11:00:00Z',
    created_at: '2026-05-01T08:00:00Z',
    tenant_role: r.tenant_role,
  }))
}

/**
 * Install a STATEFUL api-key-list/-update mock layered over the fixtures. The list
 * honours active_only (default true hides revoked); api-key-update mutates the in-
 * memory row and re-reads it (the ApiKeyUpdateResult.key shape) so the subsequent
 * list reload reflects the change. With faultUpdate, api-key-update is NOT handled
 * here — it falls through to the fixtures fault seam (a server 409/403).
 */
async function installKeyTable(page: Page, rows: KeyRow[], opts: { faultUpdate?: boolean } = {}): Promise<void> {
  const state = new Map<string, KeyRow>(rows.map((r) => [r.id, { ...r }]))
  await page.route('**/api/manage', async (route: Route) => {
    let body: { action?: string; data?: Record<string, unknown> } | null = null
    try {
      body = route.request().postDataJSON() as typeof body
    } catch {
      body = null
    }
    const action = body?.action
    const data = body?.data ?? {}

    if (action === 'api-key-list') {
      const activeOnly = data.active_only !== false // undefined → server default true
      const rowsOut = [...state.values()].filter((r) => (activeOnly ? r.active : true))
      return route.fulfill({ json: { success: true, keys: listKeys(rowsOut) } })
    }
    if (action === 'api-key-update' && !opts.faultUpdate) {
      const cur = state.get(data.id as string)
      if (cur) {
        if (typeof data.tenant_role === 'string') cur.tenant_role = data.tenant_role as KeyRow['tenant_role']
        if (typeof data.active === 'boolean') cur.active = data.active
      }
      return route.fulfill({ json: { success: true, key: listKeys([cur ?? rows[0]])[0] } })
    }
    return route.fallback()
  })
}

/** A tenant-admin whoami whose own key is `self` with the given tenant_role. */
async function installWhoami(page: Page, self: string, role: 'owner' | 'admin'): Promise<void> {
  await page.route('**/api/whoami', (route: Route) =>
    route.fulfill({
      json: {
        success: true,
        label: 'smoke-key',
        home_scope: 'home',
        read_scopes: ['home', 'shared'],
        api_key_id: self,
        tenant_id: TENANT_A,
        tenant_slug: 'acme',
        tenant_display_name: 'Acme Corp',
        admin: false,
        role,
      },
    }),
  )
}

test.describe('FE-TK7b: tenant role + activation controls', () => {
  test('promote member → admin reflects on the role control after reload', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'tenant-admin', theme: 'dark' })
    // self (ke7) is the lone owner; ci-runner (ke8) is a promotable member.
    await installKeyTable(page, [
      { id: SELF, label: 'smoke-key', tenant_role: 'owner', active: true },
      { id: K8, label: 'ci-runner', tenant_role: 'member', active: true },
    ])
    await gotoArea(page, '/tenant')

    const memberRole = page.getByLabel('role for ci-runner')
    await expect(memberRole).toHaveValue('member')
    await expect(memberRole).toBeEnabled()

    await memberRole.selectOption('admin')
    // keys.update → api-key-update (state→admin) → reload → list re-reads admin.
    await expect(page.getByLabel('role for ci-runner')).toHaveValue('admin')
    expect(errors, errors.join('\n')).toEqual([])
  })

  test('sight-guards: self-row and last-owner controls are disabled, others editable', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'tenant-admin', theme: 'dark' })
    // self is an admin (isolates the SELF guard from last-owner); ke8 is the lone
    // active owner (isolates the LAST-OWNER guard from self); ke9 is a free member.
    await installWhoami(page, SELF, 'admin')
    await installKeyTable(page, [
      { id: SELF, label: 'smoke-key', tenant_role: 'admin', active: true },
      { id: K8, label: 'sole-owner', tenant_role: 'owner', active: true },
      { id: K9, label: 'member-key', tenant_role: 'member', active: true },
    ])
    await gotoArea(page, '/tenant')

    // (a) self-row: role + active disabled (no self-elevation / self-lockout).
    await expect(page.getByLabel('role for smoke-key')).toBeDisabled()
    await expect(page.getByLabel('activation for smoke-key')).toBeDisabled()

    // (b) last active owner: role + active disabled (would orphan the tenant).
    await expect(page.getByLabel('role for sole-owner')).toBeDisabled()
    await expect(page.getByLabel('activation for sole-owner')).toBeDisabled()

    // a free member row stays fully editable.
    await expect(page.getByLabel('role for member-key')).toBeEnabled()
    await expect(page.getByLabel('activation for member-key')).toBeEnabled()
    expect(errors, errors.join('\n')).toEqual([])
  })

  test('negative: demoting an owner the server rejects renders the 409 banner', async ({ page }) => {
    await seedSession(page, {
      role: 'tenant-admin',
      theme: 'dark',
      faults: [{ action: 'api-key-update', status: 409, error: 'cannot remove the last active owner of the tenant' }],
    })
    // TWO active owners → the client last-owner guard is OFF (count 2), so the
    // co-owner's demote is enabled and reaches the server, which faults 409 (the
    // race the FE must surface). faultUpdate → api-key-update hits the fault seam.
    await installKeyTable(
      page,
      [
        { id: SELF, label: 'smoke-key', tenant_role: 'owner', active: true },
        { id: K8, label: 'co-owner', tenant_role: 'owner', active: true },
      ],
      { faultUpdate: true },
    )
    await gotoArea(page, '/tenant')

    const coOwner = page.getByLabel('role for co-owner')
    await expect(coOwner).toBeEnabled() // count 2 → not guarded
    await coOwner.selectOption('admin')

    await expect(page.locator('.action-error')).toContainText('last active owner')
  })

  test('activation toggle reactivates a revoked member key', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'tenant-admin', theme: 'dark' })
    await installKeyTable(page, [
      { id: SELF, label: 'smoke-key', tenant_role: 'owner', active: true },
      { id: K8, label: 'ci-runner', tenant_role: 'member', active: false }, // revoked
    ])
    await gotoArea(page, '/tenant')

    // Reveal revoked keys so the inactive row is in the list.
    await page.getByRole('checkbox', { name: /show revoked/i }).check()

    const toggle = page.getByLabel('activation for ci-runner')
    await expect(toggle).toHaveText('activate')
    await toggle.click()
    // api-key-update {active:true} → reload → row now active.
    await expect(page.getByLabel('activation for ci-runner')).toHaveText('deactivate')
    expect(errors, errors.join('\n')).toEqual([])
  })
})
