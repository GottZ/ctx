// @vitest-environment jsdom
//
// Camo client + post-render swap unit tests (design 07-camo §4.3, D2b).
//  1. signImages: batch shape, de-dup, fail-safe empty-map on any error.
//  2. camoProxy action (issue markdown): swaps signed placeholders for
//     same-origin <img>, keeps the placeholder for declined/failed URLs.
//  3. camoClickToLoad action (LLM output, GHSA 2026-10-31 §7): no sign request
//     on render or re-render; one click signs exactly one URL; the approval
//     survives a re-render; query-string URLs get no button and no request.

import { afterEach, describe, expect, it, vi } from 'vitest'
import { signImages } from '../api/images'
import { camoClickToLoad, camoProxy, describeImageURL } from './camo'
import { renderMarkdown } from './markdown'

function stubFetch(handler: (url: string, init?: RequestInit) => unknown): void {
  vi.stubGlobal('fetch', (url: string, init?: RequestInit) =>
    Promise.resolve({
      ok: true,
      status: 200,
      headers: { get: () => null } as unknown as Headers,
      text: () => Promise.resolve(JSON.stringify(handler(url, init))),
    } as Response),
  )
}

afterEach(() => vi.unstubAllGlobals())

describe('signImages', () => {
  it('POSTs /api/img/sign with the de-duplicated url set and returns the map', async () => {
    let captured: { url: string; body: unknown } = { url: '', body: null }
    stubFetch((url, init) => {
      captured = { url, body: JSON.parse(String(init?.body)) }
      return { success: true, signatures: { 'https://e.com/a.png': '/api/img/sig1?url=x&exp=1' } }
    })
    const out = await signImages(['https://e.com/a.png', 'https://e.com/a.png', ''], 'issue')
    expect(captured.url).toBe('/api/img/sign')
    expect(captured.body).toEqual({ urls: ['https://e.com/a.png'], origin: 'issue' })
    expect(out).toEqual({ 'https://e.com/a.png': '/api/img/sig1?url=x&exp=1' })
  })

  it('resolves to an empty map on an empty input (no request)', async () => {
    const spy = vi.fn()
    vi.stubGlobal('fetch', spy)
    expect(await signImages([], 'issue')).toEqual({})
    expect(spy).not.toHaveBeenCalled()
  })

  it('degrades to an empty map when the endpoint fails (proxy off / rate limit)', async () => {
    vi.stubGlobal('fetch', () =>
      Promise.resolve({
        ok: false,
        status: 404,
        headers: { get: () => null } as unknown as Headers,
        text: () => Promise.resolve(JSON.stringify({ success: false, error: 'not enabled' })),
      } as Response),
    )
    expect(await signImages(['https://e.com/a.png'], 'assistant')).toEqual({})
  })

  it('degrades to an empty map on a network throw', async () => {
    vi.stubGlobal('fetch', () => Promise.reject(new Error('offline')))
    expect(await signImages(['https://e.com/a.png'], 'assistant')).toEqual({})
  })
})

// Render foreign-image markdown → the pipeline leaves a placeholder carrying
// data-camo-src; mount the action on a container holding that HTML.
function mount(md: string): HTMLDivElement {
  const div = document.createElement('div')
  div.innerHTML = renderMarkdown(md)
  document.body.appendChild(div)
  return div
}

describe('camoProxy action (issue markdown)', () => {
  it('swaps a signed placeholder for a same-origin <img>, dropping the placeholder', async () => {
    let body: unknown = null
    stubFetch((_url, init) => {
      body = JSON.parse(String(init?.body))
      return {
      success: true,
      signatures: { 'https://evil.example/pixel.png': '/api/img/abc123?url=https%3A%2F%2Fevil.example%2Fpixel.png&exp=9999999999' },
      }
    })
    const div = mount('![shot](https://evil.example/pixel.png)')
    expect(div.querySelector('.md-img-blocked')).not.toBeNull()

    camoProxy(div, div.innerHTML)
    await vi.waitFor(() => {
      const img = div.querySelector('img.md-img')
      expect(img).not.toBeNull()
      expect(img?.getAttribute('src')).toBe('/api/img/abc123?url=https%3A%2F%2Fevil.example%2Fpixel.png&exp=9999999999')
    })
    // The placeholder is gone and the src is a same-origin proxy PATH — the
    // foreign host only survives percent-encoded inside the url query, never as
    // the request origin (the browser fetches from 'self').
    expect(div.querySelector('.md-img-blocked')).toBeNull()
    const src = div.querySelector('img')?.getAttribute('src') ?? ''
    expect(src.startsWith('/api/img/')).toBe(true)
    expect(body).toEqual({ urls: ['https://evil.example/pixel.png'], origin: 'issue' })
    div.remove()
  })

  it('keeps the placeholder when the server declines the URL (fallback)', async () => {
    stubFetch(() => ({ success: true, signatures: {} })) // declined
    const div = mount('![shot](https://evil.example/pixel.png)')
    camoProxy(div, div.innerHTML)
    // Give the microtask a tick; the placeholder must survive.
    await new Promise((r) => setTimeout(r, 0))
    expect(div.querySelector('.md-img-blocked')).not.toBeNull()
    expect(div.querySelector('img')).toBeNull()
    div.remove()
  })

  it('does nothing when there are no foreign-image placeholders', async () => {
    const spy = vi.fn()
    vi.stubGlobal('fetch', spy)
    const div = mount('a plain [link](/local) and `code`')
    camoProxy(div, div.innerHTML)
    await new Promise((r) => setTimeout(r, 0))
    expect(spy).not.toHaveBeenCalled()
    div.remove()
  })
})

describe('camoClickToLoad action (LLM output, GHSA 2026-10-31 §7)', () => {
  const tick = () => new Promise((r) => setTimeout(r, 0))
  const signed = (u: string) => `/api/img/s1?url=${encodeURIComponent(u)}&exp=9999999999`

  it('signs nothing on render or on any re-render (streaming)', async () => {
    const spy = vi.fn()
    vi.stubGlobal('fetch', spy)
    const div = mount('![a](https://evil.example/a.png) ![b](https://evil.example/b.png)')
    const action = camoClickToLoad(div, div.innerHTML)
    for (const md of ['![a](https://evil.example/a.png) more', '![a](https://evil.example/a.png) more ![c](https://evil.example/c.png)']) {
      div.innerHTML = renderMarkdown(md) // the {@html} block re-renders
      action.update()
    }
    await tick()
    expect(spy).not.toHaveBeenCalled()
    expect(div.querySelector('img')).toBeNull()
    expect(div.querySelectorAll('button.md-img-load')).toHaveLength(2)
    action.destroy()
    div.remove()
  })

  it('shows host + path and a Load button; the click signs exactly that one URL', async () => {
    const calls: unknown[] = []
    stubFetch((_url, init) => {
      const body = JSON.parse(String(init?.body)) as { urls: string[] }
      calls.push(body)
      return { success: true, signatures: { [body.urls[0]]: signed(body.urls[0]) } }
    })
    const div = mount('![one](https://img.example/one.png) ![two](https://img.example/two.png)')
    const action = camoClickToLoad(div, div.innerHTML)
    const ph = div.querySelector('.md-img-blocked')
    expect(ph?.textContent).toContain('img.example/one.png')
    expect(ph?.textContent).not.toContain('https://')
    const btn = ph?.querySelector<HTMLButtonElement>('button.md-img-load')
    expect(btn?.textContent).toBe('Load image')
    expect(calls).toHaveLength(0)

    btn?.click()
    await vi.waitFor(() => expect(div.querySelector('img.md-img')).not.toBeNull())
    expect(calls).toEqual([{ urls: ['https://img.example/one.png'], origin: 'assistant' }])
    expect(div.querySelector('img')?.getAttribute('src')).toBe(signed('https://img.example/one.png'))
    // The other image stays a placeholder until its own click.
    expect(div.querySelectorAll('img')).toHaveLength(1)
    expect(div.querySelectorAll('.md-img-blocked')).toHaveLength(1)
    action.destroy()
    div.remove()
  })

  it('keeps an approved image loaded across a re-render, without another sign', async () => {
    let calls = 0
    stubFetch((_url, init) => {
      calls++
      const body = JSON.parse(String(init?.body)) as { urls: string[] }
      return { success: true, signatures: { [body.urls[0]]: signed(body.urls[0]) } }
    })
    const md = '![k](https://img.example/keep.png)'
    const div = mount(md)
    const action = camoClickToLoad(div, div.innerHTML)
    div.querySelector<HTMLButtonElement>('button.md-img-load')?.click()
    await vi.waitFor(() => expect(div.querySelector('img.md-img')).not.toBeNull())

    div.innerHTML = renderMarkdown(`${md} and more text`) // streaming grows the message
    action.update()
    expect(div.querySelector('img.md-img')?.getAttribute('src')).toBe(signed('https://img.example/keep.png'))
    expect(div.querySelector('.md-img-blocked')).toBeNull()

    // A fresh mount of the same message (navigation back) also stays loaded.
    const again = mount(md)
    const a2 = camoClickToLoad(again, again.innerHTML)
    expect(again.querySelector('img.md-img')).not.toBeNull()
    await tick()
    expect(calls).toBe(1)
    action.destroy()
    a2.destroy()
    div.remove()
    again.remove()
  })

  it('offers no button and never shows or requests a query-string URL', async () => {
    const spy = vi.fn()
    vi.stubGlobal('fetch', spy)
    const div = mount('![x](https://evil.example/p.png?d=SECRET-TOKEN)')
    const action = camoClickToLoad(div, div.innerHTML)
    const ph = div.querySelector('.md-img-blocked')
    expect(ph).not.toBeNull()
    expect(ph?.querySelector('button')).toBeNull()
    expect(ph?.textContent).toContain('evil.example/p.png')
    expect(ph?.textContent).not.toContain('SECRET-TOKEN')
    div.querySelector<HTMLElement>('.md-img-blocked')?.click()
    await tick()
    expect(spy).not.toHaveBeenCalled()
    action.destroy()
    div.remove()
  })

  it('keeps the placeholder and offers a retry when the server declines', async () => {
    stubFetch(() => ({ success: true, signatures: {} }))
    const div = mount('![d](https://img.example/declined.png)')
    const action = camoClickToLoad(div, div.innerHTML)
    const btn = div.querySelector<HTMLButtonElement>('button.md-img-load')
    btn?.click()
    await vi.waitFor(() => expect(btn?.textContent).toBe('Not available — retry'))
    expect(btn?.disabled).toBe(false)
    expect(div.querySelector('img')).toBeNull()
    action.destroy()
    div.remove()
  })
})

describe('describeImageURL', () => {
  it('shows host + path, never the query string; mirrors the server mint rule', () => {
    expect(describeImageURL('https://e.com/a.png')).toEqual({ label: 'e.com/a.png', loadable: true })
    expect(describeImageURL('https://e.com/a.png?d=x')).toEqual({ label: 'e.com/a.png', loadable: false })
    expect(describeImageURL('https://e.com/a.png?')?.loadable).toBe(false)
    expect(describeImageURL('https://u:p@e.com/a.png')).toEqual({ label: 'e.com/a.png', loadable: false })
    expect(describeImageURL('https://e.com/a.png#frag?x')?.loadable).toBe(true)
    const long = describeImageURL(`https://e.com/${'a'.repeat(600)}`)
    expect(long?.loadable).toBe(false)
    expect(long?.label.length).toBeLessThan(80)
    expect(describeImageURL('mailto:x@y')).toBeNull()
  })
})
