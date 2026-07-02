// Focus-walk (design 06 §4.5, wave PV5) — Eigenbau: axe carries NO rule for
// SC 2.4.7 (Focus Visible) / SC 2.4.11 (Focus Not Obscured) — research §3.3.
// Handmade precedent: the window focus tests (graph-windows.spec.ts §207-231);
// this generalizes them per contract:
//
//   Tab traversal over the page, per stop:
//     (a) visible :focus-visible indicator — computed style against the ONE
//         indicator model (app.css :focus-visible → --focus-ring token, Q3/E5);
//     (b) the focused element is not fully covered by overlays
//         (elementFromPoint at its clamped center).
//   Plus reachability: every visible interactive element is reached by the
//   walk (radio groups count as reached when ONE member is — roving focus via
//   arrow keys is the correct radiogroup pattern, not a Tab stop per segment).
//
// The walked tab ORDER is attached to the report (focus-walk-order) — the
// §4.1 "tab-Reihenfolge dokumentiert" duty as a machine artifact.
//
// The walk stamps visited elements with data-e2e-focus-stop for cycle
// detection + reachability; it runs in @a11y flow tests only, never before a
// visual capture.

import { expect, type Page, type TestInfo } from '@playwright/test'

/** Hard iteration ceiling — a page with more real tab stops is a design smell. */
const MAX_STOPS = 200

interface WalkStop {
  desc: string
  focusVisible: boolean
  indicator: boolean
  obscured: boolean
}

type StepResult = { cycled: true } | ({ cycled: false } & WalkStop) | null

export async function runFocusWalk(page: Page, testInfo: TestInfo): Promise<void> {
  // Deterministic start: nothing focused, walk begins at the document.
  await page.evaluate(() => {
    const el = document.activeElement
    if (el instanceof HTMLElement) el.blur()
  })

  const stops: WalkStop[] = []
  for (let i = 0; i < MAX_STOPS; i++) {
    await page.keyboard.press('Tab')
    const info: StepResult = await page.evaluate((idx) => {
      const el = document.activeElement
      if (!(el instanceof HTMLElement) || el === document.body) return null
      if (el.dataset.e2eFocusStop !== undefined) return { cycled: true as const }
      el.dataset.e2eFocusStop = String(idx)

      const cs = getComputedStyle(el)
      // ONE indicator model (app.css:26): the token ring renders as a computed
      // outline; box-shadow is accepted as an alternative indicator carrier.
      const indicator =
        (cs.outlineStyle !== 'none' && parseFloat(cs.outlineWidth) > 0) || cs.boxShadow !== 'none'

      // Not-obscured probe (SC 2.4.11): the element wins the hit test at its
      // own (viewport-clamped) center — an overlay fully covering it would.
      const r = el.getBoundingClientRect()
      const cx = Math.min(Math.max(r.left + r.width / 2, 0), window.innerWidth - 1)
      const cy = Math.min(Math.max(r.top + r.height / 2, 0), window.innerHeight - 1)
      const hit = document.elementFromPoint(cx, cy)
      const obscured = !(hit === el || (hit !== null && (el.contains(hit) || hit.contains(el))))

      const name = el.getAttribute('aria-label') ?? (el.textContent ?? '').trim().slice(0, 40)
      const desc = `${el.tagName.toLowerCase()}${el.id ? `#${el.id}` : ''}${
        el.className && typeof el.className === 'string' ? `.${el.className.trim().split(/\s+/)[0]}` : ''
      } "${name}"`
      return { cycled: false as const, desc, focusVisible: el.matches(':focus-visible'), indicator, obscured }
    }, i)

    // A leading absorbed Tab (headless-shell: the first Tab after a fresh
    // body-blur can land back on body — seen on the autofocus login mask, PV7)
    // is a warm-up, not the end: only treat a null as "walk done" once at
    // least one real stop is collected. A page with genuinely zero tab stops
    // still exhausts the loop and the positive control below fails (vacuity
    // guard intact).
    if (info === null) {
      if (stops.length > 0) break
      continue
    }
    if (info.cycled) break
    stops.push(info)
  }

  await testInfo.attach('focus-walk-order', {
    body: stops.map((s, i) => `${String(i + 1).padStart(3)}. ${s.desc}`).join('\n'),
    contentType: 'text/plain',
  })

  // Positive control (vacuity guard, §5.6b pattern): a page without a single
  // tab stop would make every per-stop assert vacuously green.
  expect(stops.length, 'focus walk positive control: the page must have tab stops').toBeGreaterThan(0)

  const noIndicator = stops.filter((s) => !(s.focusVisible && s.indicator)).map((s) => s.desc)
  expect(
    noIndicator,
    `focus stops without a visible :focus-visible indicator (SC 2.4.7):\n${noIndicator.join('\n')}`,
  ).toEqual([])

  const obscured = stops.filter((s) => s.obscured).map((s) => s.desc)
  expect(obscured, `focus stops obscured by overlays (SC 2.4.11):\n${obscured.join('\n')}`).toEqual([])

  // Reachability: every visible interactive element carries the walk stamp.
  const missed: string[] = await page.evaluate(() => {
    const visible = (el: HTMLElement): boolean =>
      el.offsetParent !== null || getComputedStyle(el).position === 'fixed'
    const describe = (el: HTMLElement): string =>
      `${el.tagName.toLowerCase()}${el.id ? `#${el.id}` : ''} "${
        el.getAttribute('aria-label') ?? (el.textContent ?? '').trim().slice(0, 40)
      }"`
    const out: string[] = []
    const radioGroups = new Map<Element | null, { reached: boolean; first: HTMLElement }>()
    const candidates = Array.from(
      document.querySelectorAll<HTMLElement>('a[href], button, input, select, textarea, [tabindex]'),
    )
    for (const el of candidates) {
      if (el.tabIndex < 0) continue // roving/managed focus opts out explicitly
      if ('disabled' in el && (el as HTMLButtonElement).disabled) continue
      if (!visible(el)) continue
      if (el.closest('[aria-hidden="true"]') !== null) continue
      const reached = el.dataset.e2eFocusStop !== undefined
      const isRadio =
        el.getAttribute('role') === 'radio' || (el instanceof HTMLInputElement && el.type === 'radio')
      if (isRadio) {
        // Radiogroup = ONE tab stop; arrows move inside (correct roving pattern).
        const group = el.closest('[role="radiogroup"]') ?? el.parentElement
        const g = radioGroups.get(group)
        if (g === undefined) radioGroups.set(group, { reached, first: el })
        else if (reached) g.reached = true
        continue
      }
      if (!reached) out.push(describe(el))
    }
    for (const [, g] of radioGroups) {
      if (!g.reached) out.push(`radiogroup of ${describe(g.first)} (no member reached)`)
    }
    return out
  })
  expect(missed, `interactive elements not reachable via Tab:\n${missed.join('\n')}`).toEqual([])
}
