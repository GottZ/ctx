// Camo post-render pass (design 07-camo §4.3, D2b) as a Svelte action, shared by
// EVERY renderMarkdown sink (Markdown.svelte for issues/comments, MessageBubble
// for chat streaming) so the "one markdown path" invariant (design 04 §9.6)
// extends to image proxying: one place mints proxy <img>, not one per surface.
//
// The renderer leaves foreign remote images as span.md-img-blocked[data-camo-src]
// placeholders (markdown.ts). This action, mounted on the container that holds
// the {@html} output, collects those placeholders after each render, batch-signs
// their URLs via POST /api/img/sign, and swaps each for a proxied same-origin
// <img src="/api/img/…"> (allowed by img-src 'self' — the CSP never changes).
//
// Fail-safe by construction: the placeholder is the default and the fallback. A
// disabled proxy, a sign failure, or a URL the server declines all leave the
// visible placeholder untouched — the swap is purely additive. A per-run token
// discards a stale async response so a fast re-render (chat streaming grows the
// content and re-mints) never swaps against a superseded document.

import { signImages } from '../api/images'

export function camoProxy(node: HTMLElement, _content?: unknown) {
  let token = 0

  function run() {
    const placeholders = Array.from(
      node.querySelectorAll<HTMLElement>('span.md-img-blocked[data-camo-src]'),
    )
    if (placeholders.length === 0) return
    const current = ++token
    const urls = placeholders.map((p) => p.getAttribute('data-camo-src') ?? '')
    void signImages(urls).then((sigs) => {
      if (current !== token) return // superseded by a newer render
      if (!node.isConnected) return
      for (const ph of placeholders) {
        if (!ph.isConnected) continue
        const url = ph.getAttribute('data-camo-src') ?? ''
        const proxied = sigs[url]
        if (!proxied) continue // declined / proxy off → keep the placeholder
        const img = ph.ownerDocument.createElement('img')
        img.className = 'md-img'
        img.setAttribute('src', proxied) // server-minted, same-origin
        img.setAttribute('loading', 'lazy')
        img.setAttribute('decoding', 'async')
        const alt = ph.getAttribute('data-camo-alt')
        if (alt) img.setAttribute('alt', alt)
        ph.replaceWith(img)
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
