// Graph-Kontrast-Gate (design 02-graph-darkmode §4.6) — prüft die Graph-Farb-
// Tokens direkt aus tokens.json gegen den Canvas-Hintergrund (--graph-bg), für
// BEIDE Themes, mit der WCAG-2-Relative-Luminance-Formel (byte-identisch zu
// tokens/contrast-matrix.test.ts:55-65).
//
// Diese Welle (U02-W1) trägt zwei Gates:
//   G1b — Edge-Kontrast gegen den Canvas ≥ 3.0 in BEIDEN Themes. AM-1 (Board
//         U02-E2 = a) hebt die Politik von „nur Dark" auf symmetrisch: auch die
//         Light-Edge muss ≥ 3.0 tragen.
//   G1c — supersedes-Split-Guard: der edge-strong-Kontrast muss ≥ 1.4× den
//         edge-Kontrast betragen, sonst kollabiert der normal/supersedes-Split
//         unter einer späteren Token-Drift unsichtbar.
//
// NICHT hier (spätere Wellen, brauchen anderen Code): G1a (Node-Sweep über die
// gebackene hslToHex-Hex-Farbe, W2) und G4d (Hover-Rahmen-Kontrast, W3).
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { expect, test } from 'vitest'

type ThemePair = { dark: string; light: string }
type TokenNode = { $type?: string; $value?: string; $extensions?: { ctx?: { light?: string } } }

const tokensDir = dirname(fileURLToPath(import.meta.url))
const doc = JSON.parse(readFileSync(join(tokensDir, 'tokens.json'), 'utf8')) as Record<string, Record<string, TokenNode>>

// Farb-Tokens (Gruppe.token) mit Dark-/Light-Wert einsammeln + DTCG-Aliasse
// auflösen (Muster contrast-matrix.test.ts:27-50) — die Graph-Tokens sind heute
// bare Hex, aber die Alias-Auflösung hält das Gate robust gegen künftige {..}-Werte.
const colors = new Map<string, ThemePair>()
for (const groupName of Object.keys(doc).filter((k) => !k.startsWith('$'))) {
  const group = doc[groupName]
  for (const name of Object.keys(group).filter((k) => !k.startsWith('$'))) {
    const t = group[name]
    if (t.$type !== 'color') continue
    colors.set(name, { dark: String(t.$value), light: String(t.$extensions?.ctx?.light ?? t.$value) })
  }
}
const ALIAS = /^\{[a-z0-9-]+\.([a-z0-9-]+)\}$/
for (let depth = 0; depth < 4; depth++) {
  let changed = false
  for (const v of colors.values()) {
    for (const theme of ['dark', 'light'] as const) {
      const m = ALIAS.exec(v[theme])
      if (m && colors.has(m[1])) {
        v[theme] = colors.get(m[1])![theme]
        changed = true
      }
    }
  }
  if (!changed) break
}

// WCAG 2 relative Luminanz + Kontrast-Ratio (SC-1.4.3, identisch contrast-matrix).
function luminance(hex: string): number {
  const m = /^#([0-9a-f]{6})$/i.exec(hex)
  if (!m) throw new Error(`graph-contrast: 6-stellige Hex nötig, bekam '${hex}'`)
  const n = parseInt(m[1], 16)
  const chan = (c: number) => {
    const s = c / 255
    return s <= 0.04045 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4
  }
  return 0.2126 * chan((n >> 16) & 255) + 0.7152 * chan((n >> 8) & 255) + 0.0722 * chan(n & 255)
}
function contrast(fg: string, bg: string): number {
  const [hi, lo] = [luminance(fg), luminance(bg)].sort((a, b) => b - a)
  return (hi + 0.05) / (lo + 0.05)
}

const THEMES = ['dark', 'light'] as const
function tok(name: string, theme: 'dark' | 'light'): string {
  const v = colors.get(name)
  if (!v) throw new Error(`graph-contrast: Token '${name}' fehlt in tokens.json`)
  return v[theme]
}

const EDGE_MIN = 3.0
const SPLIT_MIN = 1.4

// G1b — Edge-Kontrast gegen den Canvas ≥ 3.0 in BEIDEN Themes (AM-1 symmetrisch).
test('G1b: --graph-edge Kontrast gegen --graph-bg ≥ 3.0 in beiden Themes', () => {
  const failures: string[] = []
  for (const theme of THEMES) {
    const edge = tok('graph-edge', theme)
    const bg = tok('graph-bg', theme)
    const ratio = contrast(edge, bg)
    if (ratio < EDGE_MIN) {
      failures.push(`${theme} --graph-edge (${edge}) auf --graph-bg (${bg}): ${ratio.toFixed(2)} < ${EDGE_MIN}`)
    }
  }
  expect(failures, `graph-edge unter ${EDGE_MIN}:\n${failures.join('\n')}`).toEqual([])
})

// G1c — supersedes-Split-Guard: edge-strong-Kontrast ≥ 1.4× edge-Kontrast, beide Themes.
test('G1c: --graph-edge-strong Split ≥ 1.4× --graph-edge in beiden Themes', () => {
  const failures: string[] = []
  for (const theme of THEMES) {
    const bg = tok('graph-bg', theme)
    const cEdge = contrast(tok('graph-edge', theme), bg)
    const cStrong = contrast(tok('graph-edge-strong', theme), bg)
    const factor = cStrong / cEdge
    if (factor < SPLIT_MIN) {
      failures.push(`${theme}: edge-strong ${cStrong.toFixed(2)} / edge ${cEdge.toFixed(2)} = ${factor.toFixed(3)}× < ${SPLIT_MIN}×`)
    }
  }
  expect(failures, `supersedes-Split unter ${SPLIT_MIN}×:\n${failures.join('\n')}`).toEqual([])
})
