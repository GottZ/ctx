// Markdown rendering for chat assistant messages (design 06 §3.9). Block
// content is foreign input (ingest, MCP, shared-scope tenants) that the model
// may quote verbatim, so the rendered HTML is hostile until proven safe. Three
// lines of defense, two of them here (the third is the SPA CSP in web.go):
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

/**
 * Render assistant markdown to sanitized HTML safe for {@html}. Escapes raw
 * HTML (html:false), rewrites ctx: citations to SPA graph routes, then runs
 * DOMPurify. Empty / non-string input renders to an empty string.
 */
export function renderMarkdown(src: string): string {
  if (typeof src !== 'string' || src === '') return ''
  return DOMPurify.sanitize(md.render(src))
}
