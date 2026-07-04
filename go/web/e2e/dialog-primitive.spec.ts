import { test, expect } from '@playwright/test'
import { seedSession, gotoArea, trackPageErrors } from './fixtures'

// Q9 Dialog-/Backdrop-Primitiv (design 05 sec.7) — the modal behaviours the six
// migrated dialogs inherit from lib/ui/Modal.svelte's showModal() call: a browser
// top-layer focus-trap and native Esc->close. These are exercised functionally by
// the create-flow specs (tenant-create / admin-tenant / selfservice-keys), but
// NEVER as an explicit Esc/trap assertion — the bestand covered only FloatingWindow
// (non-modal, graph-windows.spec.ts). This spec is the ROT-PROBE: swap Modal's
// showModal() for a non-modal .show() and BOTH tests fail (a .show() dialog neither
// traps focus nor closes on Esc). vitest is node-only here (no DOM), so the trap/Esc
// guard lives in Playwright by the repo's own doctrine (ConfirmDialog.test.ts note).

test.describe('Q9 modal primitive: focus-trap + Esc-close (showModal contract)', () => {
  test('Esc closes the modal (native cancel — a non-modal .show() would not)', async ({ page }) => {
    const errors = trackPageErrors(page)
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/admin')

    await page.getByRole('button', { name: '+ New tenant' }).click()
    const dialog = page.getByRole('dialog')
    await expect(dialog).toBeVisible()

    await page.keyboard.press('Escape')
    await expect(dialog).toBeHidden()

    expect(errors, errors.join('\n')).toEqual([])
  })

  test('open focus lands inside the modal and outside content is inert (trap)', async ({ page }) => {
    await seedSession(page, { role: 'server-admin', theme: 'dark' })
    await gotoArea(page, '/admin')

    await page.getByRole('button', { name: '+ New tenant' }).click()
    const dialog = page.getByRole('dialog')
    await expect(dialog).toBeVisible()

    // showModal() moves focus INTO the dialog on open (a non-modal .show() leaves
    // it on the trigger button, outside the dialog).
    const focusInsideOnOpen = await dialog.evaluate((d) => d.contains(document.activeElement))
    expect(focusInsideOnOpen, 'focus should be inside the modal on open').toBe(true)

    // The modal top-layer makes the page BEHIND it inert: a programmatic focus of
    // an outside control is refused, focus stays inside the dialog. A non-modal
    // .show() would let that outside control take focus (trap broken -> red).
    const trapped = await page.evaluate(() => {
      const outside = document.querySelector<HTMLElement>('main.content button, main.content a')
      outside?.focus()
      const dlg = document.querySelector('dialog[open]')
      return { hadOutside: !!outside, focusStillInside: !!dlg && dlg.contains(document.activeElement) }
    })
    expect(trapped.hadOutside, 'a focusable control must exist behind the modal').toBe(true)
    expect(trapped.focusStillInside, 'outside content should be inert while the modal is open').toBe(true)
  })
})
