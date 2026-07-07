// Farbraum-Hilfen für das Hue-Wheel (design 02a §A2/§A4-W6/§A5). ZWEI getrennte
// Zwecke, beide reine Mathematik ohne DOM (unit-testbar wie graph-client):
//
//  1. OKLCH — NUR Anzeige-Raum. Das Wheel platziert Marker perceptuell (»so kann
//     man die abstände sehen«), rechnet also die aus dem HSL-Hue GEBACKENE Farbe
//     (hslToHex, graph-client) nach OKLCH und nutzt den OKLCH-Hue-Winkel als
//     Position. PIN gegen einen späteren OKLCH-Persist (§A5): persistiert wird
//     IMMER der HSL-Hue-Grad (0–359) — nur der deckt der G1a-Sweep (alle 360
//     Integer-Hues), ein OKLCH-Persist bräche die 1:1-Deckung »gesweepte ==
//     wählbare Hues«. Diese Datei konvertiert NUR für die Anzeige.
//
//  2. WCAG-2-Kontrast — reine Info-Anzeige des Ist-Kontrasts einer Node-Farbe
//     gegen --graph-bg. KEIN Warn-/Gate-Pfad: G1a deckt jeden wählbaren Hue
//     bereits ≥3:1, es kann keine unzulässige Wahl geben (§A4-W6). Die Kern-
//     Formel ist byte-gleich zur Test-Mathematik in tokens/graph-contrast.test.ts
//     (luminance :73) — bewusst hier gespiegelt statt aus dem Test importiert.

/** '#rrggbb' → [r,g,b] in 0..255. Wirft bei Nicht-Hex (kein NaN-Leck). */
export function hexToRgb(hex: string): [number, number, number] {
  const m = /^#([0-9a-f]{6})$/i.exec(hex)
  if (!m) throw new Error(`color-space: 6-stellige Hex nötig, bekam '${hex}'`)
  const n = parseInt(m[1], 16)
  return [(n >> 16) & 255, (n >> 8) & 255, n & 255]
}

/** sRGB-Kanal (0..255) → lineares Licht (WCAG-/OKLab-gemeinsame Transferkurve). */
function srgbToLinear(c: number): number {
  const s = c / 255
  return s <= 0.04045 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4
}

export interface Oklch {
  /** Perzeptuelle Helligkeit 0..~1. */
  L: number
  /** Chroma ≥ 0. */
  C: number
  /** Hue-Winkel in Grad 0..360 (0 = +a-Achse). */
  h: number
}

/**
 * sRGB (0..255) → OKLCH. Matrizen nach Björn Ottosson (OKLab-Referenz). NUR
 * Anzeige-Raum (§A2): das Ergebnis positioniert Wheel-Marker perceptuell, es
 * wird NIE persistiert.
 */
export function rgbToOklch(r: number, g: number, b: number): Oklch {
  const lr = srgbToLinear(r)
  const lg = srgbToLinear(g)
  const lb = srgbToLinear(b)

  const l = 0.4122214708 * lr + 0.5363325363 * lg + 0.0514459929 * lb
  const m = 0.2119034982 * lr + 0.6806995451 * lg + 0.1073969566 * lb
  const s = 0.0883024619 * lr + 0.2817188376 * lg + 0.6299787005 * lb

  const l_ = Math.cbrt(l)
  const m_ = Math.cbrt(m)
  const s_ = Math.cbrt(s)

  const L = 0.2104542553 * l_ + 0.793617785 * m_ - 0.0040720468 * s_
  const a = 1.9779984951 * l_ - 2.428592205 * m_ + 0.4505937099 * s_
  const bb = 0.0259040371 * l_ + 0.7827717662 * m_ - 0.808675766 * s_

  const C = Math.hypot(a, bb)
  let h = (Math.atan2(bb, a) * 180) / Math.PI
  if (h < 0) h += 360
  return { L, C, h }
}

/** '#rrggbb' → OKLCH-Hue-Winkel in Grad (Bequem-Wrapper für die Marker-Position). */
export function hexToOklchHue(hex: string): number {
  const [r, g, b] = hexToRgb(hex)
  return rgbToOklch(r, g, b).h
}

/**
 * WCAG-2 relative Luminanz einer '#rrggbb'-Farbe (SC 1.4.3). Kern-Formel
 * byte-gleich zu graph-contrast.test.ts:73 — hier als Produktions-Util für die
 * reine Ist-Kontrast-Anzeige des Wheels gespiegelt (nicht aus dem Test
 * importiert).
 */
export function relativeLuminance(hex: string): number {
  const [r, g, b] = hexToRgb(hex)
  return 0.2126 * srgbToLinear(r) + 0.7152 * srgbToLinear(g) + 0.0722 * srgbToLinear(b)
}

/** WCAG-2-Kontrastverhältnis zweier '#rrggbb'-Farben (≥ 1). Reine Info-Anzeige. */
export function contrastRatio(fg: string, bg: string): number {
  const [hi, lo] = [relativeLuminance(fg), relativeLuminance(bg)].sort((a, b) => b - a)
  return (hi + 0.05) / (lo + 0.05)
}
