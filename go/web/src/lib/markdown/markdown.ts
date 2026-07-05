// Shared markdown pipeline (design 04-workflow-ui §4.4 — lifted from
// lib/chat/markdown.ts so chat today and issues/comments later render through
// ONE path). Rendered content is foreign input (ingest, MCP, shared-scope
// tenants, forge sync) that may be quoted verbatim, so the rendered HTML is
// hostile until proven safe. Three lines of defense, two of them here (the
// third is the SPA CSP in web.go, img-src 'self' data:):
//
//   1. markdown-it with html:false — raw HTML in the markdown is ESCAPED, never
//      parsed. An <img onerror> or <script> in a quoted block becomes text.
//   2. DOMPurify over the rendered HTML — second line, catches any renderer bug.
//
// ctx: citation links: the model is told to cite blocks as [title](ctx:<id>).
// DOMPurify's default ALLOWED_URI_REGEXP knows no ctx: scheme, so a ctx: href
// would be silently stripped and every citation link would die. The renderer
// rule below rewrites ctx:<id> to the relative SPA route /graph?focus=<id>
// BEFORE sanitizing — the allowlist stays intact, the links survive.
//
// Additive hardening for the issue context (design 04 §4.4, §5.1):
//
//   3. Foreign-origin link hardening — external http(s) anchors get
//      target="_blank" rel="noopener noreferrer nofollow" (window.opener
//      hijack + SEO abuse). ONLY foreign origins: relative hrefs and
//      same-origin (including the ctx: → /graph?focus= citations) stay
//      target-less so SPA navigation keeps working in-tab.
//   4. Remote-image placeholder — the CSP would block a foreign <img> anyway
//      (silently broken img); the hook replaces it with a visible placeholder
//      carrying the URL as text (E04-9: deliberate UX instead of a silent
//      block; never load foreign images = no tracking-pixel deanonymization).
//   5. Render budget — sources over 256 KB (§4.4.2) are truncated with a
//      visible notice. Forge issues can be pathologically large; the 50-KB
//      store cap limits ctx-side, but this contract must not depend on a
//      backend invariant (fail-safe in the renderer).

import MarkdownIt from 'markdown-it'
import DOMPurify from 'dompurify'

const md = new MarkdownIt({
  html: false, // raw HTML is escaped, not parsed — the primary XSS defense
  linkify: true, // bare URLs become links (validated by markdown-it's bad-proto list)
  breaks: true, // a single newline becomes <br>, matching chat expectations
})

// Rewrite ctx:<id> hrefs to /graph?focus=<id> at render time (see header).
// link_open has no built-in renderer rule — its default is exactly
// self.renderToken(...), which we call after patching the href.
md.renderer.rules.link_open = (tokens, idx, options, _env, self) => {
  const token = tokens[idx]
  const i = token.attrIndex('href')
  if (i >= 0 && token.attrs) {
    const href = token.attrs[i][1]
    if (href.startsWith('ctx:')) {
      token.attrs[i][1] = `/graph?focus=${encodeURIComponent(href.slice(4))}`
    }
  }
  return self.renderToken(tokens, idx, options)
}

// Source cap in UTF-16 code units (== bytes for ASCII). Design 04 §4.4.2
// names 256 KB; string length is the unit the renderer actually sees.
export const MARKDOWN_SOURCE_CAP = 256 * 1024

// Appended AFTER sanitization — a compile-time constant, so this is safe by
// construction (no user input can reach it).
const TRUNCATION_NOTICE =
  '<p class="md-truncated" role="note">Content truncated — source exceeds 256 KB.</p>'

/**
 * True for absolute http(s) URLs pointing at a DIFFERENT origin than the SPA.
 * Relative hrefs (and same-origin absolute ones) resolve to location.origin
 * and return false — they must keep in-tab SPA navigation (design 04 §4.4.1).
 * Protocol-relative //host/path resolves to a foreign origin and counts.
 * Non-http(s) schemes (mailto:, data:image) are not "foreign links" here.
 */
function isForeignHttpOrigin(url: string): boolean {
  const base = typeof location !== 'undefined' ? location.origin : 'http://localhost'
  try {
    const u = new URL(url, base)
    return (u.protocol === 'http:' || u.protocol === 'https:') && u.origin !== base
  } catch {
    return false // unparseable — leave it to DOMPurify's URI allowlist
  }
}

// Foreign-origin hardening hook (design 04 §4.4.1/§4.4.3). Registered once at
// module scope on the shared DOMPurify default instance — this module is the
// only DOMPurify consumer in the app (grep gate: one {@html} path per surface,
// fed exclusively by renderMarkdown).
DOMPurify.addHook('afterSanitizeAttributes', (node) => {
  if (node.tagName === 'A') {
    if (isForeignHttpOrigin(node.getAttribute('href') ?? '')) {
      node.setAttribute('target', '_blank')
      node.setAttribute('rel', 'noopener noreferrer nofollow')
    }
    return
  }
  if (node.tagName === 'IMG') {
    const src = node.getAttribute('src') ?? ''
    if (isForeignHttpOrigin(src)) {
      // The CSP (img-src 'self' data:) would block the load anyway; replace
      // with a visible placeholder instead of a silent broken image (E04-9).
      // The replacement carries only TEXT content, so skipping re-sanitization
      // of the inserted node is safe by construction.
      //
      // Camo (design 07 §4.3): the placeholder ALSO carries the foreign URL in
      // data-camo-src, so a post-render pass (Markdown.svelte) can batch-sign
      // the URLs via POST /api/img/sign and swap the placeholder for a proxied
      // <img src="/api/img/…"> (same-origin ⇒ CSP-allowed). The placeholder is
      // the safe DEFAULT and the fallback: no sign step, sign failure, or a
      // disabled proxy all leave this visible text in place. The attribute value
      // is a data-* attribute (never executed) set on an element we construct,
      // so it is safe by construction like the text content.
      const doc = node.ownerDocument
      if (!doc) return
      const placeholder = doc.createElement('span')
      placeholder.className = 'md-img-blocked'
      placeholder.setAttribute('role', 'note')
      placeholder.setAttribute('data-camo-src', src)
      const alt = node.getAttribute('alt')
      if (alt) placeholder.setAttribute('data-camo-alt', alt)
      placeholder.textContent = `[external image blocked] ${src}`
      node.replaceWith(placeholder)
    }
  }
})

/**
 * Render untrusted markdown to sanitized HTML safe for {@html}. Escapes raw
 * HTML (html:false), rewrites ctx: citations to SPA graph routes, hardens
 * foreign-origin links, replaces remote images with placeholders, caps the
 * source at 256 KB, then runs DOMPurify. Empty / non-string input renders to
 * an empty string.
 */
export function renderMarkdown(src: string): string {
  if (typeof src !== 'string' || src === '') return ''
  const truncated = src.length > MARKDOWN_SOURCE_CAP
  const capped = truncated ? src.slice(0, MARKDOWN_SOURCE_CAP) : src
  const html = DOMPurify.sanitize(md.render(capped))
  return truncated ? html + TRUNCATION_NOTICE : html
}
