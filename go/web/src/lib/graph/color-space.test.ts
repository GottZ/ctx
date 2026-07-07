// Pins die Farbraum-Hilfen des Hue-Wheels (design 02a §A2/§A4-W6): die OKLCH-
// Konvertierung (NUR Anzeige-Raum) und die WCAG-2-Kontrast-Info. Bekannte Werte
// (Ottosson-Referenz + WCAG) als Roundtrip-/Sanity-Anker.

import { describe, expect, it } from 'vitest'
import { contrastRatio, hexToOklchHue, hexToRgb, relativeLuminance, rgbToOklch } from './color-space'

describe('hexToRgb', () => {
  it('parst 6-stellige Hex, wirft sonst', () => {
    expect(hexToRgb('#74e7ab')).toEqual([116, 231, 171])
    expect(() => hexToRgb('hsl(149,70%,68%)')).toThrow()
  })
})

describe('rgbToOklch (Anzeige-Raum, Ottosson-Referenz)', () => {
  it('weiß → L≈1, C≈0', () => {
    const { L, C } = rgbToOklch(255, 255, 255)
    expect(L).toBeCloseTo(1, 2)
    expect(C).toBeCloseTo(0, 3)
  })

  it('schwarz → L≈0', () => {
    expect(rgbToOklch(0, 0, 0).L).toBeCloseTo(0, 3)
  })

  it('reines sRGB-Rot → OKLCH (0.6279, 0.2577, 29.23°)', () => {
    const { L, C, h } = rgbToOklch(255, 0, 0)
    expect(L).toBeCloseTo(0.6279, 3)
    expect(C).toBeCloseTo(0.2577, 3)
    expect(h).toBeCloseTo(29.23, 1)
  })

  it('reines sRGB-Grün/Blau liefern getrennte Hue-Winkel (Monotonie-Sanity)', () => {
    const green = rgbToOklch(0, 255, 0).h
    const blue = rgbToOklch(0, 0, 255).h
    expect(green).toBeCloseTo(142.5, 0)
    expect(blue).toBeCloseTo(264.05, 0)
    expect(Math.abs(green - blue)).toBeGreaterThan(10)
  })

  it('hexToOklchHue wrappt hexToRgb→rgbToOklch', () => {
    expect(hexToOklchHue('#ff0000')).toBeCloseTo(29.23, 1)
  })
})

describe('WCAG-Kontrast (Info-Anzeige, byte-gleiche Kernformel wie das Gate)', () => {
  it('luminance: weiß=1, schwarz=0', () => {
    expect(relativeLuminance('#ffffff')).toBeCloseTo(1, 5)
    expect(relativeLuminance('#000000')).toBeCloseTo(0, 5)
  })

  it('contrastRatio ist symmetrisch und maximal 21 (weiß↔schwarz)', () => {
    expect(contrastRatio('#ffffff', '#000000')).toBeCloseTo(21, 1)
    expect(contrastRatio('#000000', '#ffffff')).toBeCloseTo(21, 1)
  })

  it('gleiche Farbe → Verhältnis 1', () => {
    expect(contrastRatio('#74e7ab', '#74e7ab')).toBeCloseTo(1, 5)
  })
})
