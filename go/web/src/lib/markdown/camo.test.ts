// @vitest-environment jsdom
//
// Camo client + post-render swap unit tests (design 07-camo §4.3, D2b).
//  1. signImages: batch shape, de-dup, fail-safe empty-map on any error.
//  2. camoProxy action: swaps signed placeholders for same-origin <img>, keeps
//     the placeholder for declined/failed URLs, and discards a stale response.

import { afterEach, describe, expect, it, vi } from 'vitest'
import { signImages } from '../api/images'
import { camoProxy } from './camo'
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
    const out = await signImages(['https://e.com/a.png', 'https://e.com/a.png', ''])
    expect(captured.url).toBe('/api/img/sign')
    expect((captured.body as { urls: string[] }).urls).toEqual(['https://e.com/a.png'])
    expect(out).toEqual({ 'https://e.com/a.png': '/api/img/sig1?url=x&exp=1' })
  })

  it('resolves to an empty map on an empty input (no request)', async () => {
    const spy = vi.fn()
    vi.stubGlobal('fetch', spy)
    expect(await signImages([])).toEqual({})
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
    expect(await signImages(['https://e.com/a.png'])).toEqual({})
  })

  it('degrades to an empty map on a network throw', async () => {
    vi.stubGlobal('fetch', () => Promise.reject(new Error('offline')))
    expect(await signImages(['https://e.com/a.png'])).toEqual({})
  })
})

describe('camoProxy action', () => {
  // Render foreign-image markdown → the pipeline leaves a placeholder carrying
  // data-camo-src; mount the action on a container holding that HTML.
  function mount(md: string): HTMLDivElement {
    const div = document.createElement('div')
    div.innerHTML = renderMarkdown(md)
    document.body.appendChild(div)
    return div
  }

  it('swaps a signed placeholder for a same-origin <img>, dropping the placeholder', async () => {
    stubFetch(() => ({
      success: true,
      signatures: { 'https://evil.example/pixel.png': '/api/img/abc123?url=https%3A%2F%2Fevil.example%2Fpixel.png&exp=9999999999' },
    }))
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
