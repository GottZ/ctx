// The block view of a recording (issue #575): the ORIGINAL is the reference
// and is shown as plain text — never Markdown, never HTML; the TRANSLATION a
// model wrote is Markdown and renders only through the app's one sanitizing
// renderer (lib/markdown/Markdown.svelte: markdown-it html:false + DOMPurify +
// remote-image placeholder + foreign-link hardening). The same hostile string
// stands in both: as text it must stay text, as Markdown it must come out
// without a script, an on* handler, a javascript: link or a remote <img>.
//
// Server-rendered (svelte/server, the ConsumerRow/BlockDetail pattern) and
// parsed into a jsdom document, so the assertions read ELEMENTS, not strings
// (an escaped "<script>" as text is harmless; a <script> element is not).
// Why not the jsdom test environment pragma: under it the component compiles for
// the client while `svelte` still resolves to its server build — neither
// render() nor mount() works (probed). So the node environment renders, and a
// jsdom window is installed as the global window BEFORE the component's module
// graph loads: DOMPurify binds the window it finds at import time, so the
// Markdown pipeline under test is the real, sanitizing one (markdown.test.ts
// runs it the same way). Targets of the two mutants of the wave: the
// translation through a raw {@html} instead of <Markdown> ⇒ red; the original
// through <Markdown> ⇒ red.

import { describe, expect, it, vi } from 'vitest'
// @ts-expect-error -- jsdom (devDependency, the vitest DOM) ships no .d.ts and the lockfile carries no @types/jsdom
import { JSDOM } from 'jsdom'
import { render } from 'svelte/server'
import type { RecordingBlock as Block } from '../../lib/api/types'

const win = new JSDOM('<!doctype html><html><body></body></html>').window as Window & typeof globalThis
// DOMPurify reads the global `window` only (it takes document & co. from it)
Object.assign(globalThis, { window: win })
// dynamic: these load DOMPurify, which must see the window above
const { default: RecordingBlock } = await import('./RecordingBlock.svelte')
const { BlockModel } = await import('./recordings.svelte')
type BlockModel = InstanceType<typeof BlockModel>

const HOSTILE = [
  'Plain line.',
  '<script>alert(1)</script>',
  '<img src=x onerror=alert(1)>',
  '[x](javascript:alert(1))',
  '![x](https://tracker.example/p.png)',
  'see https://example.org/plain and [ctx](https://example.org/doc)',
].join('\n')

function block(over: Partial<Block> = {}): Block {
  return {
    address: 'request/messages/1',
    side: 'request',
    kind: 'text',
    role: 'user',
    bytes: new TextEncoder().encode(HOSTILE).length,
    utf8: true,
    translatable: true,
    inline: true,
    preview: HOSTILE,
    preview_truncated: false,
    preview_omitted: false,
    ...over,
  }
}

function model(b: Block = block()): BlockModel {
  return new BlockModel({ read: vi.fn(), translate: vi.fn() }, 'rec-1', b, 'de')
}

function dom(m: BlockModel): HTMLElement {
  const root = win.document.createElement('div')
  root.innerHTML = render(RecordingBlock, { props: { model: m } }).body
  return root
}

function hasEventHandler(root: Element): boolean {
  return [root, ...root.querySelectorAll('*')].some((el) => [...el.attributes].some((a) => a.name.toLowerCase().startsWith('on')))
}

describe('RecordingBlock — the original is text', () => {
  it('shows the hostile original byte for byte as the text of one <pre>, no element inside', () => {
    const root = dom(model())
    const orig = root.querySelector('[data-original]')
    expect(orig, 'the original is rendered').not.toBeNull()
    expect(orig?.tagName).toBe('PRE')
    expect(orig?.textContent).toBe(HOSTILE)
    expect(orig?.children.length, 'text only — no element of any kind').toBe(0)
    expect(root.querySelector('script')).toBeNull()
    expect(root.querySelector('img')).toBeNull()
    expect(root.querySelector('[data-original] a')).toBeNull()
    expect(hasEventHandler(root)).toBe(false)
  })

  it('a remote image block names its URL as text, never an <img>', () => {
    const url = 'https://tracker.example/p.png'
    const root = dom(model(block({ kind: 'image', role: 'user', preview: url, bytes: url.length, translatable: false, media_type: undefined })))
    expect(root.querySelector('img')).toBeNull()
    expect(root.querySelector('[data-original]')?.textContent).toBe(url)
  })

  it('a block whose labels the server cut says so; one with whole labels does not', () => {
    const cut = dom(model(block({ name: 'n'.repeat(256), labels_truncated: true })))
    expect(cut.querySelector('[data-labels-cut]')?.textContent).toBe('labels cut')
    expect(dom(model(block({ name: 'bob' }))).querySelector('[data-labels-cut]')).toBeNull()
  })

  it('a payload shows its media type and size, no preview and no image', () => {
    const root = dom(model(block({ kind: 'image', media_type: 'image/png', bytes: 2048, inline: false, preview: '', preview_omitted: true, translatable: false })))
    expect(root.querySelector('img')).toBeNull()
    expect(root.textContent).toContain('image/png')
    expect(root.textContent).toContain('2.0 KiB')
  })
})

describe('RecordingBlock — the translation renders through the one sanitizing renderer', () => {
  function translated(): HTMLElement {
    const m = model()
    const t = m.translation
    if (t === null) throw new Error('translatable block without a translation model')
    t.parts = [HOSTILE]
    t.chunks = 1
    t.model = 'qwen38-27b'
    t.status = 'done'
    return dom(m)
  }

  it('is marked as generated, names the model, and stands next to the unchanged original', () => {
    const root = translated()
    const tr = root.querySelector('[data-translation]')
    expect(tr).not.toBeNull()
    expect(root.textContent).toContain('Generated by a model — the original is the reference')
    expect(root.textContent).toContain('qwen38-27b')
    const orig = root.querySelector('[data-original]')
    expect(orig?.textContent).toBe(HOSTILE)
    expect(orig?.children.length).toBe(0)
  })

  it('emits no <script>, no on* handler, no javascript: link and no remote <img>', () => {
    const tr = translated().querySelector('[data-translation]')
    if (tr === null) throw new Error('no translation')
    // positive control: the Markdown really rendered (a paragraph, the links)
    expect(tr.querySelector('.md p')).not.toBeNull()
    expect(tr.querySelector('script')).toBeNull()
    expect(hasEventHandler(tr)).toBe(false)
    const links = [...tr.querySelectorAll('a')]
    expect(links.every((a) => !(a.getAttribute('href') ?? '').trim().toLowerCase().startsWith('javascript:'))).toBe(true)
    expect([...tr.querySelectorAll('img')].filter((i) => (i.getAttribute('src') ?? '').includes('tracker.example'))).toEqual([])
    expect(tr.querySelector('img'), 'no image element at all from a translation').toBeNull()
    const ph = tr.querySelector('span.md-img-blocked')
    expect(ph?.getAttribute('data-camo-src')).toBe('https://tracker.example/p.png')
  })

  it('opens a foreign link only with rel noopener noreferrer', () => {
    const tr = translated().querySelector('[data-translation]')
    const foreign = [...(tr?.querySelectorAll('a') ?? [])].filter((a) => (a.getAttribute('href') ?? '').startsWith('https://example.org'))
    expect(foreign.length, 'positive control: the foreign links exist').toBe(2)
    for (const a of foreign) {
      const rel = (a.getAttribute('rel') ?? '').split(/\s+/)
      expect(rel).toContain('noopener')
      expect(rel).toContain('noreferrer')
    }
  })
})
