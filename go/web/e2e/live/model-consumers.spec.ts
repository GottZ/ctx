// @live — the isolation class of the model API (design/05 §7 W05-18, B-1/B-7;
// masterplan I-K0) against the REAL server enforcement of the throwaway stack:
//
//   (a) a tenant-admin's /gateway lists only its own tenant's consumers
//       (model-consumer-list filters on the caller's tenant, W01-5). Positive
//       control FIRST (B's own consumer renders), then a deterministic end
//       state (exactly B's seeded rows, no further page), then the absence of
//       A's consumer label — and a label-prefix search for it, which the
//       server answers inside B's tenant with the empty state.
//   (b) a model key (ctxm_) is no web login: the real form gets a 401
//       exchange and the error band, and no session comes up (no shell, no
//       session cookie, whoami 401); POST /auth/login answers 401 as well
//       (AuthenticateRaw refuses the ctxm_ class without a DB lookup, W01-4).
//
// Negative gates (seed overlays, passed through by run-live.sh):
//   CTX_E2E_LEAK_INJECT=1   — a consumer with A's label in tenant B ⇒ (a) red
//   CTX_E2E_MODELKEY_SWAP=1 — B's owner key stands in as B's model key ⇒ (b) red
//
// No clear key in this file's artifacts (wave W05-18; measured container-free
// against Playwright 1.64.1): a trace records every fill value and request
// body, and a failing locator matcher embeds an aria snapshot that carries a
// password input's value. So this file runs without traces, blanks every
// input right after the login exchange (before any locator assertion), and
// blanks them again in afterEach for the snapshot Playwright takes when the
// context of a failed test closes. Keys live only in the seed state file.

import { test, expect, type Page, type Response } from '@playwright/test'
import { readState } from './state'

const state = readState()
const consumers = state.consumers

test.use({ trace: 'off' })

/** Blank every input of the page — the failure aria snapshot reads .value. */
async function blankInputs(page: Page): Promise<void> {
  await page.evaluate(() => {
    document.querySelectorAll('input').forEach((el) => {
      el.value = ''
    })
  })
}

test.afterEach(async ({ context }) => {
  for (const p of context.pages()) await blankInputs(p).catch(() => {})
})

/**
 * Drive the REAL login form with key and wait for its POST /auth/login
 * exchange; the field is blanked before the caller asserts anything.
 */
async function submitLogin(page: Page, key: string): Promise<Response> {
  await page.goto('/')
  await page.locator('#api-key').fill(key)
  const exchange = page.waitForResponse(
    (r) => r.request().method() === 'POST' && new URL(r.url()).pathname === '/auth/login',
  )
  await page.getByRole('button', { name: /sign in/i }).click()
  const res = await exchange
  await blankInputs(page)
  return res
}

test('@live model-consumers: B’s /gateway lists its own consumer, never A’s', async ({ page }) => {
  const login = await submitLogin(page, state.tenants.b.ownerKey)
  expect(login.status(), 'B’s owner key signs in').toBe(200)
  await expect(page.locator('.shell')).toBeVisible()

  await page.goto('/gateway')
  // A tenant-admin reaches the area only while the stack runs with
  // gateway.enabled (viewModelApi; else the guard redirects to /status) — a
  // stack without the switch goes red here instead of passing vacuously.
  await expect(page).toHaveURL(/\/gateway$/)

  const content = page.locator('main.content')
  const rows = content.getByTestId('consumer-row')

  // (1) POSITIVE CONTROL: B's own consumer renders — the list provably shows
  //     real tenant-B data from the live model-consumer-list.
  await expect(rows.getByTestId('consumer-label').filter({ hasText: consumers.b.label })).toBeVisible()

  // (2) DETERMINISTIC END STATE: the list is one complete server answer —
  //     exactly B's seeded rows and no further page (the count carries no
  //     "+"). A leaked consumer is an extra row and fails here.
  await expect(rows, 'B’s list holds exactly B’s seeded consumers — an extra row is a foreign consumer').toHaveCount(
    consumers.bRows,
  )
  await expect(content.locator('section[aria-label="model consumers"] .count')).toHaveText(String(consumers.bRows))
  await expect(content).not.toContainText(consumers.a.label)

  // (3) SERVER LOOKUP: a label-prefix search for A's label runs inside B's
  //     tenant (model-consumer-list q); its clean end state is the empty
  //     state. The field VALUE holds A's label, but that is no text node.
  await content.locator('#gw-q').fill(consumers.a.label)
  await content.locator('#gw-q').press('Enter')
  await expect(content.getByText('No consumers to show')).toBeVisible()
  await expect(content).not.toContainText(consumers.a.label)
})

test('@live model-consumers: a ctxm_ model key at the login form gets the error band, no session', async ({ page }) => {
  const login = await submitLogin(page, consumers.b.modelKey)
  // Exactly 401: the key reached AuthenticateRaw (not 400/403/429).
  expect(login.status(), 'POST /auth/login with B’s model key').toBe(401)
  await expect(page.locator('[role="alert"]')).toBeVisible()
  await expect(page.locator('.shell')).toHaveCount(0)

  // No session came up: no session cookie in the browser context, and whoami
  // over that context answers 401.
  const cookies = (await page.context().cookies()).map((c) => c.name)
  expect(cookies).not.toContain('ctx_session')
  expect(cookies).not.toContain('ctx_refresh')
  expect((await page.request.get('/api/whoami')).status()).toBe(401)
})

test('@live model-consumers: POST /auth/login with a ctxm_ model key answers 401, no cookie', async ({ request }) => {
  const res = await request.post('/auth/login', { data: { api_key: consumers.b.modelKey } })
  expect(res.status()).toBe(401)
  expect(res.headers()['set-cookie']).toBeUndefined()
})
