// Graph state holder (design 05-§3.5): ONE graphology DirectedGraph instance
// is the single source of truth, sigma renders it directly. Plain TS, no
// Svelte reactivity on graph data — the runes proxy overhead on 5k node
// objects is the documented reason (web-graph.md (e)).

import { DirectedGraph } from 'graphology'
import type { EgoResponse } from './api'
// Type-only import — erased at build, so no runtime cycle with graph-theme
// (which re-exports the colorers below). graph-theme owns the palette reads;
// the colorer signatures live here (design 03-§5b Single-Ownership).
import type { GraphPalette } from './graph-theme'

export interface NodeAttrs {
  label: string
  category: string
  scope: string
  /** ISO created_at from the API node — client-side time-range filter input. */
  createdAt: string
  /** Visible server-side degree (201 = "200+"). */
  degree: number
  /** Incidences loaded in the client → badge = degree - loadedDeg (W3). */
  loadedDeg: number
  /** Client-side BFS distance, recomputed on every focus change. */
  hopFromFocus: number
  /** LRU sequence for eviction (W3). */
  lastTouched: number
  pinned: boolean
  x: number
  y: number
  size: number
  color: string
}

export interface EdgeAttrs {
  rel: string
  conf: number
  size: number
  color: string
}

/**
 * hsl (h 0..359, s/l 0..100) → '#rrggbb' (lowercase). Total: Hue mod 360,
 * s/l auf [0,100] geklemmt, Rundung — kein Throw-Pfad, keine NaN-Propagation
 * (design 02-graph-darkmode §4.1 M1). Sigmas WebGL-Farbparser (colorToArray)
 * kollabiert `hsl(...)`-Strings auf [0,0,0,·] (schwarz) — Hex ist parsebar.
 * EXPORTIERT, damit das Node-Kontrast-Gate G1a exakt diese Funktion aufruft
 * (eine Implementierung für Render UND Gate, keine Rundungsdivergenz).
 */
export function hslToHex(h: number, s: number, l: number): string {
  const hh = ((h % 360) + 360) % 360
  const ss = Math.min(100, Math.max(0, s)) / 100
  const ll = Math.min(100, Math.max(0, l)) / 100
  const a = ss * Math.min(ll, 1 - ll)
  const f = (n: number): string => {
    const k = (n + hh / 30) % 12
    const c = ll - a * Math.max(-1, Math.min(k - 3, 9 - k, 1))
    return Math.round(255 * c)
      .toString(16)
      .padStart(2, '0')
  }
  return `#${f(0)}${f(8)}${f(4)}`
}

/** Category → hue, deterministic without a category registry. Saturation and
 *  lightness come from the palette (theme-aware) — only the hue is hashed.
 *  Ausgabe ist Hex (nicht hsl), weil sigma nur Hex/rgb parst (§4.1 M1). */
export function categoryColor(category: string, palette: GraphPalette): string {
  let hash = 0
  for (let i = 0; i < category.length; i++) {
    hash = (hash * 31 + category.charCodeAt(i)) | 0
  }
  const hue = ((hash % 360) + 360) % 360
  return hslToHex(hue, palette.nodeSat, palette.nodeLum)
}

/** supersedes renders dim/strong — it is displayed, never traversed. */
export function edgeColor(rel: string, palette: GraphPalette): string {
  return rel === 'supersedes' ? palette.edgeStrongColor : palette.edgeColor
}

export function nodeSize(degree: number): number {
  return 4 + Math.min(8, Math.sqrt(degree))
}

let touchSeq = 0

export function createGraph(): DirectedGraph<NodeAttrs, EdgeAttrs> {
  return new DirectedGraph<NodeAttrs, EdgeAttrs>()
}

/**
 * Merge one ego response into the graph. Index tuples resolve to UUIDs
 * RIGHT HERE (indices are response-local, design 05-§6.7). New nodes seed
 * near their parent (the response focus if present, else origin) + jitter;
 * existing nodes keep their position so the layout never jumps.
 */
export function mergeEgo(
  graph: DirectedGraph<NodeAttrs, EdgeAttrs>,
  resp: EgoResponse,
  palette: GraphPalette,
): void {
  const seed = seedPosition(graph, resp.focus)
  for (const n of resp.nodes) {
    if (graph.hasNode(n.id)) {
      graph.mergeNodeAttributes(n.id, {
        label: n.title,
        degree: n.degree,
        lastTouched: ++touchSeq,
      })
      continue
    }
    const [x, y] = jitter(seed, n.hop)
    graph.addNode(n.id, {
      label: n.title,
      category: n.category,
      scope: n.scope,
      createdAt: n.created_at,
      degree: n.degree,
      loadedDeg: 0,
      hopFromFocus: n.hop,
      lastTouched: ++touchSeq,
      pinned: false,
      x,
      y,
      size: nodeSize(n.degree),
      color: categoryColor(n.category, palette),
    })
  }
  for (const [src, dst, rel, conf] of resp.edges) {
    const source = resp.nodes[src]?.id
    const target = resp.nodes[dst]?.id
    const relName = resp.rels[rel]
    if (source === undefined || target === undefined || relName === undefined) continue
    graph.mergeDirectedEdge(source, target, {
      rel: relName,
      conf,
      size: relName === 'supersedes' ? 1 : 1 + conf,
      color: edgeColor(relName, palette),
    })
  }
  for (const n of resp.nodes) {
    graph.setNodeAttribute(n.id, 'loadedDeg', graph.degree(n.id))
  }
}

/** Recompute hopFromFocus by client-side BFS (design 05-§3.5). */
export function recomputeHops(graph: DirectedGraph<NodeAttrs, EdgeAttrs>, focus: string): void {
  graph.forEachNode((node) => graph.setNodeAttribute(node, 'hopFromFocus', Infinity))
  if (!graph.hasNode(focus)) return
  graph.setNodeAttribute(focus, 'hopFromFocus', 0)
  let frontier = [focus]
  let hop = 0
  while (frontier.length > 0) {
    hop++
    const next: string[] = []
    for (const node of frontier) {
      graph.forEachNeighbor(node, (neighbor) => {
        if (graph.getNodeAttribute(neighbor, 'hopFromFocus') > hop) {
          graph.setNodeAttribute(neighbor, 'hopFromFocus', hop)
          next.push(neighbor)
        }
      })
    }
    frontier = next
  }
}

/** Mark a node as freshly used (LRU input for eviction). */
export function touch(graph: DirectedGraph<NodeAttrs, EdgeAttrs>, node: string): void {
  if (graph.hasNode(node)) graph.setNodeAttribute(node, 'lastTouched', ++touchSeq)
}

/** Degree-badge math: incidences not yet loaded; 201 means "200+". */
export function remainingDegree(attrs: Pick<NodeAttrs, 'degree' | 'loadedDeg'>): string | null {
  if (attrs.degree >= 201) return '200+'
  const remaining = attrs.degree - attrs.loadedDeg
  return remaining > 0 ? String(remaining) : null
}

export interface EvictOptions {
  maxNodes?: number
  maxEdges?: number
  /** Hysteresis target: once over a cap, prune nodes down to this count. */
  targetNodes?: number
}

/**
 * Hard client-memory ceiling (design 05-§4-W3, risk §6.6 — mandatory, not a
 * retrofit): over 5k nodes / 20k edges, drop the nodes farthest from the
 * focus first (BFS distance from recomputeHops), LRU as tie-break, down to
 * 4k (hysteresis so every expand doesn't re-trigger a prune). The focus and
 * pinned nodes are never evicted. Returns the number of nodes removed.
 */
export function evict(
  graph: DirectedGraph<NodeAttrs, EdgeAttrs>,
  focus: string,
  { maxNodes = 5000, maxEdges = 20000, targetNodes = 4000 }: EvictOptions = {},
): number {
  if (graph.order <= maxNodes && graph.size <= maxEdges) return 0

  const candidates: { id: string; hop: number; touched: number }[] = []
  graph.forEachNode((id, attrs) => {
    if (id === focus || attrs.pinned) return
    candidates.push({ id, hop: attrs.hopFromFocus, touched: attrs.lastTouched })
  })
  // Farthest first (Infinity = unreachable sorts first), oldest touch breaks ties.
  candidates.sort((a, b) => (b.hop - a.hop || a.touched - b.touched))

  let removed = 0
  for (const c of candidates) {
    if (graph.order <= targetNodes && graph.size <= maxEdges) break
    graph.dropNode(c.id) // drops incident edges with it
    removed++
  }
  return removed
}

/** Where new nodes spawn: the focus position if loaded, else the origin. */
function seedPosition(graph: DirectedGraph<NodeAttrs, EdgeAttrs>, focus: string): [number, number] {
  if (graph.hasNode(focus)) {
    return [graph.getNodeAttribute(focus, 'x'), graph.getNodeAttribute(focus, 'y')]
  }
  return [0, 0]
}

/**
 * Deterministic-ish radial placement: hop rings around the seed with
 * per-node angle spread. Good enough to read before the FA2 worker (W3)
 * takes over layout.
 */
function jitter([sx, sy]: [number, number], hop: number): [number, number] {
  const angle = (touchSeq * 2.399963) % (Math.PI * 2) // golden-angle spacing
  const radius = hop === 0 ? 0 : hop * 80 + (touchSeq % 7) * 6
  return [sx + Math.cos(angle) * radius, sy + Math.sin(angle) * radius]
}
