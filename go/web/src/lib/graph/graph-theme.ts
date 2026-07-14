// The ONE graph color bridge (design 03-§3 / §5b Single-Ownership): the single
// place the graph reads its palette from the CSS theme tokens (tokens.css
// `--graph-*`). Sigma/graphology eat color STRINGS, not var() — so the bridge
// resolves the tokens via getComputedStyle once and hands a plain GraphPalette
// to the param-injected colorers. categoryColor/edgeColor keep their home in
// graph-client.ts (pure/testable); this module re-exports them so callers get
// palette + colorers from one import. G1 seeds new merges from the palette;
// recolorGraph is the O(order+size) re-paint pass G2 runs on a theme switch.

import type { MultiDirectedGraph, UndirectedGraph } from 'graphology'
import type { OverviewResponse } from './api'
import { categoryColor, edgeColor, type EdgeAttrs, type NodeAttrs } from './graph-client'
import type { MetaEdgeAttrs, MetaNodeAttrs } from './overview-map'

export { categoryColor, edgeColor }

export interface GraphPalette {
  labelColor: string
  edgeColor: string
  edgeStrongColor: string
  nodeSat: number
  nodeLum: number
  /** Hover-/Highlight-Kasten: opake Rückfläche (U02-W3, node-hover.ts). */
  hoverBg: string
  /** Hover-/Highlight-Kasten: 1px-Rahmen (Abhebung gegen den Canvas). */
  hoverStroke: string
  /** Hover-/Highlight-Kasten: Label-Farbe. */
  hoverLabel: string
}

// Dark-theme fallback — ships a readable graph BEFORE/WITHOUT the theme axis
// (and on SSR / token divergence). Values mirror tokens.css :root (design
// 03-§5a). Keep in sync with the dark column there.
const DARK_FALLBACK: GraphPalette = {
  labelColor: '#9aa0bb',
  edgeColor: '#5d5f80',
  edgeStrongColor: '#7d80a8',
  nodeSat: 70,
  nodeLum: 68,
  // Spiegel der aufgelösten Dark-Aliasse (graph-hover-*): surface-3 / edge-strong
  // / text.text. Keep in sync with the dark column there.
  hoverBg: '#1e1e28',
  hoverStroke: '#7d80a8',
  hoverLabel: '#d8dae5',
}

/**
 * Read the `--graph-*` tokens off the document root. Color tokens are strings;
 * sat/lum are BARE numbers (no `%`) — they feed hslToHex (U02-W2 parser fix),
 * which needs plain 0..100 values. `Number('')` and `Number('70%')` are both
 * NaN, so `Number(raw) || fallback` keeps an empty OR a `%`-suffixed token on
 * the dark default (no NaN reaches the hex baking).
 * SSR-safe: no document → full dark fallback.
 */
export function readGraphPalette(): GraphPalette {
  if (typeof document === 'undefined') return { ...DARK_FALLBACK }
  const cs = getComputedStyle(document.documentElement)
  const str = (name: string, fallback: string): string => {
    const raw = cs.getPropertyValue(name).trim()
    return raw || fallback
  }
  const num = (name: string, fallback: number): number => {
    const raw = cs.getPropertyValue(name).trim()
    return Number(raw) || fallback
  }
  return {
    labelColor: str('--graph-label', DARK_FALLBACK.labelColor),
    edgeColor: str('--graph-edge', DARK_FALLBACK.edgeColor),
    edgeStrongColor: str('--graph-edge-strong', DARK_FALLBACK.edgeStrongColor),
    nodeSat: num('--graph-node-sat', DARK_FALLBACK.nodeSat),
    nodeLum: num('--graph-node-lum', DARK_FALLBACK.nodeLum),
    // Alias-Tokens: getComputedStyle liefert den var()-substituierten Hex-Wert
    // pro Theme (Substitution ist Teil des computed value von Custom Properties).
    hoverBg: str('--graph-hover-bg', DARK_FALLBACK.hoverBg),
    hoverStroke: str('--graph-hover-stroke', DARK_FALLBACK.hoverStroke),
    hoverLabel: str('--graph-hover-label', DARK_FALLBACK.hoverLabel),
  }
}

/**
 * Re-paint every node/edge `color` attribute from the palette (O(order+size)).
 * Sigma bakes color into the attributes at merge time (graph-client mergeEgo),
 * so a theme switch needs an explicit pass — the reducers alone never touch a
 * baked attribute. G2 calls this on THEME_CHANGE; G1 only ships it.
 */
export function recolorGraph(
  graph: MultiDirectedGraph<NodeAttrs, EdgeAttrs>,
  palette: GraphPalette,
  overrides?: Map<string, number>,
): void {
  graph.forEachNode((id, attrs) => {
    graph.setNodeAttribute(id, 'color', categoryColor(attrs.category, palette, overrides))
  })
  graph.forEachEdge((id, attrs) => {
    graph.setEdgeAttribute(id, 'color', edgeColor(attrs.rel, palette))
  })
}

/**
 * Overview/cluster-map variant of recolorGraph (G2). The meta-graph is a
 * SEPARATE UndirectedGraph (overview-map.ts) whose nodes carry no `category`
 * and whose edges carry no `rel` attr — the meta-node color derives from the
 * cluster's top category in the wire response and meta-edges are always the
 * normal edge color (see buildOverviewGraph). So the re-paint re-derives from
 * the RETAINED response (keyed by cluster ordinal) rather than from node attrs.
 * Positions/FA2 are untouched — recolor only, no re-layout. O(order+size).
 */
export function recolorOverviewGraph(
  graph: UndirectedGraph<MetaNodeAttrs, MetaEdgeAttrs>,
  resp: OverviewResponse,
  palette: GraphPalette,
  overrides?: Map<string, number>,
): void {
  for (const node of resp.nodes) {
    const key = String(node.cluster)
    if (graph.hasNode(key)) {
      graph.setNodeAttribute(key, 'color', categoryColor(node.top_categories[0] ?? 'cluster', palette, overrides))
    }
  }
  graph.forEachEdge((id) => graph.setEdgeAttribute(id, 'color', palette.edgeColor))
}
