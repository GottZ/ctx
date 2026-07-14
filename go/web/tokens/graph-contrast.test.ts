// Graph-Kontrast-Gate (design 02-graph-darkmode §4.6) — prüft die Graph-Farb-
// Tokens direkt aus tokens.json gegen den Canvas-Hintergrund (--graph-bg), für
// BEIDE Themes, mit der WCAG-2-Relative-Luminance-Formel (byte-identisch zu
// tokens/contrast-matrix.test.ts:55-65).
//
// Gates (U02-W1 + U02-W2):
//   G1b — Edge-Kontrast gegen den Canvas ≥ 3.0 in BEIDEN Themes. AM-1 (Board
//         U02-E2 = a) hebt die Politik von „nur Dark" auf symmetrisch: auch die
//         Light-Edge muss ≥ 3.0 tragen.
//   G1c — supersedes-Split-Guard: der edge-strong-Kontrast muss ≥ 1.4× den
//         edge-Kontrast betragen, sonst kollabiert der normal/supersedes-Split
//         unter einer späteren Token-Drift unsichtbar.
//   G1a — Node-Kontrast-Sweep (U02-W2): für BEIDE Themes und ALLE 360 Integer-
//         Hues den Kontrast der TATSÄCHLICH gebackenen Node-Farbe gegen
//         --graph-bg ≥ 3.0. VERBINDLICH: ruft exakt die Ship-Funktion hslToHex
//         (aus graph-client) auf und parst deren Ausgabe über colorToArray
//         (sigma/utils) zu RGB — KEINE test-lokale hsl→rgb-Mathematik, damit
//         Gate und Render bit-identisch sind (Fail-open-Schutz am 0.13-Grenz-
//         abstand des Light-Floors). Plus Floor-Regressions-Pins.
//   G2  — Sigma-Parse-Gate (U02-W2): colorToArray(categoryColor(...)) sowie
//         alle String-Farbfelder der Palette ≠ [0,0,0,·] (schwarz), plus ein
//         exakt gepinnter RGB-Fall. Parse-Ebene == Render-Ebene (WebGL-Node-
//         Fill nimmt denselben Parser). Rot gegen Ist: das ehemalige hsl()-
//         Format kollabierte hier auf [0,0,0,254].
//
//   G4d — Hover-Rahmen-Kontrast (U02-W3): contrast(graph-hover-stroke, graph-bg)
//         ≥ 3.0 in BEIDEN Themes (Non-Text, SC 1.4.11). Das ist der Gate, der die
//         Kasten-Abhebung gegen den Canvas prüft — die graph-hover-bg als
//         class:surface bewusst NICHT liefert (dark 1.19:1). Alias-Auflösung
//         ({graph.graph-edge-strong}) läuft über dieselbe Kette wie oben.
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { expect, test } from 'vitest'
import { colorToArray } from 'sigma/utils'
import { categoryColor, hslToHex } from '../src/lib/graph/graph-client'
import { hexToOklchHue } from '../src/lib/graph/color-space'
import type { GraphPalette } from '../src/lib/graph/graph-theme'

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

// Node-sat/lum sind $type:number (nicht in der color-Map oben) — direkt aus doc
// lesen: dark = $value, light = $extensions.ctx.light. Bare Zahlen (kein %).
function num(name: string, theme: 'dark' | 'light'): number {
  const t = doc.graph[name]
  if (!t) throw new Error(`graph-contrast: Token '${name}' fehlt in tokens.json`)
  const raw = theme === 'light' ? t.$extensions?.ctx?.light : t.$value
  return Number(raw)
}

// Die Laufzeit-Palette pro Theme, wie readGraphPalette sie nach einem Theme-
// Wechsel liefert — gespeist aus tokens.json (dieselbe Quelle wie graph-theme).
function palette(theme: 'dark' | 'light'): GraphPalette {
  return {
    labelColor: tok('graph-label', theme),
    edgeColor: tok('graph-edge', theme),
    edgeStrongColor: tok('graph-edge-strong', theme),
    edgeStructuralColor: tok('graph-edge-structural', theme),
    nodeSat: num('graph-node-sat', theme),
    nodeLum: num('graph-node-lum', theme),
    // Hover-Felder (U02-W3): alias-aufgelöst aus tokens.json über tok().
    hoverBg: tok('graph-hover-bg', theme),
    hoverStroke: tok('graph-hover-stroke', theme),
    hoverLabel: tok('graph-hover-label', theme),
  }
}

// WCAG-2-Luminanz direkt aus RGB-Kanälen (identische Kern-Formel wie luminance()
// oben, aber Eingabe ist das von colorToArray geparste [r,g,b] — der Kontrast
// wird also gegen die TATSÄCHLICH gerenderte Farbe gerechnet, nicht gegen ein
// paralleles Modell).
function luminanceRGB(r: number, g: number, b: number): number {
  const chan = (c: number) => {
    const s = c / 255
    return s <= 0.04045 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4
  }
  return 0.2126 * chan(r) + 0.7152 * chan(g) + 0.0722 * chan(b)
}
function contrastLum(a: number, b: number): number {
  const [hi, lo] = [a, b].sort((x, y) => y - x)
  return (hi + 0.05) / (lo + 0.05)
}

const EDGE_MIN = 3.0
const SPLIT_MIN = 1.4
const NODE_MIN = 3.0
// Floor-Pins: mit der Ship-hslToHex GEMESSEN (nicht analytisch) im W2-Build —
// dark 5.05 @ Hue 240, light 3.13 @ Hue 60. Absenkung = bewusste Diff-Entscheidung.
const NODE_FLOOR_PIN = { dark: 5.0, light: 3.1 } as const

// G1b — Edge-Kontrast gegen den Canvas ≥ 3.0 in BEIDEN Themes (AM-1 symmetrisch).
// GC1-Erweiterung (design graph-structural 03-§3.3): die structural-Kante läuft
// in derselben Schleife — dritte Kanten-Farbe, gleiche Politik.
const EDGE_TOKENS = ['graph-edge', 'graph-edge-structural'] as const
test('G1b: Edge-Tokens (edge, edge-structural) Kontrast gegen --graph-bg ≥ 3.0 in beiden Themes', () => {
  const failures: string[] = []
  for (const theme of THEMES) {
    const bg = tok('graph-bg', theme)
    for (const name of EDGE_TOKENS) {
      const edge = tok(name, theme)
      const ratio = contrast(edge, bg)
      if (ratio < EDGE_MIN) {
        failures.push(`${theme} --${name} (${edge}) auf --graph-bg (${bg}): ${ratio.toFixed(2)} < ${EDGE_MIN}`)
      }
    }
  }
  expect(failures, `Edge-Token unter ${EDGE_MIN}:\n${failures.join('\n')}`).toEqual([])
})

// G1f (GC1, design graph-structural 03-§3.3) — OKLCH-Hue-Distanz: der Farbkanal
// dream↔structural darf unter Token-Drift nicht unsichtbar kollabieren. ΔHue
// (zirkulär) zwischen structural und BEIDEN dream-Kanten-Tokens ≥ 60° je Theme.
// Mathematik = Ship-Funktion hexToOklchHue (color-space.ts, Ottosson-referenz-
// getestet in color-space.test.ts) — keine test-lokale Farbraum-Kopie.
const HUE_MIN = 60
function hueDist(a: string, b: string): number {
  const d = Math.abs(hexToOklchHue(a) - hexToOklchHue(b))
  return Math.min(d, 360 - d)
}
test('G1f: OKLCH-ΔHue structural↔edge und structural↔edge-strong ≥ 60° in beiden Themes', () => {
  const failures: string[] = []
  for (const theme of THEMES) {
    const structural = tok('graph-edge-structural', theme)
    for (const other of ['graph-edge', 'graph-edge-strong'] as const) {
      const dist = hueDist(structural, tok(other, theme))
      if (dist < HUE_MIN) {
        failures.push(`${theme} ΔHue(graph-edge-structural, ${other}) = ${dist.toFixed(1)}° < ${HUE_MIN}°`)
      }
    }
  }
  expect(failures, `structural-Hue-Abstand unter ${HUE_MIN}°:\n${failures.join('\n')}`).toEqual([])
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

const HOVER_STROKE_MIN = 3.0

// G4d — Hover-Rahmen-Kontrast: der 1px-Rahmen des Hover-Kastens (graph-hover-
// stroke, Alias auf graph-edge-strong) trägt die Abhebung gegen den Canvas.
// ≥ 3.0 in BEIDEN Themes (Non-Text). Negativ-Probe: Alias temporär auf
// {surface.surface-2} (~1.1:1 vs graph-bg) → rot; zurück → grün.
test('G4d: --graph-hover-stroke Kontrast gegen --graph-bg ≥ 3.0 in beiden Themes', () => {
  const failures: string[] = []
  for (const theme of THEMES) {
    const stroke = tok('graph-hover-stroke', theme)
    const bg = tok('graph-bg', theme)
    const ratio = contrast(stroke, bg)
    if (ratio < HOVER_STROKE_MIN) {
      failures.push(`${theme} --graph-hover-stroke (${stroke}) auf --graph-bg (${bg}): ${ratio.toFixed(2)} < ${HOVER_STROKE_MIN}`)
    }
  }
  expect(failures, `graph-hover-stroke unter ${HOVER_STROKE_MIN}:\n${failures.join('\n')}`).toEqual([])
})

// RGB der gebackenen Node-Farbe: hslToHex (Ship-Funktion) → colorToArray (sigma).
// Genau die Kette, die der WebGL-Node-Fill nimmt — keine parallele Mathematik.
function bakedNodeRGB(hue: number, sat: number, lum: number): [number, number, number] {
  const [r, g, b] = colorToArray(hslToHex(hue, sat, lum))
  return [r, g, b]
}

// G1a — Node-Kontrast-Sweep: alle 360 Integer-Hues, beide Themes, gebackene Farbe
// (Ship-hslToHex → colorToArray) gegen --graph-bg ≥ 3.0. Fail-closed über die
// offene Kategorie-Menge (jeder künftige Hash-Hue liegt im gegateten Bereich).
test('G1a: Node-Kontrast (gebackene Hex via hslToHex+colorToArray) gegen --graph-bg ≥ 3.0, alle 360 Hues, beide Themes', () => {
  const failures: string[] = []
  for (const theme of THEMES) {
    const p = palette(theme)
    const bg = luminance(tok('graph-bg', theme))
    for (let hue = 0; hue < 360; hue++) {
      const [r, g, b] = bakedNodeRGB(hue, p.nodeSat, p.nodeLum)
      const ratio = contrastLum(luminanceRGB(r, g, b), bg)
      if (ratio < NODE_MIN) {
        failures.push(`${theme} Hue ${hue} (${hslToHex(hue, p.nodeSat, p.nodeLum)}): ${ratio.toFixed(2)} < ${NODE_MIN}`)
      }
    }
  }
  // nur die ersten paar Verstöße zeigen, sonst wird die Message riesig
  expect(failures, `Node-Kontrast unter ${NODE_MIN}:\n${failures.slice(0, 8).join('\n')}${failures.length > 8 ? `\n… (+${failures.length - 8})` : ''}`).toEqual([])
})

// G1a Floor-Regression: der schwächste Hue je Theme bleibt über dem Pin.
// Absenkung ⇒ dieser Test bricht ⇒ bewusste Diff-Entscheidung nötig.
test('G1a Floor-Pin: dark ≥ 5.0, light ≥ 3.1 (mit der Ship-hslToHex gemessen)', () => {
  for (const theme of THEMES) {
    const p = palette(theme)
    const bg = luminance(tok('graph-bg', theme))
    let floor = Infinity
    let floorHue = -1
    for (let hue = 0; hue < 360; hue++) {
      const [r, g, b] = bakedNodeRGB(hue, p.nodeSat, p.nodeLum)
      const ratio = contrastLum(luminanceRGB(r, g, b), bg)
      if (ratio < floor) {
        floor = ratio
        floorHue = hue
      }
    }
    expect(
      floor,
      `${theme} Node-Floor ${floor.toFixed(3)} @ Hue ${floorHue} unter Pin ${NODE_FLOOR_PIN[theme]}`,
    ).toBeGreaterThanOrEqual(NODE_FLOOR_PIN[theme])
  }
})

// G2 — Sigma-Parse-Gate: jede je gebackene Farbe muss sigma-parsebar sein
// (≠ schwarz). categoryColor(...) über repräsentative Kategorien + alle String-
// Farbfelder der Palette, beide Themes. Rot gegen Ist: colorToArray des alten
// hsl()-Formats kollabierte auf [0,0,0,254].
const REPR_CATEGORIES = ['learnings', 'reference', 'projects', 'test', 'infrastructure', 'decisions']
test('G2: colorToArray(categoryColor(...)) ≠ schwarz für repräsentative Kategorien, beide Themes', () => {
  const failures: string[] = []
  for (const theme of THEMES) {
    const p = palette(theme)
    for (const cat of REPR_CATEGORIES) {
      const hex = categoryColor(cat, p)
      const [r, g, b] = colorToArray(hex)
      if (r === 0 && g === 0 && b === 0) {
        failures.push(`${theme} ${cat} (${hex}) → colorToArray schwarz`)
      }
    }
  }
  expect(failures, `Node-Farben parsen auf schwarz:\n${failures.join('\n')}`).toEqual([])
})

test('G2: colorToArray aller String-Farbfelder der Palette ≠ schwarz, beide Themes', () => {
  const failures: string[] = []
  for (const theme of THEMES) {
    const p = palette(theme)
    for (const field of ['labelColor', 'edgeColor', 'edgeStrongColor', 'edgeStructuralColor'] as const) {
      const hex = p[field]
      const [r, g, b] = colorToArray(hex)
      if (r === 0 && g === 0 && b === 0) {
        failures.push(`${theme} ${field} (${hex}) → colorToArray schwarz`)
      }
    }
  }
  expect(failures, `Palette-Farbfelder parsen auf schwarz:\n${failures.join('\n')}`).toEqual([])
})

// G2 Pin: exakte RGBA-Erwartung eines gebackenen Falls — der Alpha-Wert ist
// GEMESSEN (sigma packt 254, nicht 255), nicht geraten.
test('G2 Pin: categoryColor(learnings, dark) = #74e7ab → colorToArray [116,231,171,254]', () => {
  const dark = palette('dark')
  expect(categoryColor('learnings', dark)).toBe('#74e7ab')
  expect(colorToArray('#74e7ab')).toEqual([116, 231, 171, 254])
})
