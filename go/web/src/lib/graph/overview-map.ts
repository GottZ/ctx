// Overview "landkarte" graph builder (design 07-§3.5). Distinct from the ego
// graph-client: meta-nodes are Louvain clusters (UndirectedGraph — meta-edges
// are undirected), the map is small (~hundreds of nodes), so layout runs as a
// single SYNCHRONOUS FA2 pass — no worker, no eviction (design 07-§3.5/§4-W3).
// Pure (no Svelte, no DOM) → unit-testable like graph-client.

import { UndirectedGraph } from 'graphology'
import forceAtlas2 from 'graphology-layout-forceatlas2'
import type { OverviewResponse } from './api'
import { categoryColor } from './graph-client'
import type { GraphPalette } from './graph-theme'

export interface MetaNodeAttrs {
  label: string
  /** Representative block — a single click drills into its ego net. */
  reprId: string
  /** Raw visible cluster size (label/tooltip). */
  clusterSize: number
  scopeMix: string[]
  x: number
  y: number
  size: number
  color: string
}

export interface MetaEdgeAttrs {
  size: number
  color: string
  linkCount: number
}

/** Cluster meta-node radius: sublinear in member count (size 1..10^6). */
export function clusterNodeSize(size: number): number {
  return 6 + Math.min(26, Math.sqrt(size) * 1.8)
}

/** Meta-edge thickness: sublinear in aggregated link count. */
export function clusterEdgeSize(linkCount: number): number {
  return Math.min(8, 0.6 + Math.log2(1 + linkCount))
}

/**
 * Build the overview supergraph from the wire response. The cluster ordinal is
 * the node key (the wire already uses it; the internal cluster_id never reaches
 * the client). Edge ordinal tuples are resolved RIGHT HERE (response-local,
 * design 07-§3.5). Initial circular placement + one synchronous FA2 pass gives
 * a readable layout without a worker.
 */
export function buildOverviewGraph(
  resp: OverviewResponse,
  palette: GraphPalette,
  overrides?: Map<string, number>,
): UndirectedGraph<MetaNodeAttrs, MetaEdgeAttrs> {
  const g = new UndirectedGraph<MetaNodeAttrs, MetaEdgeAttrs>()
  const n = Math.max(1, resp.nodes.length)
  resp.nodes.forEach((node, i) => {
    const angle = (i / n) * 2 * Math.PI
    g.addNode(String(node.cluster), {
      // W7: the topic label when the server has one, the representative title
      // otherwise. Not a replacement — repr_title stays the caption of the
      // drill-down target, and a map without an identity layer renders exactly
      // as it did before.
      label: `${node.label ?? node.repr_title} · ${node.size}`,
      reprId: node.repr_id,
      clusterSize: node.size,
      scopeMix: node.scope_mix,
      x: Math.cos(angle) * 10,
      y: Math.sin(angle) * 10,
      size: clusterNodeSize(node.size),
      color: categoryColor(node.top_categories[0] ?? 'cluster', palette, overrides),
    })
  })
  for (const [src, dst, linkCount] of resp.edges) {
    const s = String(src)
    const d = String(dst)
    if (s === d || !g.hasNode(s) || !g.hasNode(d) || g.hasEdge(s, d)) continue
    g.addEdge(s, d, { size: clusterEdgeSize(linkCount), color: palette.edgeColor, linkCount })
  }
  if (g.order >= 2) {
    forceAtlas2.assign(g, { iterations: 300, settings: { ...forceAtlas2.inferSettings(g), slowDown: 8 } })
  }
  return g
}
