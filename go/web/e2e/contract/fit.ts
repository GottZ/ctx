import { expect, type Page } from '@playwright/test'

// Viewport fit (#419, #430, #440): the layout assertion the contract runner
// makes on a settled state. Three layers, one page.evaluate:
//
//   1. document — documentElement never scrolls, neither down nor sideways
//      (#419 height, #430 width). The shell is a 100dvh grid whose content
//      region owns the scroll, and the login mask centres its card inside one
//      viewport.
//   2. clipped  — an element that clips sideways without being declared a
//      sideways scroller holds no content wider than its padding box
//      (scrollWidth <= clientWidth), and no box inside it starts left of its
//      padding box (the scroll range never reaches there, so scrollWidth does
//      not count it). overflow-y: auto alone computes overflow-x to auto, so a
//      box that is too wide inside such a region scrolls it sideways or is
//      cut off, and the document check cannot see it: main.content (#440),
//      the stacked blocks workbench (#439), a dialog body (#442); overflow
//      hidden or clip cuts it off where nobody can scroll to it (the graph and
//      chat page frames). A region whose stylesheet declares overflow-x auto
//      or scroll (also through the overflow shorthand) is a sideways scroller
//      by choice — the table wrappers of lib/ui/Table.svelte, the board
//      columns, main.content[data-mode='board'] (AppShell), code blocks — and
//      is not checked here; its own box still is, by layer 3.
//   3. outside  — the visible part of every rendered element box (its border
//      box, cut to the boxes of the clipping ancestors that clip it) lies
//      between x = 0 and the viewport width. This catches what layers 1 and 2
//      cannot: a box that sticks out on the left only (the scroll range never
//      reaches left of x = 0), and fixed and top-layer boxes (a modal dialog,
//      a popover), whose containing block is the viewport, not a scroller —
//      unless an ancestor holds fixed boxes (a transform, containment), which
//      then clips them like absolute ones.
//
// Clipping follows the containing block (#457): a box is cut by the clipping
// ancestors of its containing block, not by every clipping element above it
// in the DOM. An absolutely positioned box escapes a scroller that lies
// between it and its containing block (the nearest positioned ancestor, or
// one that holds fixed boxes): that scroller neither cuts it off (layer 2)
// nor hides it from the edge check (layer 3). A fixed box is cut by the
// clipping chain of the ancestor that holds it, else by the viewport alone.
//
// Layer 2 compares whole pixels, as the browser scrolls (#457, measured at
// device scale 1 and 2): Chromium snaps a region's sideways scroll range to
// whole pixels and scrollWidth > clientWidth holds exactly when that range is
// not 0, so the check reports a region the user can scroll sideways, and a
// sub-pixel overhang with no scroll range passes.
//
// Structural exemptions, no per-page list:
//   - a declared sideways scroller (layer 2, above): the overflow-x value that
//     wins the element's cascade is auto or scroll (cascadedX below), and an
//     author rule or the inline style declares it — the UA's overflow: auto
//     on a modal dialog or a popover is no author's choice. A rule whose
//     @media, @supports or @container condition does not hold cannot win the
//     cascade; the author check reads @media and @supports conditions and
//     counts @container rules always (no API evaluates a container condition
//     from script), so only a UA auto next to a matching but inactive
//     @container rule can exempt more.
//   - text-overflow: ellipsis clips on purpose; input, select and textarea
//     scroll their own value; layer 2 skips such an element.
//   - a subtree under clip-path or clip (the visually-hidden pattern: a 1 px
//     box whose label overflows it) is not painted where it lays out; neither
//     layer looks into it. So a decoratively clipped subtree is unchecked.
//   - svg and canvas are leaves: their own box is checked, their insides are
//     drawing coordinates, not layout.
//

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

    // Selectors of the style rules that declare overflow-x auto or scroll (the
    // overflow shorthand sets overflow-x too), in effect right now.
    const declaredX: string[] = []
    const collect = (rules: CSSRuleList): void => {
      for (const rule of Array.from(rules)) {
        if (rule instanceof CSSStyleRule) {
          const x = rule.style.overflowX
          if (x === 'auto' || x === 'scroll') declaredX.push(rule.selectorText)
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
    // An author declares overflow-x auto or scroll for the element: inline, or
    // in a matching rule in effect (declaredX).
    const authorDeclaresX = (el: Element): boolean => {
      const inline = el instanceof HTMLElement ? el.style.overflowX : ''
      if (inline === 'auto' || inline === 'scroll') return true
      return declaredX.some((sel) => matches(el, sel))
    }

    // The overflow-x value that won the cascade of each such element (#457).
    // The computed value cannot tell: visible computes to auto next to a
    // non-visible overflow-y, so a winning overflow-x: visible looks like a
    // scroller, and a losing auto rule still matches the element. With
    // overflow-y forced to visible (inline and !important, the strongest
    // author declaration) overflow-x computes to the value that won — the
    // browser's own cascade: specificity, order, layers, !important, inline.
    // Batched: all writes, then all reads, then every style attribute
    // restored as it was. Overflow is not inherited, so no element's probe
    // moves another's value; where auto or scroll won, the element stays a
    // scroll container throughout and keeps its scroll offset.
    const cascadedX = new Map<Element, string>()
    const scrollers = Array.from(document.body.querySelectorAll('*')).filter((el) => {
      const x = getComputedStyle(el).overflowX
      return (x === 'auto' || x === 'scroll') && authorDeclaresX(el)
    }) as HTMLElement[]
    const attrs = scrollers.map((el) => el.getAttribute('style'))
    for (const el of scrollers) el.style.setProperty('overflow-y', 'visible', 'important')
    for (const el of scrollers) cascadedX.set(el, getComputedStyle(el).overflowX)
    scrollers.forEach((el, i) => {
      const a = attrs[i]
      // set first: removing an attribute whose inline style was written through
      // the CSSOM leaves style="" behind in Chromium (measured)
      el.setAttribute('style', a ?? '')
      if (a === null) el.removeAttribute('style')
    })

    // A region is a declared sideways scroller when an author declared
    // overflow-x auto or scroll and that value won its cascade: a later
    // overflow: hidden or overflow-x: visible that wins leaves none (cs is the
    // element's computed style).
    const declaresX = (el: Element, cs: CSSStyleDeclaration): boolean => {
      if (cs.overflowX !== 'auto' && cs.overflowX !== 'scroll') return false
      const won = cascadedX.get(el)
      return won === 'auto' || won === 'scroll'
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

    // An element that becomes the containing block of its position: fixed
    // descendants (CSS Transforms, Filter Effects, Containment, Will Change):
    // such a descendant is laid out and clipped like an absolute box in it.
    // container-type is not among them: it applies size and style
    // containment, not layout containment (measured: a fixed or absolute box
    // under container-type: inline-size is placed against the viewport).
    const holdsFixed = (cs: CSSStyleDeclaration): boolean =>
      cs.transform !== 'none' ||
      cs.translate !== 'none' ||
      cs.rotate !== 'none' ||
      cs.scale !== 'none' ||
      cs.perspective !== 'none' ||
      cs.transformStyle === 'preserve-3d' ||
      cs.filter !== 'none' ||
      cs.backdropFilter !== 'none' ||
      /\b(layout|paint|strict|content)\b/.test(cs.contain) ||
      cs.contentVisibility === 'auto' ||
      /\b(transform|translate|rotate|scale|perspective|filter|contain)\b/.test(cs.willChange)

    // The clipping a box is subject to: the horizontal clip of the clipping
    // ancestors of its containing block (left, right), and the nearest
    // implied scroller among them (layer 2) with its left padding edge — a box
    // starting left of it is cut off for good.
    interface Clip {
      left: number
      right: number
      scroller: Element | null
      scrollerLeft: number
    }
    const VIEWPORT: Clip = { left: -Infinity, right: Infinity, scroller: null, scrollerLeft: -Infinity }

    // flow: the clip of the in-flow boxes here; abs: the clip of the
    // containing block an absolutely positioned box here has (the nearest
    // positioned ancestor, or one that holds fixed boxes); fixed: the clip of
    // the ancestor that holds fixed boxes, else the viewport.
    const visit = (el: Element, flow: Clip, abs: Clip, fixed: Clip): void => {
      const cs = getComputedStyle(el)
      if (cs.display === 'none') return
      if (cs.clipPath !== 'none' || cs.clip !== 'auto') return
      // display: contents generates no box: position and transforms do not apply
      const box = cs.display !== 'contents'
      const pos = box ? cs.position : 'static'
      // the top layer escapes every ancestor, for its fixed descendants too
      const top = matches(el, ':modal') || matches(el, ':popover-open')
      const inc = top ? VIEWPORT : pos === 'absolute' ? abs : pos === 'fixed' ? fixed : flow
      let cl = inc.left
      let cr = inc.right
      let sc = inc.scroller
      let sl = inc.scrollerLeft
      if (box) {
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
                `which is not declared a sideways scroller`,
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
            cs.textOverflow === 'ellipsis' || matches(el, 'input, select, textarea') || declaresX(el, cs)
          if (!exempt && el.scrollWidth > el.clientWidth) {
            findings.push(
              `clipped: ${label(el)} holds ${el.scrollWidth} px of content in ${el.clientWidth} px ` +
                `(overflow-x ${cs.overflowX}, not declared a sideways scroller); widest: ${culprit(el, padLeft, padRight)}`,
            )
          }
          cl = Math.max(cl, padLeft)
          cr = Math.min(cr, padRight)
          // a declared sideways scroller owns what it scrolls
          sc = exempt ? null : el
          sl = exempt ? -Infinity : padLeft
        }
      }
      const out: Clip = { left: cl, right: cr, scroller: sc, scrollerLeft: sl }
      const holds = box && holdsFixed(cs)
      const childAbs = pos !== 'static' || holds ? out : abs
      const childFixed = holds ? out : top ? VIEWPORT : fixed
      for (const ch of Array.from(el.children)) visit(ch, out, childAbs, childFixed)
    }
    for (const ch of Array.from(document.body.children)) visit(ch, VIEWPORT, VIEWPORT, VIEWPORT)

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
