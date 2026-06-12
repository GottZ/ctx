// Graph state holder (design 05-§3.5): ONE graphology DirectedGraph instance
// is the single source of truth, sigma renders it directly. Plain TS, no
// Svelte reactivity on graph data — the runes proxy overhead on 5k node
// objects is the documented reason (web-graph.md (e)).

import { DirectedGraph } from 'graphology'
import type { ApiNode, EgoResponse } from './api'

export interface NodeAttrs {
  label: string
  category: string
  scope: string
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

/** Category → hue, deterministic without a category registry. */
export function categoryColor(category: string): string {
  let hash = 0
  for (let i = 0; i < category.length; i++) {
    hash = (hash * 31 + category.charCodeAt(i)) | 0
  }
  const hue = ((hash % 360) + 360) % 360
  return `hsl(${hue} 55% 62%)`
}

/** supersedes renders dashed/dim — it is displayed, never traversed. */
export function edgeColor(rel: string): string {
  return rel === 'supersedes' ? '#595c6b' : '#34344a'
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
export function mergeEgo(graph: DirectedGraph<NodeAttrs, EdgeAttrs>, resp: EgoResponse): void {
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
      degree: n.degree,
      loadedDeg: 0,
      hopFromFocus: n.hop,
      lastTouched: ++touchSeq,
      pinned: false,
      x,
      y,
      size: nodeSize(n.degree),
      color: categoryColor(n.category),
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
      color: edgeColor(relName),
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
