// Pins the graph-color bridge (design 03-§3/§5a/§6): categoryColor is hue-hash
// deterministic with palette-driven sat/lum, edgeColor maps supersedes→strong,
// and readGraphPalette never leaks NaN — empty OR `%`-suffixed tokens fall back
// to the dark default (the silent-Light-break guard, §6).

import { afterEach, describe, expect, it } from 'vitest'
import type { ApiNode, EgoResponse, OverviewNode, OverviewResponse } from './api'
import { createGraph, mergeEgo } from './graph-client'
import { buildOverviewGraph } from './overview-map'
import {
  categoryColor,
  edgeColor,
  readGraphPalette,
  recolorGraph,
  recolorOverviewGraph,
  type GraphPalette,
} from './graph-theme'
import tokensJson from '../../../tokens/tokens.json'

// G1d (design 02-graph-darkmode §4.6): die Palette-Fixtures werden AUS tokens.json
// gebunden, nicht als Literale gepinnt — damit ist die N1-Drift (ein Hand-Pin
// wie edgeStrongColor '#a6abbd', der vom Token abwich) strukturell tot: dark =
// $value, light = $extensions.ctx.light (dasselbe Bindungs-Muster wie
// tokens/contrast-matrix.test.ts:27-36). Ändert ein Token, wandern die Fixtures
// automatisch mit; Format bleibt Hex/Zahl wie in tokens.json.
const graphTokens = (tokensJson as unknown as {
  graph: Record<string, { $value: string; $extensions?: { ctx?: { light?: string } } }>
}).graph
const tok = (name: string, theme: 'dark' | 'light'): string =>
  theme === 'light' ? String(graphTokens[name].$extensions?.ctx?.light) : String(graphTokens[name].$value)

const dark: GraphPalette = {
  labelColor: tok('graph-label', 'dark'),
  edgeColor: tok('graph-edge', 'dark'),
  edgeStrongColor: tok('graph-edge-strong', 'dark'),
  nodeSat: Number(tok('graph-node-sat', 'dark')),
  nodeLum: Number(tok('graph-node-lum', 'dark')),
}

// The light column of the §5a token contract — the palette readGraphPalette()
// yields after a dark→light theme switch.
const light: GraphPalette = {
  labelColor: tok('graph-label', 'light'),
  edgeColor: tok('graph-edge', 'light'),
  edgeStrongColor: tok('graph-edge-strong', 'light'),
  nodeSat: Number(tok('graph-node-sat', 'light')),
  nodeLum: Number(tok('graph-node-lum', 'light')),
}

function apiNode(id: string, hop: number, partial: Partial<ApiNode> = {}): ApiNode {
  return {
    id,
    title: `block ${id}`,
    category: 'learnings',
    scope: 'private',
    degree: 3,
    hop,
    created_at: '2026-06-01T00:00:00Z',
    ...partial,
  }
}

function ego(partial: Partial<EgoResponse>): EgoResponse {
  return {
    success: true,
    focus: 'a',
    params: {},
    rels: ['topical', 'factual', 'causal', 'recurrent', 'supersedes'],
    nodes: [],
    edges: [],
    stats: { nodes: 0, edges: 0, truncated: false, elapsed_ms: 1 },
    ...partial,
  }
}

function onode(cluster: number, partial: Partial<OverviewNode> = {}): OverviewNode {
  return {
    cluster,
    size: 10,
    top_categories: ['learnings'],
    repr_id: `repr-${cluster}`,
    repr_title: `Cluster ${cluster}`,
    scope_mix: ['private'],
    ...partial,
  }
}

function overview(partial: Partial<OverviewResponse>): OverviewResponse {
  return {
    success: true,
    params: {},
    nodes: [],
    edges: [],
    stats: { nodes: 0, edges: 0, truncated: false, computed_at: null, elapsed_ms: 1 },
    ...partial,
  }
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
    const light: GraphPalette = { ...dark, nodeSat: 62, nodeLum: 33 }
    expect(categoryColor('learnings', light)).toBe('hsl(149 62% 33%)')
  })
})

describe('edgeColor', () => {
  it('maps supersedes to the strong color and everything else to the normal edge', () => {
    expect(edgeColor('supersedes', dark)).toBe(dark.edgeStrongColor)
    expect(edgeColor('topical', dark)).toBe(dark.edgeColor)
    expect(edgeColor('factual', dark)).toBe(dark.edgeColor)
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
      '--graph-edge': '#767b91',
      '--graph-edge-strong': '#54597a',
      '--graph-node-sat': '62',
      '--graph-node-lum': '33',
    })
    expect(readGraphPalette()).toEqual({
      labelColor: '#54586a',
      edgeColor: '#767b91',
      edgeStrongColor: '#54597a',
      nodeSat: 62,
      nodeLum: 33,
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

// G2 re-color pass (design 03-§4/§6 "Sigma bakes color into the attr"): a theme
// switch must rewrite EVERY baked node/edge color attr, not only the Sigma
// label/edge settings. The settings are a WebGL concern verified by the
// Playwright gate; here we pin the attribute re-paint that recolorGraph drives.
describe('recolorGraph', () => {
  /** Ego graph with two categories + a strong (supersedes) and a normal edge. */
  function builtGraph(palette: GraphPalette) {
    const graph = createGraph()
    mergeEgo(
      graph,
      ego({
        nodes: [apiNode('a', 0, { category: 'learnings' }), apiNode('b', 1, { category: 'decisions' })],
        edges: [
          [0, 1, 4, 0.8], // a→b supersedes → strong edge color
          [1, 0, 0, 0.5], // b→a topical → normal edge color
        ],
      }),
      palette,
    )
    return graph
  }

  it('re-paints ALL node color attrs from the new palette', () => {
    const graph = builtGraph(dark)
    // baked dark before the switch
    expect(graph.getNodeAttribute('a', 'color')).toBe(categoryColor('learnings', dark))

    recolorGraph(graph, light)

    // every node now carries the light color for its category — none left dark
    let allLight = true
    graph.forEachNode((_id, attrs) => {
      if (attrs.color !== categoryColor(attrs.category, light)) allLight = false
    })
    expect(allLight).toBe(true)
    expect(graph.getNodeAttribute('a', 'color')).toBe(categoryColor('learnings', light))
    expect(graph.getNodeAttribute('b', 'color')).toBe(categoryColor('decisions', light))
  })

  it('re-paints ALL edge color attrs, keeping the supersedes/normal split', () => {
    const graph = builtGraph(dark)
    expect(graph.getEdgeAttribute('a', 'b', 'color')).toBe(dark.edgeStrongColor)

    recolorGraph(graph, light)

    let allLight = true
    graph.forEachEdge((_id, attrs) => {
      if (attrs.color !== edgeColor(attrs.rel, light)) allLight = false
    })
    expect(allLight).toBe(true)
    expect(graph.getEdgeAttribute('a', 'b', 'color')).toBe(light.edgeStrongColor) // supersedes
    expect(graph.getEdgeAttribute('b', 'a', 'color')).toBe(light.edgeColor) // topical
  })

  // The GraphPage THEME_CHANGE_EVENT handler is exactly `readGraphPalette()`
  // followed by `recolorGraph(graph, palette)`. The window listener + Sigma
  // setSetting/refresh live in the WebGL canvas (Playwright gate, §6); here we
  // pin that the handler's KERNEL flips the baked attrs when the tokens change.
  it('simulates the theme-switch handler: readGraphPalette() → recolorGraph re-colors', () => {
    const savedDoc = Object.getOwnPropertyDescriptor(globalThis, 'document')
    const savedGcs = Object.getOwnPropertyDescriptor(globalThis, 'getComputedStyle')
    let tokens: Record<string, string> = {}
    ;(globalThis as { document?: unknown }).document = { documentElement: {} }
    ;(globalThis as { getComputedStyle?: unknown }).getComputedStyle = () => ({
      getPropertyValue: (name: string) => tokens[name] ?? '',
    })
    try {
      // boot under dark tokens (empty → dark fallback)
      const graph = builtGraph(readGraphPalette())
      expect(graph.getNodeAttribute('a', 'color')).toBe(categoryColor('learnings', dark))

      // user flips to light: tokens change, then the handler body runs
      tokens = {
        '--graph-label': '#54586a',
        '--graph-edge': '#767b91',
        '--graph-edge-strong': '#54597a',
        '--graph-node-sat': '62',
        '--graph-node-lum': '33',
      }
      const palette = readGraphPalette() // what GraphPage.onThemeChange reads
      recolorGraph(graph, palette)

      expect(graph.getNodeAttribute('a', 'color')).toBe(categoryColor('learnings', light))
      expect(graph.getEdgeAttribute('a', 'b', 'color')).toBe(light.edgeStrongColor)
    } finally {
      if (savedDoc) Object.defineProperty(globalThis, 'document', savedDoc)
      else delete (globalThis as { document?: unknown }).document
      if (savedGcs) Object.defineProperty(globalThis, 'getComputedStyle', savedGcs)
      else delete (globalThis as { getComputedStyle?: unknown }).getComputedStyle
    }
  })
})

// The overview meta-graph is a separate UndirectedGraph whose nodes carry no
// category — recolorOverviewGraph re-derives node colors from the RETAINED wire
// response (by cluster ordinal) and resets every meta-edge to the normal edge
// color (matching buildOverviewGraph). Positions are never touched.
describe('recolorOverviewGraph', () => {
  function builtOverview(palette: GraphPalette) {
    const resp = overview({
      nodes: [
        onode(0, { top_categories: ['learnings'] }),
        onode(1, { top_categories: ['decisions'] }),
        onode(2, { top_categories: [] }), // no top category → 'cluster' fallback
      ],
      edges: [[0, 1, 5, 1]],
    })
    return { graph: buildOverviewGraph(resp, palette), resp }
  }

  it('re-paints meta-node colors from the response (top category, cluster fallback)', () => {
    const { graph, resp } = builtOverview(dark)
    expect(graph.getNodeAttribute('0', 'color')).toBe(categoryColor('learnings', dark))

    recolorOverviewGraph(graph, resp, light)

    expect(graph.getNodeAttribute('0', 'color')).toBe(categoryColor('learnings', light))
    expect(graph.getNodeAttribute('1', 'color')).toBe(categoryColor('decisions', light))
    expect(graph.getNodeAttribute('2', 'color')).toBe(categoryColor('cluster', light))
  })

  it('resets ALL meta-edge colors to the normal edge color', () => {
    const { graph, resp } = builtOverview(dark)
    expect(graph.getEdgeAttribute('0', '1', 'color')).toBe(dark.edgeColor)

    recolorOverviewGraph(graph, resp, light)

    let allLight = true
    graph.forEachEdge((_id, attrs) => {
      if (attrs.color !== light.edgeColor) allLight = false
    })
    expect(allLight).toBe(true)
  })

  it('leaves node positions untouched (recolor only, no re-layout)', () => {
    const { graph, resp } = builtOverview(dark)
    const x0 = graph.getNodeAttribute('0', 'x')
    const y0 = graph.getNodeAttribute('0', 'y')

    recolorOverviewGraph(graph, resp, light)

    expect(graph.getNodeAttribute('0', 'x')).toBe(x0)
    expect(graph.getNodeAttribute('0', 'y')).toBe(y0)
  })
})
