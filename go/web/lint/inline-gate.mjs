// lint/inline-gate.mjs — verschärftes Inline-Style-Gate (design 05-§4.4,
// Welle Q4). Stylelint sieht nur <style>-Blöcke; dieser Gate deckt den
// Attribut-Kanal, den die CSP bewusst offen lässt (style-src 'unsafe-inline'):
//
//   1. style="…"-Attribute und style:-Direktiven mit farb-/typo-wirksamer
//      Property sind VERBOTEN — Property-basiert, WERT-unabhängig (auch
//      var(--token) ist als Inline-Farbe verboten: Farbe/Typo trägt IMMER
//      eine Klasse). Geometrie (left/top/width/height/transform/z-index/
//      grid-*) bleibt in beiden Formen erlaubt.
//   2. style={…}-Expressions sind statisch nicht inspizierbar → jede Stelle
//      braucht einen Eintrag in lint/inline-style-allowlist.json mit
//      Begründung (nur Geometrie-Properties zulässig). Jede neue Stelle =
//      Allowlist-Diff = Review-sichtbar. Stale Einträge (Datei weg, Count
//      geschrumpft) sind ebenso rot — die Allowlist ist ein Ratchet, kein
//      Bodensatz.
//
// Absichtlich grob (Substring-Match, keine AST): fail-closed. Wächst die
// Allowlist oder werden die Muster zu grob, ersetzt eine AST-Regel via
// eslint-plugin-svelte dieses Skript (Eskalationspfad §4.4.3).
//
// Läuft dependency-frei (node:fs) via `bun run lint:css` / `node lint/inline-gate.mjs`.
// Stärker als das Q3-Zeilen-grep: Scan über den Datei-INHALT, damit auch
// mehrzeilige style="…"-Attribute (AppShell) nicht durchs Zeilenraster fallen.

import { readdirSync, readFileSync } from 'node:fs'
import { dirname, join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'

const webRoot = join(dirname(fileURLToPath(import.meta.url)), '..')
const srcRoot = join(webRoot, 'src')
const allowlistPath = join(webRoot, 'lint', 'inline-style-allowlist.json')

/** Farb-/typo-wirksame Property-Stämme (design 05-§4.4.1 — exakt das Q3-grep). */
const BANNED = ['background', 'color', 'fill', 'stroke', 'font']
const bannedGroup = BANNED.join('|')

// style="…"-Attribute: [^"]* deckt auch Zeilenumbrüche im Attribut ab.
const attrRe = new RegExp(`style\\s*=\\s*"[^"]*(?:${bannedGroup})`, 'g')
// style:-Direktiven: nur der Svelte-Direktiven-Prefix, kein CSS `list-style: none`
// (das Leerzeichen nach dem Doppelpunkt schützt CSS-Deklarationen nicht —
// der Match verlangt den Property-Stamm DIREKT nach `style:`).
const directiveRe = new RegExp(`style:(?:${bannedGroup})`, 'g')
// style={…}-Expressions (statisch nicht inspizierbar → Allowlist).
const exprRe = /style\s*=\s*\{/g

/** @returns {string[]} alle .svelte-Dateien unter src/, sortiert */
function svelteFiles(dir) {
  const out = []
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, entry.name)
    if (entry.isDirectory()) out.push(...svelteFiles(p))
    else if (entry.name.endsWith('.svelte')) out.push(p)
  }
  return out.sort()
}

/** Zeilennummer eines Match-Offsets (1-basiert). */
function lineOf(content, index) {
  return content.slice(0, index).split('\n').length
}

const errors = []
/** @type {Map<string, number>} rel. Pfad → style={}-Trefferzahl */
const exprHits = new Map()

for (const file of svelteFiles(srcRoot)) {
  const rel = relative(webRoot, file)
  const content = readFileSync(file, 'utf8')

  for (const m of content.matchAll(attrRe)) {
    errors.push(
      `${rel}:${lineOf(content, m.index)}: farb-/typo-wirksame Property im style="…"-Attribut ` +
        `(verboten, wert-unabhängig — Klasse verwenden): ${m[0].replaceAll('\n', ' ').slice(0, 80)}`,
    )
  }
  for (const m of content.matchAll(directiveRe)) {
    errors.push(
      `${rel}:${lineOf(content, m.index)}: farb-/typo-wirksame style:-Direktive ` +
        `(verboten, wert-unabhängig — Klasse verwenden): ${m[0]}`,
    )
  }
  for (const m of content.matchAll(exprRe)) {
    exprHits.set(rel, (exprHits.get(rel) ?? 0) + 1)
    void m
  }
}

// ── Rule 2: style={}-Expressions gegen die committete Allowlist ──────────────
/** @type {{ file: string, count: number, reason: string }[]} */
const allowlist = JSON.parse(readFileSync(allowlistPath, 'utf8'))

for (const entry of allowlist) {
  if (!entry.reason || !entry.reason.trim()) {
    errors.push(`${allowlistPath}: Eintrag ${entry.file} ohne Begründung (reason ist Pflicht)`)
  }
  const actual = exprHits.get(entry.file) ?? 0
  if (actual !== entry.count) {
    errors.push(
      `${entry.file}: style={}-Expressions: ${actual} gefunden, Allowlist erlaubt exakt ${entry.count} ` +
        `(Allowlist-Diff nötig — Review-sichtbar, nur Geometrie-Properties zulässig)`,
    )
  }
  exprHits.delete(entry.file)
}
for (const [file, count] of exprHits) {
  errors.push(
    `${file}: ${count}× style={}-Expression ohne Allowlist-Eintrag ` +
      `(lint/inline-style-allowlist.json, Begründung Pflicht, nur Geometrie)`,
  )
}

if (errors.length > 0) {
  console.error('inline-gate: FAIL (design 05-§4.4)')
  for (const e of errors) console.error('  ' + e)
  process.exit(1)
}
console.log('inline-gate: OK — kein farb-/typo-wirksamer Inline-Style, Expressions == Allowlist')
