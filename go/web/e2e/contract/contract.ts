// PageContract model + runner (design 06 §4.1, wave PV4).
//
// Doctrine split: MECHANISM = this runner (test generation, meta-guards),
// POLICY = the declarative contract data in registry.ts. A new page changes
// only data (registry entry + baselines), never this file.
//
// PV4 generates the flow/visual/deny/leak/scale dimensions; the a11y gate
// (axe + focus walk, PV5) and the ARIA snapshots (PV6) extend THIS generator
// in their own waves — COVERAGE.md marks those columns as pending until then.
//
// Model deviations from design 06 §4.1, named (not silently resolved):
//   - `mobile` is `{ exempt: string }` instead of the doc's bare `false` +
//     JSDoc duty — machine-checkable like the scale exempt (a JSDoc duty
//     cannot make the matrix red; W19: opt-outs must be mechanically visible).
//   - `mode`/`path`/`flowDoc`/`adminCalls` are additive fields the doc model
//     does not list: `mode` carries the consolidated data-mode asserts of the
//     legacy smoke suite (seam S8), `path` resolves '*'/template routes to a
//     navigable URL, `flowDoc` makes the §4.1 "Kern-Pfad als Satz" JSDoc duty
//     a machine-carried COVERAGE.md column, `adminCalls` names the page's
//     admin API calls whose ABSENCE the generated deny test asserts.
//   - Scale states carry an explicit `domCap` (selector + max) instead of the
//     doc's global "<300 DOM-Knoten" literal: the W7 workbench paginates via
//     keyset (10 rows/page server default) without virtualization, so the cap
//     is a per-contract declaration; the 300-element literal returns as the
//     default once the virtualized Achse-04 list surfaces land.

import { expect, test, type Page } from '@playwright/test'
import type { LayoutMode } from '../../src/lib/layout/modes'
import { captureShot } from './capture'
import {
  gotoArea,
  SENTINEL,
  seedSession,
  trackPageErrors,
  type Role,
  type SeededSession,
  type SeedOptions,
} from '../fixtures'

export type Theme = 'dark' | 'light'
export type ViewportName = 'desktop' | 'mobile'

/** The two contract viewports (design 06 §4.1: 1440×900 + 390×844). */
export const VIEWPORTS: Record<ViewportName, { width: number; height: number }> = {
  desktop: { width: 1440, height: 900 },
  mobile: { width: 390, height: 844 },
}

export const THEMES: readonly Theme[] = ['dark', 'light']

/**
 * Fixture "now": fixtures.ts pins as_of/finished_at to 2026-06-29T12:00:00Z;
 * fixing the page clock 5 s after that keeps every relative-time render
 * ("5s ago" on /status) byte-stable across runs (design 06 §4.3, time rule).
 */
export const FIXED_NOW = new Date('2026-06-29T12:00:05Z')

/** Contract-level seed: theme comes from the runner loop, role defaults to the contract role. */
export type ContractSeed = Omit<SeedOptions, 'role' | 'theme'> & { role?: Role }

export interface PageState {
  /** Baseline/matrix key — part of the screenshot {arg}, so [a-z0-9-] only. */
  name: string
  /** fixtures options for this state (role defaults to the contract role). */
  seed: ContractSeed
  /** Bring the page into the state after mount (open dialog, …). */
  prepare?: (page: Page) => Promise<void>
}

/** DOM ceiling for a scale state — the region that must stay bounded at 10k items. */
export interface DomCap {
  /** Locator of the repeated data rows (positive control: must match > 0). */
  selector: string
  /** Maximum rendered rows at 10k fixture items (≪ 10 000 by construction). */
  max: number
}

/** A scale PageState: the 10k proof with a mandatory DOM ceiling (design 06 §6.2). */
export interface ScaleState extends PageState {
  domCap: DomCap
  /** Page-typical scale interaction (keyset load-more, scroll, …) — runs after the cap assert. */
  flow?: (page: Page, session: SeededSession) => Promise<void>
}

/**
 * Target-scale MANDATORY dimension (W8): a scale state or a REASONED opt-out.
 * Compile-red without the field; the matrix meta-test fails on an empty
 * exempt string — the 10k+ dimension cannot erode silently (design 06 §4.1).
 */
export type ScaleDimension = ScaleState | { exempt: string }

export function isScaleState(s: ScaleDimension): s is ScaleState {
  return (s as ScaleState).name !== undefined
}

export interface AxeExclude {
  selector: string
  /** Why the region is excluded — surfaced in COVERAGE.md (design 06 §4.3c). */
  reason: string
}

export interface PageContract {
  /** Key from areaRoutes ('/blocks', '*', …) or the literal 'login' (pre-router gate). */
  route: string
  /** Matrix/baseline key ('blocks', 'notfound', 'login'). */
  name: string
  /**
   * Minimal role that reaches the page per the MECHANICAL client guard
   * (guard.ts TIER_GATED: /admin, /tenant). Named Ist-finding (PV4): the rail
   * hides /status + /settings from members (viewOpsSurfaces), but the guard
   * does NOT gate them — rail and guard diverge, so "Rail-/Guard-konsistent"
   * (§4.1) is not satisfiable there. This field follows the guard (the only
   * redirect the generated deny test can assert); server-side authorization
   * stays the real gate (RequireAdminOrTenantAdmin).
   */
  role: Role
  /** Expected main.content data-mode (consolidated S8 assert of the legacy smoke). */
  mode?: LayoutMode
  /** Concrete navigation path — REQUIRED for '*' and ':param' template routes. */
  path?: string
  /** At least 'default'; empty/error states are declared, not hand-written. */
  states: PageState[]
  scale: ScaleDimension
  /** The PRIMARY human path as one sentence — carried into COVERAGE.md (§4.1). */
  flowDoc: string
  /** The primary human flow, on a mounted page (runner owns goto/shell-wait). */
  primaryFlow: (page: Page, session: SeededSession) => Promise<void>
  /** Contract-local mask selectors (additional to [data-e2e-mask]); meta-guarded (§4.3). */
  mask?: string[]
  /** The only exception to the 40 % mask budget — reason + issue mandatory. */
  maskBudgetOverride?: { reason: string; issue: string }
  /** axe excludes with reason duty — consumed by the PV5 a11y gate, listed in COVERAGE.md now. */
  axe?: { exclude?: AxeExclude[] }
  /** Mobile opt-OUT with mandatory reason (default: mobile runs) — design 06 §4.1. */
  mobile?: { exempt: string }
  /** true ⇒ generated tenant-leak probe with positive control (design 06 §5.6b). */
  tenantScoped?: boolean
  /** Admin API calls (manage actions or /api paths) that must NOT fire in the deny test. */
  adminCalls?: string[]
  /** Optional @live flow — consumed by the live tier (PV10). */
  live?: (page: Page) => Promise<void>
}

/** Next-lower role for the generated deny test (design 06 §4.1). */
export function denyRoleFor(role: Role): Role | null {
  if (role === 'server-admin') return 'tenant-admin'
  if (role === 'tenant-admin') return 'member'
  return null
}

/** Guard landing target for a role (mirrors landingFor, guard.ts:34-36). */
export function landingForRole(role: Role): string {
  return role === 'member' ? '/home' : '/status'
}

const STATE_NAME = /^[a-z0-9-]+$/

/**
 * Structural contract validation — PURE (no playwright runtime), shared by the
 * runner (throws), the matrix meta-test (asserts empty) and the vitest pin.
 * Returns the list of violations; [] = valid.
 */
export function validateContract(c: PageContract): string[] {
  const errs: string[] = []
  if (!c.name || !STATE_NAME.test(c.name)) errs.push(`name '${c.name}' must match ${STATE_NAME}`)
  if (!c.route) errs.push('route must be non-empty')
  else if (c.route !== 'login' && c.route !== '*' && !c.route.startsWith('/'))
    errs.push(`route '${c.route}' must be an areaRoutes key ('/…', '*') or 'login'`)
  if ((c.route === '*' || c.route.includes(':')) && !c.path)
    errs.push(`route '${c.route}' needs an explicit navigable 'path'`)
  if (c.path !== undefined && !c.path.startsWith('/')) errs.push(`path '${c.path}' must start with '/'`)
  if (c.states.length === 0) errs.push('states must contain at least the default state')
  if (!c.states.some((s) => s.name === 'default')) errs.push("states must include a 'default' state")
  const names = c.states.map((s) => s.name).concat(isScaleState(c.scale) ? [c.scale.name] : [])
  if (new Set(names).size !== names.length) errs.push('state names must be unique (they key the baselines)')
  for (const n of names) if (!STATE_NAME.test(n)) errs.push(`state name '${n}' must match ${STATE_NAME}`)
  if (c.flowDoc.trim() === '') errs.push('flowDoc must document the primary human path (§4.1 JSDoc duty, machine-carried)')
  if (isScaleState(c.scale)) {
    if (c.scale.domCap.selector.trim() === '') errs.push('scale.domCap.selector must be non-empty')
    if (!(c.scale.domCap.max >= 1)) errs.push('scale.domCap.max must be ≥ 1')
    if (c.scale.domCap.max >= 10_000) errs.push('scale.domCap.max must be ≪ the 10k corpus (the cap IS the proof)')
  } else if (c.scale.exempt.trim() === '') {
    errs.push('scale.exempt must carry a non-empty reason — the target-scale dimension may not erode silently (W8)')
  }
  if (c.mobile !== undefined && c.mobile.exempt.trim() === '')
    errs.push('mobile.exempt must carry a non-empty reason (opt-out discipline, §4.1)')
  if (c.maskBudgetOverride !== undefined) {
    if (c.maskBudgetOverride.reason.trim() === '' || c.maskBudgetOverride.issue.trim() === '')
      errs.push('maskBudgetOverride requires non-empty reason AND issue (§4.3)')
  }
  for (const ex of c.axe?.exclude ?? []) {
    if (ex.selector.trim() === '' || ex.reason.trim() === '')
      errs.push('axe.exclude entries require non-empty selector AND reason (§4.3c)')
  }
  return errs
}

/** Navigable URL of a contract (template/'*' routes declare it explicitly). */
export function navPath(c: PageContract): string {
  return c.path ?? c.route
}

function seedFor(c: PageContract, state: PageState, theme: Theme): SeedOptions {
  const { role, ...rest } = state.seed
  return { role: role ?? c.role, theme, ...rest }
}

/** Wait for the authenticated shell (App.svelte → AppShell, past restore). */
async function waitForShell(page: Page): Promise<void> {
  await expect(page.locator('.shell')).toBeVisible()
  // Below the sm breakpoint the persistent rail is replaced by the mobile bar
  // (S3) — assert whichever the current viewport renders.
  const vp = page.viewportSize()
  const mobile = vp !== null && vp.width < 640
  await expect(page.locator(mobile ? 'header.mobile-bar' : 'nav.rail[aria-label="Primary"]')).toBeVisible()
}

/** Mount a contract state: fixed clock → seed → goto → shell + theme/mode asserts. */
async function mountState(
  page: Page,
  c: PageContract,
  state: PageState,
  theme: Theme,
): Promise<SeededSession> {
  await page.clock.setFixedTime(FIXED_NOW)
  const session = await seedSession(page, seedFor(c, state, theme))
  await gotoArea(page, navPath(c))
  await waitForShell(page)
  await expect(page.locator('html')).toHaveAttribute('data-theme', theme)
  if (c.mode) await expect(page.locator('main.content')).toHaveAttribute('data-mode', c.mode)
  if (state.prepare) await state.prepare(page)
  return session
}

function visualTest(c: PageContract, state: PageState, theme: Theme, viewport: ViewportName): void {
  test(`${c.name} ${state.name} ${theme} ${viewport} visual`, { tag: '@visual' }, async ({ page }) => {
    await mountState(page, c, state, theme)
    // Let the area settle (sigma boot / lazy chunk) before capture; fonts are
    // awaited inside captureShot (stabilize). Same settle as the PV3 suite —
    // the 13 committed baselines stay byte-valid.
    await page.waitForTimeout(600)
    await captureShot(page, `${c.name}--${state.name}--${theme}--${viewport}.png`, {
      mask: c.mask,
      maskBudgetOverride: c.maskBudgetOverride,
    })
  })
}

/**
 * Generate the PV4 test family for one contract (design 06 §4.1 table —
 * flow/visual/deny/leak/scale; axe/ARIA/focus-walk join in PV5/PV6):
 *
 *   - primary flow            1 × @flow      (desktop, dark, default state)
 *   - toHaveScreenshot        states(+scale) × themes × viewports × @visual
 *                             (mobile halved away by a reasoned opt-out)
 *   - deny                    1 × @flow      (role ≠ member only)
 *   - tenant-leak probe       1 × @flow      (tenantScoped only, §5.6b)
 *   - scale                   1 × @flow      (scale is a PageState only)
 */
export function definePageContract(c: PageContract): void {
  const problems = validateContract(c)
  if (problems.length > 0) {
    throw new Error(`invalid PageContract '${c.name}': ${problems.join('; ')}`)
  }
  const path = navPath(c)
  const defaultState = c.states.find((s) => s.name === 'default') as PageState
  const visualStates: PageState[] = [...c.states, ...(isScaleState(c.scale) ? [c.scale] : [])]

  test.describe(`contract:${c.name}`, () => {
    // ---- primary flow (the human core path — W21: never only the side path) ----
    test(`${c.name} primary flow`, { tag: '@flow' }, async ({ page }) => {
      const errors = trackPageErrors(page)
      const session = await mountState(page, c, defaultState, 'dark')
      await c.primaryFlow(page, session)
      expect(errors, `uncaught page errors on ${path}:\n${errors.join('\n')}`).toEqual([])
    })

    // ---- visual dimension (@visual — compared only inside the pinned container) ----
    for (const state of visualStates) {
      for (const theme of THEMES) visualTest(c, state, theme, 'desktop')
    }
    if (c.mobile === undefined) {
      test.describe(() => {
        test.use({ viewport: VIEWPORTS.mobile })
        for (const state of visualStates) {
          for (const theme of THEMES) visualTest(c, state, theme, 'mobile')
        }
      })
    }

    // ---- deny (generated auth dimension, §4.1) ----
    const lower = denyRoleFor(c.role)
    if (lower !== null) {
      const landing = landingForRole(lower)
      test(`${c.name} deny (${lower})`, { tag: '@flow' }, async ({ page }) => {
        const session = await seedSession(page, { role: lower, theme: 'dark' })
        await page.goto(path)
        await page.locator('.shell').waitFor({ state: 'visible', timeout: 10_000 })
        // The guard redirect (guardArea → landingFor, guard.ts:55-63) must land
        // the lower role on ITS landing — never mount the gated area.
        await expect(page).toHaveURL(new RegExp(`${landing.replace(/\//g, '\\/')}$`))
        await page.locator('main.content > *').first().waitFor({ state: 'attached', timeout: 10_000 })
        const fired = session.calls.map((x) => x.action ?? x.path)
        for (const a of c.adminCalls ?? []) {
          expect(fired, `admin call '${a}' must not fire for a ${lower} session`).not.toContain(a)
        }
      })
    }

    // ---- tenant-leak probe (generated, §5.6b — positive control first) ----
    if (c.tenantScoped === true) {
      test(`${c.name} tenant leak probe`, { tag: '@flow' }, async ({ page }) => {
        await seedSession(page, { role: c.role, theme: 'dark', tenant: 'B' })
        await gotoArea(page, path)
        await waitForShell(page)
        // (1) Positive control: the OWN sentinel is rendered in the data
        // region — the detector provably sees data; an error state or empty
        // store can no longer make the probe vacuously green.
        await expect(page.locator('main.content')).toContainText(SENTINEL.B)
        // (2) Absence: the FOREIGN sentinel appears nowhere in the DOM —
        // visible text AND full markup (hidden nodes included).
        await expect(page.locator('body')).not.toContainText(SENTINEL.A)
        expect(await page.content(), `foreign sentinel ${SENTINEL.A} leaked into the DOM`).not.toContain(SENTINEL.A)
      })
    }

    // ---- scale (@flow half; the @visual half rides the visualStates loop) ----
    if (isScaleState(c.scale)) {
      const s = c.scale
      test(`${c.name} scale ${s.name}`, { tag: '@flow' }, async ({ page }) => {
        const session = await mountState(page, c, s, 'dark')
        const rows = page.locator(s.domCap.selector)
        // Positive control: the detector sees data (vacuity guard, §5.6b pattern).
        await expect(rows.first()).toBeVisible()
        const n = await rows.count()
        expect(n, `scale positive control: '${s.domCap.selector}' must render rows`).toBeGreaterThan(0)
        expect(
          n,
          `DOM ceiling: ${n} rendered rows exceed the declared cap ${s.domCap.max} at ${SCALE_LABEL}`,
        ).toBeLessThanOrEqual(s.domCap.max)
        if (s.flow) await s.flow(page, session)
        // The page-typical interaction (load-more/scroll) must not blow the cap either.
        expect(await rows.count(), 'DOM ceiling after the scale interaction').toBeLessThanOrEqual(s.domCap.max)
      })
    }
  })
}

const SCALE_LABEL = '10k fixture items (design 06 §6.2)'
