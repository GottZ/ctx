// Pins the graph-client invariants (design 05-§3.5/§6.7): edge index tuples
// resolve to UUIDs at merge time and never survive as indices, re-merges
// keep node positions stable, and hopFromFocus follows the client-side BFS.

import { describe, expect, it } from 'vitest'
import type { ApiNode, EgoResponse } from './api'
import { createGraph, mergeEgo, recomputeHops } from './graph-client'

function node(id: string, hop: number, partial: Partial<ApiNode> = {}): ApiNode {
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

describe('mergeEgo', () => {
  it('resolves index tuples to UUID edges with the rel legend', () => {
    const graph = createGraph()
    mergeEgo(
      graph,
      ego({
        nodes: [node('a', 0), node('b', 1)],
        edges: [[0, 1, 4, 0.83]],
      }),
    )
    expect(graph.order).toBe(2)
    expect(graph.size).toBe(1)
    expect(graph.hasDirectedEdge('a', 'b')).toBe(true)
    expect(graph.getEdgeAttribute('a', 'b', 'rel')).toBe('supersedes')
    expect(graph.getEdgeAttribute('a', 'b', 'conf')).toBe(0.83)
  })

  it('treats indices as response-local: a second response reuses index 0 for a different node', () => {
    const graph = createGraph()
    mergeEgo(graph, ego({ nodes: [node('a', 0), node('b', 1)], edges: [[0, 1, 0, 0.5]] }))
    // Second response: index 0 is now node b — the edge must land on b→c.
    mergeEgo(graph, ego({ focus: 'b', nodes: [node('b', 0), node('c', 1)], edges: [[0, 1, 0, 0.9]] }))
    expect(graph.hasDirectedEdge('b', 'c')).toBe(true)
    expect(graph.hasDirectedEdge('a', 'c')).toBe(false)
  })

  it('keeps existing node positions on re-merge (layout never jumps)', () => {
    const graph = createGraph()
    mergeEgo(graph, ego({ nodes: [node('a', 0), node('b', 1)], edges: [] }))
    graph.setNodeAttribute('b', 'x', 123)
    graph.setNodeAttribute('b', 'y', -7)
    mergeEgo(graph, ego({ focus: 'b', nodes: [node('b', 0), node('c', 1)], edges: [] }))
    expect(graph.getNodeAttribute('b', 'x')).toBe(123)
    expect(graph.getNodeAttribute('b', 'y')).toBe(-7)
  })

  it('keeps directed parallel pairs apart (a→b and b→a are two edges)', () => {
    const graph = createGraph()
    mergeEgo(
      graph,
      ego({
        nodes: [node('a', 0), node('b', 1)],
        edges: [
          [0, 1, 0, 0.5],
          [1, 0, 1, 0.7],
        ],
      }),
    )
    expect(graph.size).toBe(2)
    expect(graph.getEdgeAttribute('a', 'b', 'rel')).toBe('topical')
    expect(graph.getEdgeAttribute('b', 'a', 'rel')).toBe('factual')
  })

  it('tracks loadedDeg from the merged incidences', () => {
    const graph = createGraph()
    mergeEgo(
      graph,
      ego({
        nodes: [node('a', 0), node('b', 1), node('c', 1)],
        edges: [
          [0, 1, 0, 0.5],
          [0, 2, 0, 0.5],
        ],
      }),
    )
    expect(graph.getNodeAttribute('a', 'loadedDeg')).toBe(2)
    expect(graph.getNodeAttribute('b', 'loadedDeg')).toBe(1)
  })
})

describe('recomputeHops', () => {
  it('BFS-relabels distances after a focus change', () => {
    const graph = createGraph()
    mergeEgo(
      graph,
      ego({
        nodes: [node('a', 0), node('b', 1), node('c', 2)],
        edges: [
          [0, 1, 0, 0.5],
          [1, 2, 0, 0.5],
        ],
      }),
    )
    recomputeHops(graph, 'c')
    expect(graph.getNodeAttribute('c', 'hopFromFocus')).toBe(0)
    expect(graph.getNodeAttribute('b', 'hopFromFocus')).toBe(1)
    expect(graph.getNodeAttribute('a', 'hopFromFocus')).toBe(2)
  })

  it('marks unreachable nodes Infinity', () => {
    const graph = createGraph()
    mergeEgo(graph, ego({ nodes: [node('a', 0), node('b', 1)], edges: [] }))
    recomputeHops(graph, 'a')
    expect(graph.getNodeAttribute('b', 'hopFromFocus')).toBe(Infinity)
  })
})
