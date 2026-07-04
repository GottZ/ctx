// Reduced-motion walk (design 05-§4.4 Lint-Grenze 2 / §7 Q12 gate) — proves the
// motion system is ABSCHALTBAR. Sibling of focus.ts in the 06 walk family (PV5):
// a DOM sweep, no baselines, runs in @a11y flow only.
//
// Under prefers-reduced-motion the global app.css guard (@media block) forces
// every animation-/transition-duration to a near-zero literal via `*`, so no
// element carries a running duration. The walk sweeps every element and asserts
// exactly that.
//
// The gate is deliberately TRANSITION-anchored, not animation-anchored: the long
// component animations (Wordmark pulse, Login rise, MessageBubble cursor) each
// ALSO carry a local `animation: none` reduced-motion guard, so an animation-only
// sweep would stay green even with the GLOBAL guard removed (a vacuous ROT-Beweis).
// The button/input transitions (app.css) rely on the global guard ALONE — their
// computed transition-duration is the real proof: remove the global block and
// every var(--dur-1) transition reports 120ms and the walk goes red.
//
// The signature element (Wordmark-Puls, design 05-§4.9) is additionally asserted
// silenceable by name — it is the ONE bold motion in the system and MUST be
// switch-off-able (frontend-design Qualitätsboden).

import { expect, type Page, type TestInfo } from '@playwright/test'

// The guard forces 0.01ms (= 0.00001s); real token motion is >= 120ms (0.12s).
// A 1ms ceiling sits three orders of magnitude below any real duration and well
// above the guard's near-zero literal.
const MAX_DURATION_S = 0.001

interface MotionReport {
  emulationActive: boolean
  running: string[]
  transitionCarriers: number
  animationCarriers: number
  signature: { found: boolean; disabled: boolean; detail: string }
}

export async function runReducedMotionWalk(page: Page, testInfo: TestInfo): Promise<void> {
  // Emulate explicitly (self-contained, not reliant on a test.use fixture): the
  // media query re-evaluates live, so it takes effect on the already-loaded page.
  await page.emulateMedia({ reducedMotion: 'reduce' })

  const report: MotionReport = await page.evaluate((eps) => {
    const maxDur = (v: string): number => Math.max(0, ...v.split(',').map((s) => parseFloat(s) || 0))
    const describe = (el: Element): string => {
      const cls =
        typeof el.className === 'string' && el.className.trim() !== ''
          ? '.' + el.className.trim().split(/\s+/)[0]
          : ''
      return `${el.tagName.toLowerCase()}${el.id ? '#' + el.id : ''}${cls}`
    }

    const running: string[] = []
    let transitionCarriers = 0
    let animationCarriers = 0

    for (const el of Array.from(document.querySelectorAll('*'))) {
      const cs = getComputedStyle(el)
      if (cs.animationName !== '' && cs.animationName !== 'none') {
        animationCarriers++
        if (maxDur(cs.animationDuration) > eps) {
          running.push(`animation ${describe(el)} name=${cs.animationName} dur=${cs.animationDuration}`)
        }
      }
      // A carrier is an element with an EXPLICIT transition-property list; the
      // 'all'/'none' initial value is not a declared transition.
      const tp = cs.transitionProperty
      if (tp !== '' && tp !== 'all' && tp !== 'none') {
        transitionCarriers++
        if (maxDur(cs.transitionDuration) > eps) {
          running.push(`transition ${describe(el)} prop=${tp} dur=${cs.transitionDuration}`)
        }
      }
    }

    // Signature element: the Wordmark cursor pulse (design 05-§4.9).
    const cursor = document.querySelector('.wordmark .cursor')
    let signature = { found: false, disabled: false, detail: 'wordmark cursor not on this page' }
    if (cursor !== null) {
      const cs = getComputedStyle(cursor)
      const disabled = cs.animationName === 'none' || maxDur(cs.animationDuration) <= eps
      signature = { found: true, disabled, detail: `name=${cs.animationName} dur=${cs.animationDuration}` }
    }

    const emulationActive = window.matchMedia('(prefers-reduced-motion: reduce)').matches
    return { emulationActive, running, transitionCarriers, animationCarriers, signature }
  }, MAX_DURATION_S)

  await testInfo.attach('reduced-motion-report', {
    body:
      `reduced-motion emulation active: ${report.emulationActive}\n` +
      `transition carriers: ${report.transitionCarriers}\n` +
      `animation carriers: ${report.animationCarriers}\n` +
      `signature (wordmark pulse): ${report.signature.found ? report.signature.detail : 'absent'}\n` +
      `running under reduced-motion:\n${report.running.join('\n') || '(none)'}`,
    contentType: 'text/plain',
  })

  // Vacuity guard #1: the emulation MUST actually be in effect — a walk against
  // a page that is NOT in reduced-motion mode would prove nothing (every guard is
  // a `prefers-reduced-motion` @media block).
  expect(report.emulationActive, 'prefers-reduced-motion emulation is not active on the page').toBe(true)

  // Positive control (vacuity guard #2, §5.6b pattern): the page MUST carry
  // declared transitions — otherwise "nothing runs" is trivially true and the
  // global guard is never exercised.
  expect(
    report.transitionCarriers,
    'reduced-motion positive control: the page must carry declared transitions (buttons/inputs)',
  ).toBeGreaterThan(0)

  // The gate: nothing carries a running duration under prefers-reduced-motion.
  expect(
    report.running,
    `elements with a running duration under prefers-reduced-motion ` +
      `(the global guard must zero them; a non-empty list means motion is NOT abschaltbar):\n${report.running.join('\n')}`,
  ).toEqual([])

  // The signature element, where present, MUST be silenceable.
  if (report.signature.found) {
    expect(
      report.signature.disabled,
      `signature element (wordmark pulse) not disabled under reduced-motion: ${report.signature.detail}`,
    ).toBe(true)
  }
}
