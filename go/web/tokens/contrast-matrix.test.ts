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

// Alle Farb-Tokens einsammeln: Gruppen sind die non-$-Keys der Wurzel,
// Tokens die non-$-Keys der Gruppe (Struktur wie tokens/generate.mjs).
const doc = read('tokens.json') as Record<string, Record<string, { $type?: string; $value?: string; $extensions?: { ctx?: { light?: string } } }>>
const colors = new Map<string, { dark: string; light: string }>()
for (const groupName of Object.keys(doc).filter((k) => !k.startsWith('$'))) {
  const group = doc[groupName]
  for (const name of Object.keys(group).filter((k) => !k.startsWith('$'))) {
    const t = group[name]
    if (t.$type !== 'color') continue
    colors.set(name, { dark: String(t.$value), light: String(t.$extensions?.ctx?.light ?? t.$value) })
  }
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
