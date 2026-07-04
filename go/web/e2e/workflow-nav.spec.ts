import { test, expect } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors } from './fixtures'

// Cap-negative probe for the workflow surface (design 04 §4.1.3/§4.1.6/§5.5,
// wave U04). The dark-launch invariant, BOTH directions:
//   - member WITHOUT the viewWorkflow flag: NO Workflow rail section AND NO
//     MemberHome tile — yet a deep link to /issues|/issues/:id|/board still
//     renders the page's EmptyState (no redirect to a landing, no NotFound, no
//     crash: the routes are ungated member surfaces, auth stays server-side).
//   - member WITH the flag: the Workflow rail section (Issues + Board) and the
//     home tile appear.
// This is the rail↔guard separation (PV7 precedence): visibility is the flag's
// job, reachability is NOT gated — the section can hide while the route lives.

test.describe('workflow surface — dark-launch cap gate (U04)', () => {
  test('member WITHOUT viewWorkflow: no section, no tile, deep links still render EmptyState', async ({
    page,
  }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark' })

    // /home: the workflow tile is absent.
    await gotoArea(page, '/home')
    const content = page.locator('main.content')
    await expect(content).toContainText('Welcome, smoke-key')
    await expect(content.getByRole('link', { name: /Open issues/ })).toHaveCount(0)
    // Rail: no Workflow group.
    await expect(page.locator('nav.rail [role="group"][aria-label="Workflow"]')).toHaveCount(0)

    // Deep link to each dark-launched route still renders the page, never a
    // redirect back to /home, never NotFound. (/issues auto-selects the lone
    // project and writes ?scope=, so the URL carries a query — U05.)
    await gotoArea(page, '/issues')
    await expect(page).toHaveURL(/\/issues(\?|$)/)
    await expect(page.getByRole('heading', { name: 'Issues' })).toBeVisible()

    await gotoArea(page, '/board')
    await expect(page).toHaveURL(/\/board$/)
    await expect(content).toContainText('No board to show yet')

    await gotoArea(page, '/issues/550e8400-e29b-41d4-a716-446655440001')
    await expect(page).toHaveURL(/\/issues\/550e8400/)
    await expect(content).toContainText('Issue detail is not wired up yet')

    expect(errors, errors.join('\n')).toEqual([])
  })

  test('member WITH viewWorkflow: Workflow section + home tile appear', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'member', theme: 'dark', capabilities: { workflow: true } })

    await gotoArea(page, '/home')
    const content = page.locator('main.content')
    const tile = content.getByRole('link', { name: /Open issues/ })
    await expect(tile).toBeVisible()

    // Rail: the Workflow group carries both items.
    const group = page.locator('nav.rail [role="group"][aria-label="Workflow"]')
    await expect(group).toBeVisible()
    await expect(group.getByRole('link', { name: 'Issues' })).toBeVisible()
    await expect(group.getByRole('link', { name: 'Board' })).toBeVisible()

    // The tile links into the issue surface.
    await tile.click()
    await expect(page).toHaveURL(/\/issues(\?|$)/)
    await expect(page.getByRole('heading', { name: 'Issues' })).toBeVisible()

    expect(errors, errors.join('\n')).toEqual([])
  })
})
