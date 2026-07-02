import { expect, type Locator, type Page } from '@playwright/test'

// Capture helper (design 06 §4.3, wave PV3): determinism before tolerance.
// One entry point for every visual assert — the PageContract runner (PV4)
// routes through here too, so the mask meta-guard below gates ALL captures.
//
// Masking convention: components with inherently non-deterministic pixel
// output carry `data-e2e-mask` (first carriers: the sigma canvases — the
// ForceAtlas2 layout is not seed-stable). That attribute is ALWAYS masked;
// contract-local `mask` selectors add to it.
//
// Mask meta-guard (§4.3 — the vacuity knobs are themselves gated): masks are
// policy-as-data and could otherwise hollow out the gate (`mask: ['body']` =
// green nothing). Three rules, enforced per capture:
//   (a) forbidden list — body/html/.shell/main.content (and any selector that
//       RESOLVES to them) are hard-forbidden mask targets ⇒ error;
//   (b) area budget — the summed boundingBox area of all masked regions may
//       not exceed 40 % of the viewport ⇒ error, EXCEPT via an explicit
//       maskBudgetOverride { reason, issue } (full-bleed canvas pages are the
//       legitimate case);
//   (c) visibility — overrides carry non-empty reason AND issue, surfaced in
//       COVERAGE.md from PV4 on.

/** Attribute selector that is always masked (design 06 §4.3). */
export const E2E_MASK_ATTR = '[data-e2e-mask]'

/** Summed masked area may not exceed this share of the viewport. */
export const MASK_AREA_BUDGET = 0.4

export interface MaskBudgetOverride {
  /** Why the budget is legitimately exceeded (e.g. full-bleed sigma canvas). */
  reason: string
  /** Tracking reference — GitHub issue or design-doc anchor until Achse 02 issues exist. */
  issue: string
}

export interface CaptureOptions {
  /** Contract-local mask selectors, additional to [data-e2e-mask]. */
  mask?: string[]
  /** The ONLY exception to the 40 % mask budget — reason + issue mandatory. */
  maskBudgetOverride?: MaskBudgetOverride
  /** Element shot instead of viewport shot (e.g. nav.rail). */
  target?: Locator
  fullPage?: boolean
}

/**
 * Settle non-deterministic render inputs before capture. Fonts: await
 * document.fonts.ready ourselves instead of attributing it to the framework
 * (research §7.4 — costs nothing). Motion: animations:'disabled' is already
 * the toHaveScreenshot default policy (playwright.config.ts expect block).
 */
export async function stabilize(page: Page): Promise<void> {
  await page.evaluate(() => document.fonts.ready)
}

interface MaskAudit {
  forbidden: string[]
  invalid: string[]
  maskedArea: number
  viewportArea: number
}

/**
 * Visual capture with mask meta-guard. `name` is the {arg} of the
 * snapshotPathTemplate: `<page>--<state>--<theme>--<viewport>.png`.
 */
export async function captureShot(page: Page, name: string, opts: CaptureOptions = {}): Promise<void> {
  const selectors = [E2E_MASK_ATTR, ...(opts.mask ?? [])]

  const audit = await page.evaluate((sels): MaskAudit => {
    const forbidden: string[] = []
    const invalid: string[] = []
    let maskedArea = 0
    for (const sel of sels) {
      let els: Element[]
      try {
        els = Array.from(document.querySelectorAll(sel))
      } catch {
        invalid.push(sel)
        continue
      }
      if (
        els.some(
          (el) =>
            el === document.body || el === document.documentElement || el.matches('.shell, main.content'),
        )
      ) {
        forbidden.push(sel)
        continue
      }
      for (const el of els) {
        const r = el.getBoundingClientRect()
        maskedArea += Math.max(0, r.width) * Math.max(0, r.height)
      }
    }
    return { forbidden, invalid, maskedArea, viewportArea: window.innerWidth * window.innerHeight }
  }, selectors)

  if (audit.invalid.length > 0) {
    throw new Error(`mask meta-guard: invalid mask selector(s): ${audit.invalid.join(', ')}`)
  }
  if (audit.forbidden.length > 0) {
    throw new Error(
      `mask meta-guard: forbidden mask target(s): ${audit.forbidden.join(', ')} — ` +
        `body, html, .shell and main.content (and selectors resolving to them) are hard-forbidden ` +
        `(design 06 §4.3: masking the page shell would turn the visual gate into a green nothing)`,
    )
  }

  const ratio = audit.viewportArea > 0 ? audit.maskedArea / audit.viewportArea : 0
  if (ratio > MASK_AREA_BUDGET) {
    const ov = opts.maskBudgetOverride
    if (!ov) {
      throw new Error(
        `mask meta-guard: mask budget exceeded — masked regions cover ${(ratio * 100).toFixed(1)} % ` +
          `of the viewport (budget ${MASK_AREA_BUDGET * 100} %). Full-bleed canvas pages may declare ` +
          `maskBudgetOverride { reason, issue } (design 06 §4.3).`,
      )
    }
    if (ov.reason.trim() === '' || ov.issue.trim() === '') {
      throw new Error(
        'mask meta-guard: maskBudgetOverride requires a non-empty reason AND issue reference (design 06 §4.3)',
      )
    }
  }

  await stabilize(page)
  const mask = selectors.map((s) => page.locator(s))
  if (opts.target) {
    await expect(opts.target).toHaveScreenshot(name, { mask })
  } else {
    await expect(page).toHaveScreenshot(name, { mask, fullPage: opts.fullPage ?? false })
  }
}
