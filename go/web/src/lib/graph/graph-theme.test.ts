// Pins the graph-color bridge (design 03-§3/§5a/§6): categoryColor is hue-hash
// deterministic with palette-driven sat/lum, edgeColor maps supersedes→strong,
// and readGraphPalette never leaks NaN — empty OR `%`-suffixed tokens fall back
// to the dark default (the silent-Light-break guard, §6).

import { afterEach, describe, expect, it } from 'vitest'
import { categoryColor, edgeColor, readGraphPalette, type GraphPalette } from './graph-theme'

const dark: GraphPalette = {
  labelColor: '#9aa0bb',
  edgeColor: '#3a3a52',
  edgeStrongColor: '#5b5e74',
  nodeSat: 70,
  nodeLum: 68,
}

describe('categoryColor', () => {
  it('builds hsl(hue sat% lum%) with the hashed hue and palette sat/lum', () => {
    // hue('learnings') = 149 (hash*31 char fold, mod 360) — pins the formula.
    expect(categoryColor('learnings', dark)).toBe('hsl(149 70% 68%)')
    expect(categoryColor('cluster', dark)).toBe('hsl(74 70% 68%)')
  })

  it('is deterministic per category', () => {
    expect(categoryColor('infrastructure', dark)).toBe(categoryColor('infrastructure', dark))
    expect(categoryColor('decisions', dark)).toBe('hsl(175 70% 68%)')
  })

  it('takes saturation/lightness from the palette (theme-swappable), hue fixed', () => {
    const light: GraphPalette = { ...dark, nodeSat: 62, nodeLum: 45 }
    expect(categoryColor('learnings', light)).toBe('hsl(149 62% 45%)')
  })
})

describe('edgeColor', () => {
  it('maps supersedes to the strong color and everything else to the normal edge', () => {
    expect(edgeColor('supersedes', dark)).toBe('#5b5e74')
    expect(edgeColor('topical', dark)).toBe('#3a3a52')
    expect(edgeColor('factual', dark)).toBe('#3a3a52')
  })
})

describe('readGraphPalette', () => {
  const savedDoc = Object.getOwnPropertyDescriptor(globalThis, 'document')
  const savedGcs = Object.getOwnPropertyDescriptor(globalThis, 'getComputedStyle')

  afterEach(() => {
    if (savedDoc) Object.defineProperty(globalThis, 'document', savedDoc)
    else delete (globalThis as { document?: unknown }).document
    if (savedGcs) Object.defineProperty(globalThis, 'getComputedStyle', savedGcs)
    else delete (globalThis as { getComputedStyle?: unknown }).getComputedStyle
  })

  /** Stub document + getComputedStyle so a token name maps to a raw value. */
  function stubTokens(tokens: Record<string, string>): void {
    ;(globalThis as { document?: unknown }).document = { documentElement: {} }
    ;(globalThis as { getComputedStyle?: unknown }).getComputedStyle = () => ({
      getPropertyValue: (name: string) => tokens[name] ?? '',
    })
  }

  it('returns the dark fallback under SSR (no document)', () => {
    delete (globalThis as { document?: unknown }).document
    expect(readGraphPalette()).toEqual(dark)
  })

  it('reads present tokens and trims them', () => {
    stubTokens({
      '--graph-label': '  #54586a ',
      '--graph-edge': '#c4c8d4',
      '--graph-edge-strong': '#a6abbd',
      '--graph-node-sat': '62',
      '--graph-node-lum': '45',
    })
    expect(readGraphPalette()).toEqual({
      labelColor: '#54586a',
      edgeColor: '#c4c8d4',
      edgeStrongColor: '#a6abbd',
      nodeSat: 62,
      nodeLum: 45,
    })
  })

  it('falls back to the dark default on EMPTY tokens — no NaN sat/lum', () => {
    stubTokens({})
    const p = readGraphPalette()
    expect(p).toEqual(dark)
    expect(Number.isNaN(p.nodeSat)).toBe(false)
    expect(Number.isNaN(p.nodeLum)).toBe(false)
  })

  it('falls back when sat/lum carry a `%` (Number("62%") is NaN → dark default)', () => {
    stubTokens({ '--graph-node-sat': '62%', '--graph-node-lum': '45%' })
    const p = readGraphPalette()
    expect(p.nodeSat).toBe(70)
    expect(p.nodeLum).toBe(68)
    expect(Number.isNaN(p.nodeSat)).toBe(false)
    expect(Number.isNaN(p.nodeLum)).toBe(false)
    // and the resulting color is well-formed, never hsl(h NaN% NaN%)
    expect(categoryColor('learnings', p)).toBe('hsl(149 70% 68%)')
  })
})
