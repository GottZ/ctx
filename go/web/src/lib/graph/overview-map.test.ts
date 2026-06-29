// Pins the overview-map builder (design 07-§3.5): cluster ordinals are node
// keys, response-local edge tuples resolve to those keys, self-loops/duplicates
// are dropped, and the internal cluster_id never appears (only repr_id, which
// is a visible block).

import { describe, expect, it } from 'vitest'
import type { OverviewNode, OverviewResponse } from './api'
import { buildOverviewGraph, clusterEdgeSize, clusterNodeSize } from './overview-map'
import { readGraphPalette } from './graph-theme'

// node test env: no document → dark fallback palette for the param-injected
// colorer / meta-edge color buildOverviewGraph now takes.
const palette = readGraphPalette()

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

describe('buildOverviewGraph', () => {
  it('keys nodes by cluster ordinal and exposes repr_id for drill-down', () => {
    const g = buildOverviewGraph(
      overview({ nodes: [onode(0, { repr_id: 'block-aaa' }), onode(1)] }),
      palette,
    )
    expect(g.order).toBe(2)
    expect(g.hasNode('0')).toBe(true)
    expect(g.getNodeAttribute('0', 'reprId')).toBe('block-aaa')
  })

  it('resolves response-local ordinal edge tuples to ordinal node keys', () => {
    const g = buildOverviewGraph(
      overview({ nodes: [onode(0), onode(1), onode(2)], edges: [[0, 2, 7, 1.5]] }),
      palette,
    )
    expect(g.hasEdge('0', '2')).toBe(true)
    expect(g.getEdgeAttribute('0', '2', 'linkCount')).toBe(7)
  })

  it('skips self-loops and duplicate (undirected) edges', () => {
    const g = buildOverviewGraph(
      overview({
        nodes: [onode(0), onode(1)],
        edges: [
          [0, 0, 1, 1], // self-loop
          [0, 1, 2, 1],
          [1, 0, 3, 1], // same undirected edge as above
        ],
      }),
      palette,
    )
    expect(g.size).toBe(1)
  })

  it('handles an empty map without crashing', () => {
    const g = buildOverviewGraph(overview({ nodes: [], edges: [] }), palette)
    expect(g.order).toBe(0)
    expect(g.size).toBe(0)
  })

  it('assigns finite positions (synchronous FA2 ran)', () => {
    const g = buildOverviewGraph(overview({ nodes: [onode(0), onode(1)], edges: [[0, 1, 5, 1]] }), palette)
    expect(Number.isFinite(g.getNodeAttribute('0', 'x'))).toBe(true)
    expect(Number.isFinite(g.getNodeAttribute('1', 'y'))).toBe(true)
  })
})

describe('size helpers', () => {
  it('clusterNodeSize grows sublinearly and is bounded', () => {
    expect(clusterNodeSize(1)).toBeLessThan(clusterNodeSize(100))
    expect(clusterNodeSize(1_000_000)).toBeLessThanOrEqual(6 + 26)
  })
  it('clusterEdgeSize grows with link count and is bounded', () => {
    expect(clusterEdgeSize(1)).toBeLessThan(clusterEdgeSize(1000))
    expect(clusterEdgeSize(10_000_000)).toBeLessThanOrEqual(8)
  })
})
