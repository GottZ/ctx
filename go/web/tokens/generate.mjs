// tokens/generate.mjs — emits src/styles/tokens.css from tokens.json (DTCG).
// Single source: tokens.json. Run via `bun run tokens` (or `node tokens/generate.mjs`).
// Dependency-free (node:fs/path only). Deterministic: output order is the
// tokens.json insertion order (stable per ECMA-262 for non-numeric keys) —
// no re-sorting, because the Q1 gate demands byte-identity with the
// hand-curated pre-Q1 file layout (design 05-§3.2: pure carrier swap).
// The drift gate (tokens/tokens.drift.test.ts) keeps the committed CSS
// in lockstep with this generator's output.
import { readFileSync, writeFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const HEADER = [
  '/* GENERATED — edit tokens.json (tokens/tokens.json) and run `bun run tokens`.',
  ' * Do not edit by hand: tokens/tokens.drift.test.ts fails on any drift. */',
  '',
]

/** @typedef {{ $type?: string, $value?: string, $extensions?: { ctx?: Record<string, any> } }} Node */

/** @param {Node} node @returns {Record<string, string | string[]> } */
function ctx(node) {
  return (node.$extensions && node.$extensions.ctx) || {}
}

/** Group keys = all non-$ keys, in insertion order. @param {Record<string, any>} obj */
function children(obj) {
  return Object.keys(obj).filter((k) => !k.startsWith('$'))
}

/** Resolve a $value: DTCG alias "{group.token}" → var(--token), else verbatim. @param {string} value */
function cssValue(value) {
  const alias = /^\{[a-z0-9-]+\.([a-z0-9-]+)\}$/.exec(value)
  return alias ? `var(--${alias[1]})` : value
}

/**
 * Emit one theme block.
 * @param {Record<string, any>} doc parsed tokens.json
 * @param {'dark' | 'light'} theme
 * @returns {string[]}
 */
function emitBlock(doc, theme) {
  const light = theme === 'light'
  const out = []
  for (const groupName of children(doc)) {
    const group = doc[groupName]
    const lines = []
    const meta = ctx(group)
    const comment = light ? meta.lightComment : meta.comment
    for (const tokenName of children(group)) {
      const token = group[tokenName]
      const tokenCtx = ctx(token)
      if (light && tokenCtx.light === undefined) continue
      const value = light ? String(tokenCtx.light) : cssValue(String(token.$value))
      const trail = String((light ? tokenCtx.lightTrail : tokenCtx.trail) ?? '')
      lines.push(`  --${tokenName}: ${value};${trail}`)
    }
    if (lines.length === 0) continue
    if (out.length) out.push('')
    if (Array.isArray(comment)) out.push(...comment)
    out.push(...lines)
  }
  out.push('', `  color-scheme: ${theme};`)
  return out
}

/** Render the full tokens.css text from a parsed tokens.json document. @param {Record<string, any>} doc */
export function render(doc) {
  const meta = ctx(doc)
  const fileComment = Array.isArray(meta.fileComment) ? meta.fileComment : []
  const lightPreamble = Array.isArray(meta.lightPreamble) ? meta.lightPreamble : []
  return [
    ...HEADER,
    ...fileComment,
    ':root {',
    ...emitBlock(doc, 'dark'),
    '}',
    '',
    ...lightPreamble,
    ":root[data-theme='light'] {",
    ...emitBlock(doc, 'light'),
    '}',
    '',
  ].join('\n')
}

const tokensDir = dirname(fileURLToPath(import.meta.url))

/** Read tokens.json and render the CSS. */
export function generate() {
  const doc = JSON.parse(readFileSync(join(tokensDir, 'tokens.json'), 'utf8'))
  return render(doc)
}

// CLI: write the target file (optional --out <path> for tooling/tests).
if (process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href) {
  const outFlag = process.argv.indexOf('--out')
  const target = outFlag > -1 ? process.argv[outFlag + 1] : join(tokensDir, '..', 'src', 'styles', 'tokens.css')
  writeFileSync(target, generate())
  console.log(`tokens: wrote ${target}`)
}
