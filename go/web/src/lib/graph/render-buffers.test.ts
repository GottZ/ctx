// Pins für die graphology→TypedArray-Extraktion (RV1): die Buffer-Renderer
// (cosmos/deck/three) hängen an exakt diesen Invarianten — Index-Mapping,
// Filter-Sichtbarkeit inkl. Endpunkt-Regel, Farb-Parsing, Positions-Nachzug.
// Node-tauglich ohne WebGL (bewusst kein Renderer-Import).

import { describe, expect, it } from 'vitest'
import { defaultFilters } from './filters'
import { createGraph, type EdgeAttrs, type NodeAttrs } from './graph-client'
import { buildEdgeBuffers, buildNodeBuffers, hexToRgba, refreshPositions } from './render-buffers'

function node(over: Partial<NodeAttrs> = {}): NodeAttrs {
  return {
    label: 'n',
    category: 'learnings',
    scope: 'private',
    createdAt: '2026-08-01T00:00:00Z',
    degree: 3,
    loadedDeg: 3,
    hopFromFocus: 1,
    lastTouched: 1,
    pinned: false,
    x: 10,
    y: -5,
    size: 6,
    color: '#ff8000',
    ...over,
  }
}

function edge(over: Partial<EdgeAttrs> = {}): EdgeAttrs {
  return { rel: 'topical', kind: 'dream', conf: 0.9, size: 1.9, color: '#336699', ...over }
}

describe('hexToRgba', () => {
  it('parst #rrggbb nach 0..1-rgba', () => {
    expect(hexToRgba('#ff8000')).toEqual([1, 128 / 255, 0, 1])
    expect(hexToRgba('#000000', 0.5)).toEqual([0, 0, 0, 0.5])
  })

  it('fällt bei unparsebaren Strings auf Grau — Renderer sterben nie an einer Farbe', () => {
    expect(hexToRgba('hsl(200 50% 50%)')).toEqual([0.5, 0.5, 0.5, 1])
    expect(hexToRgba('#fff')).toEqual([0.5, 0.5, 0.5, 1])
  })
})

describe('buildNodeBuffers', () => {
  it('extrahiert Positionen/Farben/Größen mit stabilem id↔Index-Mapping', () => {
    const g = createGraph()
    g.addNode('a', node({ x: 1, y: 2, size: 4 }))
    g.addNode('b', node({ x: -3, y: 7, size: 9, color: '#000000' }))
    const b = buildNodeBuffers(g, defaultFilters())
    expect(b.ids).toHaveLength(2)
    const ia = b.index.get('a')!
    const ib = b.index.get('b')!
    expect(b.ids[ia]).toBe('a')
    expect([b.positions[ia * 2], b.positions[ia * 2 + 1]]).toEqual([1, 2])
    expect([b.positions[ib * 2], b.positions[ib * 2 + 1]]).toEqual([-3, 7])
    expect(b.sizes[ib]).toBe(9)
    expect(b.colors[ib * 4 + 3]).toBe(1)
    expect(b.visible[ia]).toBe(1)
  })

  it('markiert Filter-versteckte Knoten als unsichtbar statt sie auszulassen', () => {
    const g = createGraph()
    g.addNode('a', node({ category: 'learnings' }))
    g.addNode('b', node({ category: 'projects' }))
    const f = { ...defaultFilters(), categories: ['projects'] }
    const b = buildNodeBuffers(g, f)
    expect(b.visible[b.index.get('a')!]).toBe(0)
    expect(b.visible[b.index.get('b')!]).toBe(1)
  })

  it('kodiert hopFromFocus=Infinity als -1 und unparsebares createdAt als NaN', () => {
    const g = createGraph()
    g.addNode('a', node({ hopFromFocus: Infinity, createdAt: 'nope' }))
    const b = buildNodeBuffers(g, defaultFilters())
    expect(b.hops[0]).toBe(-1)
    expect(Number.isNaN(b.createdAt[0])).toBe(true)
  })
})

describe('buildEdgeBuffers', () => {
  it('liefert Indexpaare in Node-Buffer-Reihenfolge samt Kantenfarbe', () => {
    const g = createGraph()
    g.addNode('a', node())
    g.addNode('b', node())
    g.addDirectedEdge('a', 'b', edge({ color: '#336699' }))
    const nodes = buildNodeBuffers(g, defaultFilters())
    const e = buildEdgeBuffers(g, defaultFilters(), nodes)
    expect([e.pairs[0], e.pairs[1]]).toEqual([nodes.index.get('a'), nodes.index.get('b')])
    expect(e.visible[0]).toBe(1)
    expect(e.colors[0]).toBeCloseTo(0x33 / 255)
  })

  it('versteckt eine Kante, wenn ein Endpunkt Filter-versteckt ist (Sigma-Endpunkt-Regel)', () => {
    const g = createGraph()
    g.addNode('a', node({ category: 'learnings' }))
    g.addNode('b', node({ category: 'projects' }))
    g.addDirectedEdge('a', 'b', edge())
    const f = { ...defaultFilters(), categories: ['projects'] }
    const e = buildEdgeBuffers(g, f, buildNodeBuffers(g, f))
    expect(e.visible[0]).toBe(0)
  })

  it('versteckt Kanten unter dem Confidence-Gate (dream), structural bleibt exempt', () => {
    const g = createGraph()
    g.addNode('a', node())
    g.addNode('b', node())
    g.addDirectedEdgeWithKey('d', 'a', 'b', edge({ conf: 0.2 }))
    g.addDirectedEdgeWithKey('s', 'b', 'a', edge({ kind: 'structural', rel: 'references', conf: 1 }))
    const f = { ...defaultFilters(), minConfidence: 0.5 }
    const nodes = buildNodeBuffers(g, f)
    const e = buildEdgeBuffers(g, f, nodes)
    // Walk-Reihenfolge folgt der Einfüge-Reihenfolge der Kanten.
    expect(e.visible[0]).toBe(0)
    expect(e.visible[1]).toBe(1)
  })
})

describe('refreshPositions', () => {
  it('zieht nur x/y nach — Farben/Sichtbarkeit bleiben stehen (Layout-Tick-Fastpath)', () => {
    const g = createGraph()
    g.addNode('a', node({ x: 0, y: 0 }))
    const b = buildNodeBuffers(g, defaultFilters())
    g.setNodeAttribute('a', 'x', 42)
    g.setNodeAttribute('a', 'y', -7)
    const colorBefore = b.colors[0]
    refreshPositions(g, b)
    expect([b.positions[0], b.positions[1]]).toEqual([42, -7])
    expect(b.colors[0]).toBe(colorBefore)
  })
})
