import { expect, type Page } from '@playwright/test'

// Viewport fit (#419, #430, #440): the layout assertion the contract runner
// makes on a settled state. Three layers, one page.evaluate:
//
//   1. document — documentElement never scrolls, neither down nor sideways
//      (#419 height, #430 width). The shell is a 100dvh grid whose content
//      region owns the scroll, and the login mask centres its card inside one
//      viewport.
//   2. clipped  — an element that scrolls sideways only because it scrolls
//      down holds no content wider than its padding box (scrollWidth <=
//      clientWidth), and no box inside it starts left of its padding box (the
//      scroll range never reaches there, so scrollWidth does not count it).
//      overflow-y: auto alone computes overflow-x to auto, so a
//      box that is too wide inside such a region scrolls it sideways or is
//      cut off, and the document check cannot see it: main.content (#440),
//      the stacked blocks workbench (#439), a dialog body (#442). A region
//      whose stylesheet declares overflow-x (or the overflow shorthand) other
//      than visible chose to clip or scroll sideways — the table wrappers of
//      lib/ui/Table.svelte, the board columns, main.content[data-mode='board']
//      (AppShell), code blocks — and is not checked here; its own box still
//      is, by layer 3.
//   3. outside  — the visible part of every rendered element box (its border
//      box, cut to the boxes of the clipping ancestors above it) lies between
//      x = 0 and the viewport width. This catches what layers 1 and 2 cannot:
//      a box that sticks out on the left only (the scroll range never reaches
//      left of x = 0), and fixed and top-layer boxes (a modal dialog, a
//      popover), whose containing block is the viewport, not a scroller.
//
// Structural exemptions, no per-page list:
//   - declared sideways overflow (layer 2, above). Rules inside @media and
//     @supports count only while their condition holds; rules inside
//     @container count always (no API evaluates a container condition from
//     script), which can only exempt more, never fail a fitting box.
//   - text-overflow: ellipsis clips on purpose; input, select and textarea
//     scroll their own value; layer 2 skips such an element.
//   - a subtree under clip-path or clip (the visually-hidden pattern: a 1 px
//     box whose label overflows it) is not painted where it lays out; neither
//     layer looks into it. So a decoratively clipped subtree is unchecked.
//   - svg and canvas are leaves: their own box is checked, their insides are
//     drawing coordinates, not layout.
//
// Known limit: an absolutely positioned box is cut to every clipping ancestor
// above it in the DOM, also to one that is not in its containing-block chain
// and therefore does not clip it. That can hide such a box from layer 3; it
// cannot make a fitting box fail.

/**
 * Sub-pixel slack for layer 3: boxes are compared at fractional precision, and
 * a centred box at an odd width can land half a pixel off. A box 1 px too wide
 * or 1 px off the edge still fails.
 */
const EDGE_SLACK_PX = 0.5

/** At most this many findings are listed in a failure message. */
const MAX_LISTED = 12

interface FitMeasure {
  scrollHeight: number
  clientHeight: number
  scrollWidth: number
  clientWidth: number
  findings: string[]
}

/**
 * Assert the three layers on the current page. Called on the settled state:
 * before every visual capture, and in the mobile fit test of a contract that
 * opts out of the mobile capture (contract.ts).
 */
export async function expectViewportFit(page: Page): Promise<void> {
  // The fit measures text runs: wait for the web font (JetBrains Mono) as the
  // capture does (stabilize, capture.ts), so a late font cannot move a box.
  await page.evaluate(async () => {
    await document.fonts.ready
  })
  const m = await page.evaluate((slack): FitMeasure => {
    const root = document.documentElement
    const vw = root.clientWidth
    const findings: string[] = []

    const matches = (el: Element, sel: string): boolean => {
      try {
        return el.matches(sel)
      } catch {
        return false
      }
    }

    // Selectors of the style rules that declare overflow-x other than visible
    // (the overflow shorthand sets overflow-x too), in effect right now.
    const declaredX: string[] = []
    const collect = (rules: CSSRuleList): void => {
      for (const rule of Array.from(rules)) {
        if (rule instanceof CSSStyleRule) {
          const x = rule.style.overflowX
          if (x !== '' && x !== 'visible') declaredX.push(rule.selectorText)
        }
        if (rule instanceof CSSMediaRule && !window.matchMedia(rule.conditionText).matches) continue
        if (rule instanceof CSSSupportsRule && !CSS.supports(rule.conditionText)) continue
        if ('cssRules' in rule && rule.cssRules instanceof CSSRuleList) collect(rule.cssRules)
      }
    }
    for (const sheet of Array.from(document.styleSheets)) {
      try {
        collect(sheet.cssRules)
      } catch {
        // a cross-origin sheet hides its rules; the SPA ships none
      }
    }
    const declaresX = (el: Element): boolean => {
      const inline = el instanceof HTMLElement ? el.style.overflowX : ''
      if (inline !== '' && inline !== 'visible') return true
      return declaredX.some((sel) => matches(el, sel))
    }

    const label = (el: Element): string => {
      const parts: string[] = []
      let e: Element | null = el
      while (e !== null && e !== document.body && parts.length < 3) {
        const cls = Array.from(e.classList)
          .filter((c) => !c.startsWith('svelte-'))
          .map((c) => `.${c}`)
          .join('')
        const mode = e.getAttribute('data-mode')
        parts.unshift(`${e.tagName.toLowerCase()}${cls}${mode !== null ? `[data-mode='${mode}']` : ''}`)
        e = e.parentElement
      }
      return parts.join(' > ')
    }
    const px = (n: number): string => `${Math.round(n * 10) / 10}`

    // The descendant box reaching furthest beyond a clipping element's padding
    // box — names the culprit in the message (computed only on a finding).
    const culprit = (el: Element, left: number, right: number): string => {
      let worst: Element | null = null
      let over = 0
      for (const d of Array.from(el.querySelectorAll('*'))) {
        const r = d.getBoundingClientRect()
        if (r.width === 0 || r.height === 0) continue
        const o = Math.max(left - r.left, r.right - right)
        if (o > over) {
          over = o
          worst = d
        }
      }
      return worst === null
        ? 'text or inline content'
        : `${label(worst)} (${px(worst.getBoundingClientRect().width)} px wide)`
    }

    // scroller/scrollerLeft: the nearest implied scroller above (layer 2) and
    // its left padding edge; a box starting left of it is cut off for good.
    const visit = (
      el: Element,
      clipLeft: number,
      clipRight: number,
      scroller: Element | null,
      scrollerLeft: number,
    ): void => {
      const cs = getComputedStyle(el)
      if (cs.display === 'none') return
      if (cs.clipPath !== 'none' || cs.clip !== 'auto') return
      let cl = clipLeft
      let cr = clipRight
      let sc = scroller
      let sl = scrollerLeft
      if (cs.position === 'fixed' || matches(el, ':modal') || matches(el, ':popover-open')) {
        cl = -Infinity
        cr = Infinity
        sc = null
        sl = -Infinity
      }
      if (cs.display !== 'contents') {
        const r = el.getBoundingClientRect()
        if (r.width > 0 && r.height > 0 && cs.visibility !== 'hidden') {
          const left = Math.max(r.left, cl)
          const right = Math.min(r.right, cr)
          if (left < right && (left < -slack || right > vw + slack)) {
            findings.push(
              `outside: ${label(el)} spans x ${px(left)}..${px(right)} in the ${vw} px viewport (${px(r.width)} px wide)`,
            )
          }
          if (sc !== null && r.left < sl - slack) {
            findings.push(
              `clipped: ${label(el)} starts at x ${px(r.left)}, left of the padding edge ${px(sl)} of ${label(sc)}, ` +
                `which scrolls sideways only through overflow-y`,
            )
            // its descendants are cut off with it; one finding names the box
            sc = null
            sl = -Infinity
          }
        }
        if (el instanceof SVGElement || el instanceof HTMLCanvasElement) return
        if (cs.overflowX !== 'visible' && cs.display !== 'inline') {
          const padLeft = r.left + parseFloat(cs.borderLeftWidth)
          const padRight = padLeft + el.clientWidth
          const exempt =
            cs.textOverflow === 'ellipsis' || matches(el, 'input, select, textarea') || declaresX(el)
          if (!exempt && el.scrollWidth > el.clientWidth) {
            findings.push(
              `clipped: ${label(el)} holds ${el.scrollWidth} px of content in ${el.clientWidth} px ` +
                `(overflow-x ${cs.overflowX} only through overflow-y ${cs.overflowY}); widest: ${culprit(el, padLeft, padRight)}`,
            )
          }
          cl = Math.max(cl, padLeft)
          cr = Math.min(cr, padRight)
          // a region that declares its sideways overflow owns what it clips
          sc = exempt ? null : el
          sl = exempt ? -Infinity : padLeft
        }
      }
      for (const ch of Array.from(el.children)) visit(ch, cl, cr, sc, sl)
    }
    for (const ch of Array.from(document.body.children)) visit(ch, -Infinity, Infinity, null, -Infinity)

    return {
      scrollHeight: root.scrollHeight,
      clientHeight: root.clientHeight,
      scrollWidth: root.scrollWidth,
      clientWidth: vw,
      findings,
    }
  }, EDGE_SLACK_PX)

  expect(
    m.scrollHeight,
    `the document is ${m.scrollHeight - m.clientHeight} px taller than the ${m.clientHeight} px viewport (#419)`,
  ).toBe(m.clientHeight)
  expect(
    m.scrollWidth,
    `the document is ${m.scrollWidth - m.clientWidth} px wider than the ${m.clientWidth} px viewport (#430)`,
  ).toBe(m.clientWidth)
  const listed = m.findings.slice(0, MAX_LISTED)
  const more = m.findings.length - listed.length
  expect(
    m.findings,
    `${m.findings.length} box(es) do not fit sideways (#440):\n${listed.join('\n')}${more > 0 ? `\n… and ${more} more` : ''}`,
  ).toEqual([])
}
