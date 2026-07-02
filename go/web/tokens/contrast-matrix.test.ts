// Kontrast-Matrix (Q2, design 05-§4.2) — statisches G-B-Gate mit
// Vollständigkeits-Zwang: JEDER $type:color-Token aus tokens.json braucht
// GENAU EINE Klassifikation in pairings.json (Paarungs-Regel | surface |
// decorative | composition). Unklassifiziert ⇒ FEHLER, nicht Skip; verwaiste
// Manifest-Einträge und on-Ziele, die keine surface sind, ebenso — ein neuer
// Farb-Token ohne Manifest-Zeile macht die Suite rot (fail-closed).
// Paarungs-Regeln werden für BEIDE Themes (Dark-$value + $extensions.ctx.light)
// mit der WCAG-2-Relative-Luminance-Formel (SC 1.4.3) gerechnet, min durchgesetzt.
// Grenze (ehrlich, §4.2): Token-PAARE, keine Komposition — die fängt axe (06-PV5).
import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { expect, test } from 'vitest'

const tokensDir = dirname(fileURLToPath(import.meta.url))
const read = (f: string) => JSON.parse(readFileSync(join(tokensDir, f), 'utf8'))

type Rule = { on?: string[]; min?: number; class?: string; note?: string }
const CLASSES = ['surface', 'decorative', 'composition']

// Alle Farb- und Shadow-Tokens einsammeln: Gruppen sind die non-$-Keys der
// Wurzel, Tokens die non-$-Keys der Gruppe (Struktur wie tokens/generate.mjs).
// Q3: $type:shadow gehört zum Klassifikations-Universum (nur class-Form möglich
// — eine Paarungs-Regel auf einem Shadow scheitert an luminance(), fail-closed);
// DTCG-Aliasse ({group.token}, z. B. sync-* → semantic.*) werden auf die Werte
// des Ziel-Tokens aufgelöst, damit Paarungs-Regeln auf Aliassen rechenbar sind.
const doc = read('tokens.json') as Record<string, Record<string, { $type?: string; $value?: string; $extensions?: { ctx?: { light?: string } } }>>
const colors = new Map<string, { dark: string; light: string }>()
for (const groupName of Object.keys(doc).filter((k) => !k.startsWith('$'))) {
  const group = doc[groupName]
  for (const name of Object.keys(group).filter((k) => !k.startsWith('$'))) {
    const t = group[name]
    if (t.$type !== 'color' && t.$type !== 'shadow') continue
    colors.set(name, { dark: String(t.$value), light: String(t.$extensions?.ctx?.light ?? t.$value) })
  }
}
// Alias-Auflösung (eine Ebene reicht für sync-* → semantic.*; Tiefe begrenzt
// gegen Zyklen). Unauflösbare Aliasse bleiben stehen und scheitern — falls je
// mit Paarungs-Regel versehen — laut an luminance() statt still zu passen.
const ALIAS = /^\{[a-z0-9-]+\.([a-z0-9-]+)\}$/
for (let depth = 0; depth < 4; depth++) {
  let changed = false
  for (const v of colors.values()) {
    for (const theme of ['dark', 'light'] as const) {
      const m = ALIAS.exec(v[theme])
      if (m && colors.has(m[1])) { v[theme] = colors.get(m[1])![theme]; changed = true }
    }
  }
  if (!changed) break
}
const pairings = read('pairings.json') as Record<string, Rule>
const entries = Object.keys(pairings).filter((k) => !k.startsWith('$'))

// WCAG 2 relative Luminanz + Kontrast-Ratio (SC-1.4.3-Definition).
function luminance(hex: string, ctx: string): number {
  const m = /^#([0-9a-f]{6})$/i.exec(hex)
  if (!m) throw new Error(`${ctx}: Paarungs-Regeln brauchen 6-stellige Hex-Werte, bekam '${hex}' (rgba/alpha ⇒ composition klassifizieren)`)
  const n = parseInt(m[1], 16)
  const chan = (c: number) => { const s = c / 255; return s <= 0.04045 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4 }
  return 0.2126 * chan((n >> 16) & 255) + 0.7152 * chan((n >> 8) & 255) + 0.0722 * chan(n & 255)
}
function contrast(fg: string, bg: string, ctx: string): number {
  const [hi, lo] = [luminance(fg, ctx), luminance(bg, ctx)].sort((a, b) => b - a)
  return (hi + 0.05) / (lo + 0.05)
}

test('Vollständigkeits-Zwang: jeder Farb-Token hat genau eine gültige Klassifikation', () => {
  const problems: string[] = []
  for (const name of colors.keys()) {
    if (!entries.includes(name)) problems.push(`UNKLASSIFIZIERT: '${name}' fehlt in pairings.json (Regel {on,min} oder class surface|decorative|composition eintragen)`)
  }
  for (const name of entries) {
    const rule = pairings[name]
    if (!colors.has(name)) problems.push(`VERWAIST: pairings.json-Eintrag '${name}' ist kein $type:color-Token in tokens.json`)
    const isPairing = Array.isArray(rule.on) && typeof rule.min === 'number'
    const isClass = typeof rule.class === 'string' && CLASSES.includes(rule.class)
    if (isPairing === isClass) problems.push(`UNGÜLTIG: '${name}' braucht GENAU EINE Form — {on:[…],min:n} ODER {class:${CLASSES.join('|')}}`)
    for (const target of rule.on ?? []) {
      if (pairings[target]?.class !== 'surface') problems.push(`ON-ZIEL: '${name}' paart gegen '${target}', das nicht als surface klassifiziert ist`)
    }
  }
  const used = new Set(entries.flatMap((n) => pairings[n].on ?? []))
  for (const name of entries) {
    if (pairings[name].class === 'surface' && !used.has(name)) problems.push(`SURFACE UNBENUTZT: '${name}' ist on-Ziel keiner Paarungs-Regel`)
  }
  expect(problems, `pairings.json ⇄ tokens.json inkonsistent:\n${problems.join('\n')}`).toEqual([])
})

test('Paarungs-Regeln: WCAG-Ratio ≥ min in beiden Themes', () => {
  const failures: string[] = []
  for (const name of entries) {
    const rule = pairings[name]
    if (!Array.isArray(rule.on) || typeof rule.min !== 'number') continue
    for (const target of rule.on) {
      for (const theme of ['dark', 'light'] as const) {
        const fg = colors.get(name)?.[theme]
        const bg = colors.get(target)?.[theme]
        if (!fg || !bg) continue // fehlende Tokens meldet der Vollständigkeits-Test
        const ratio = contrast(fg, bg, `${theme} ${name}/${target}`)
        if (ratio < rule.min) failures.push(`${theme} --${name} (${fg}) auf --${target} (${bg}): ${ratio.toFixed(2)} < ${rule.min}`)
      }
    }
  }
  expect(failures, `Kontrast-Paarungen unter min:\n${failures.join('\n')}`).toEqual([])
})

// ── Palette-Validator für --type-slot-1..8 (Q3, design 05-§4.8.1) ────────────
// Übernommen aus dem dataviz-Skill (scripts/validate_palette.js), hier als
// Gate-Prüfschritt: (a) OKLCH-Lightness-Band 0.43–0.77 light / 0.48–0.67 dark,
// (b) Chroma ≥ 0.10 (darunter liest ein Hue als Grau), (c) CVD-Separation
// Machado-2009 (Severity 1.0) adjacenter Slots, CIE76 ΔE. Gate-Politik: das
// Skill-Gate hart-failt erst < 8 (Floor, legal NUR mit Zweit-Encoding — die
// Badge-Labels sind das Zweit-Encoding); wir gaten auf das 12er-ZIEL, weil die
// Ist-Palette es klar erfüllt (protan/deutan ≥ 25.9, tritan ≥ 24.5 über beide
// Themes) — eine Absenkung in den 8–12-Floor wäre eine bewusste, hier zu
// kommentierende Entscheidung. Kontrast ≥ 3:1 vs. surface-1 läuft bereits als
// Paarungs-Regel oben. Die Slot-ORDNUNG ist der CVD-Mechanismus (ordering-
// enumeriert über beide Themes) — Slots nie umsortieren, ohne neu zu rechnen.
const BAND = { light: [0.43, 0.77], dark: [0.48, 0.67] } as const
const CHROMA_FLOOR = 0.10
const CVD_MIN = 12.0
const MACHADO = {
  protan: [[0.152286, 1.052583, -0.204868], [0.114503, 0.786281, 0.099216], [-0.003882, -0.048116, 1.051998]],
  deutan: [[0.367322, 0.860646, -0.227968], [0.280085, 0.672501, 0.047413], [-0.011820, 0.042940, 0.968881]],
  tritan: [[1.255528, -0.076749, -0.178779], [-0.078411, 0.930809, 0.147602], [0.004733, 0.691367, 0.303900]],
} as const

function linRgb(hex: string): [number, number, number] {
  const m = /^#([0-9a-f]{6})$/i.exec(hex)
  if (!m) throw new Error(`Palette-Validator braucht 6-stellige Hex-Werte, bekam '${hex}'`)
  const n = parseInt(m[1], 16)
  const s2l = (c: number) => { const s = c / 255; return s <= 0.04045 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4 }
  return [s2l((n >> 16) & 255), s2l((n >> 8) & 255), s2l(n & 255)]
}
function oklch(hex: string): { L: number; C: number } {
  const [r, g, b] = linRgb(hex)
  const l = Math.cbrt(0.4122214708 * r + 0.5363325363 * g + 0.0514459929 * b)
  const m = Math.cbrt(0.2119034982 * r + 0.6806995451 * g + 0.1073969566 * b)
  const s = Math.cbrt(0.0883024619 * r + 0.2817188376 * g + 0.6299787005 * b)
  const L = 0.2104542553 * l + 0.7936177850 * m - 0.0040720468 * s
  const a = 1.9779984951 * l - 2.4285922050 * m + 0.4505937099 * s
  const bb = 0.0259040371 * l + 0.7827717662 * m - 0.8086757660 * s
  return { L, C: Math.hypot(a, bb) }
}
function lab(rgb: [number, number, number]): [number, number, number] {
  const [r, g, b] = rgb
  const X = 0.4124564 * r + 0.3575761 * g + 0.1804375 * b
  const Y = 0.2126729 * r + 0.7151522 * g + 0.0721750 * b
  const Z = 0.0193339 * r + 0.1191920 * g + 0.9503041 * b
  const f = (t: number) => (t > 0.008856 ? Math.cbrt(t) : 7.787 * t + 16 / 116)
  const [fx, fy, fz] = [f(X / 0.95047), f(Y), f(Z / 1.08883)]
  return [116 * fy - 16, 500 * (fx - fy), 200 * (fy - fz)]
}
function cvdDeltaE(h1: string, h2: string, kind: keyof typeof MACHADO): number {
  const M = MACHADO[kind]
  const clamp = (c: number) => Math.max(0, Math.min(1, c))
  const sim = (h: string): [number, number, number] => {
    const [r, g, b] = linRgb(h)
    return [
      clamp(M[0][0] * r + M[0][1] * g + M[0][2] * b),
      clamp(M[1][0] * r + M[1][1] * g + M[1][2] * b),
      clamp(M[2][0] * r + M[2][1] * g + M[2][2] * b),
    ]
  }
  const a = lab(sim(h1))
  const b = lab(sim(h2))
  return Math.hypot(a[0] - b[0], a[1] - b[1], a[2] - b[2])
}

const SLOTS = Array.from({ length: 8 }, (_, i) => `type-slot-${i + 1}`)

test('Palette-Validator: type-slot-1..8 (OKLCH-Band, Chroma, CVD ≥ 12)', () => {
  const problems: string[] = []
  for (const name of SLOTS) {
    if (!colors.has(name)) problems.push(`FEHLT: '${name}' ist kein Token in tokens.json`)
  }
  for (const theme of ['dark', 'light'] as const) {
    const [lo, hi] = BAND[theme === 'dark' ? 'dark' : 'light']
    const pal = SLOTS.map((n) => colors.get(n)?.[theme]).filter((v): v is string => !!v)
    for (const [i, hex] of pal.entries()) {
      const { L, C } = oklch(hex)
      if (L < lo || L > hi) problems.push(`${theme} ${SLOTS[i]} (${hex}): OKLCH L ${L.toFixed(3)} außerhalb [${lo}, ${hi}]`)
      if (C < CHROMA_FLOOR) problems.push(`${theme} ${SLOTS[i]} (${hex}): Chroma ${C.toFixed(3)} < ${CHROMA_FLOOR} (liest als Grau)`)
    }
    for (let i = 0; i < pal.length - 1; i++) {
      for (const kind of ['protan', 'deutan', 'tritan'] as const) {
        const d = cvdDeltaE(pal[i], pal[i + 1], kind)
        if (d < CVD_MIN) problems.push(`${theme} ${SLOTS[i]}↔${SLOTS[i + 1]} (${kind}): ΔE ${d.toFixed(1)} < ${CVD_MIN}`)
      }
    }
  }
  expect(problems, `type-slot-Palette fällt durch den Validator:\n${problems.join('\n')}`).toEqual([])
})
