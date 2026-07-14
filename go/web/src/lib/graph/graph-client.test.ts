// Pins the graph-client invariants (design 05-§3.5/§6.7): edge index tuples
// resolve to UUIDs at merge time and never survive as indices, re-merges
// keep node positions stable, and hopFromFocus follows the client-side BFS.

import { describe, expect, it } from 'vitest'
import type { ApiNode, EgoResponse } from './api'
import {
  createGraph,
  dreamKey,
  evict,
  mergeEgo,
  recomputeHops,
  remainingDegree,
  structKey,
  touch,
} from './graph-client'
import { readGraphPalette } from './graph-theme'

// In the node test env there is no document → readGraphPalette() returns the
// dark fallback: a concrete palette for the now param-injected colorers.
const palette = readGraphPalette()

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
      palette,
    )
    expect(graph.order).toBe(2)
    expect(graph.size).toBe(1)
    expect(graph.hasDirectedEdge('a', 'b')).toBe(true)
    expect(graph.getEdgeAttribute(dreamKey('a', 'b'), 'rel')).toBe('supersedes')
    expect(graph.getEdgeAttribute(dreamKey('a', 'b'), 'conf')).toBe(0.83)
  })

  it('treats indices as response-local: a second response reuses index 0 for a different node', () => {
    const graph = createGraph()
    mergeEgo(graph, ego({ nodes: [node('a', 0), node('b', 1)], edges: [[0, 1, 0, 0.5]] }), palette)
    // Second response: index 0 is now node b — the edge must land on b→c.
    mergeEgo(
      graph,
      ego({ focus: 'b', nodes: [node('b', 0), node('c', 1)], edges: [[0, 1, 0, 0.9]] }),
      palette,
    )
    expect(graph.hasDirectedEdge('b', 'c')).toBe(true)
    expect(graph.hasDirectedEdge('a', 'c')).toBe(false)
  })

  it('keeps existing node positions on re-merge (layout never jumps)', () => {
    const graph = createGraph()
    mergeEgo(graph, ego({ nodes: [node('a', 0), node('b', 1)], edges: [] }), palette)
    graph.setNodeAttribute('b', 'x', 123)
    graph.setNodeAttribute('b', 'y', -7)
    mergeEgo(graph, ego({ focus: 'b', nodes: [node('b', 0), node('c', 1)], edges: [] }), palette)
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
      palette,
    )
    expect(graph.size).toBe(2)
    expect(graph.getEdgeAttribute(dreamKey('a', 'b'), 'rel')).toBe('topical')
    expect(graph.getEdgeAttribute(dreamKey('b', 'a'), 'rel')).toBe('factual')
  })

  it('re-merging the same response keeps the edge count stable (multigraph keyed merge, N4)', () => {
    const graph = createGraph()
    const resp = ego({ nodes: [node('a', 0), node('b', 1)], edges: [[0, 1, 0, 0.5]] })
    mergeEgo(graph, resp, palette)
    mergeEgo(graph, resp, palette)
    expect(graph.order).toBe(2)
    expect(graph.size).toBe(1)
  })

  it('holds a dream and a structural edge apart on the same directed pair (collision pair, U4)', () => {
    const graph = createGraph()
    mergeEgo(
      graph,
      ego({ nodes: [node('a', 0, { degree: 2 }), node('b', 1, { degree: 2 })], edges: [[0, 1, 0, 0.5]] }),
      palette,
    )
    // Same directed pair, own key — one row per (pair, class), PK of M076.
    graph.mergeDirectedEdgeWithKey(structKey('a', 'b', 'references'), 'a', 'b', {
      rel: 'references',
      kind: 'structural',
      conf: 1,
      size: 1.5,
      color: palette.edgeColor,
    })
    expect(graph.size).toBe(2)
    expect(graph.getEdgeAttribute(dreamKey('a', 'b'), 'rel')).toBe('topical')
    expect(graph.getEdgeAttribute(structKey('a', 'b', 'references'), 'rel')).toBe('references')
    // Badge math counts link ROWS of both data classes (K5/U4): degree - loadedDeg
    // must reach zero on the collision pair, not go negative or leave a ghost badge.
    expect(graph.degree('a')).toBe(2)
    expect(remainingDegree({ degree: 2, loadedDeg: graph.degree('a') })).toBeNull()
    expect(remainingDegree({ degree: 5, loadedDeg: graph.degree('a') })).toBe('3')
  })

  it('consumes structural_edges into kind-tagged keyed edges (GA2 wire contract)', () => {
    const graph = createGraph()
    mergeEgo(
      graph,
      ego({
        nodes: [node('a', 0), node('b', 1)],
        edges: [[0, 1, 0, 0.5]],
        struct_rels: ['references'],
        origins: ['system'],
        structural_edges: [[0, 1, 0, 0]],
      }),
      palette,
    )
    expect(graph.size).toBe(2)
    const k = structKey('a', 'b', 'references')
    expect(graph.getEdgeAttribute(k, 'kind')).toBe('structural')
    expect(graph.getEdgeAttribute(k, 'rel')).toBe('references')
    expect(graph.getEdgeAttribute(k, 'origin')).toBe('system')
    expect(graph.getEdgeAttribute(k, 'conf')).toBe(1)
    expect(graph.getEdgeAttribute(dreamKey('a', 'b'), 'kind')).toBe('dream')
  })

  it('updates loadedDeg including structural incidences (GA1 review carry)', () => {
    const graph = createGraph()
    mergeEgo(
      graph,
      ego({
        nodes: [node('a', 0), node('b', 1)],
        edges: [[0, 1, 0, 0.5]],
        struct_rels: ['references'],
        structural_edges: [[0, 1, 0, 0]],
      }),
      palette,
    )
    // Badge math (degree - loadedDeg) reads the STORED attribute — it must
    // count both data classes, or the badge under-reports on collision pairs.
    expect(graph.getNodeAttribute('a', 'loadedDeg')).toBe(2)
    expect(graph.getNodeAttribute('b', 'loadedDeg')).toBe(2)
  })

  it('tolerates an old server without structural fields (byte-identical behavior)', () => {
    const graph = createGraph()
    mergeEgo(graph, ego({ nodes: [node('a', 0), node('b', 1)], edges: [[0, 1, 0, 0.5]] }), palette)
    expect(graph.size).toBe(1)
    let structural = 0
    graph.forEachEdge((_id, attrs) => {
      if (attrs.kind === 'structural') structural++
    })
    expect(structural).toBe(0)
  })

  it('skips malformed structural tuples and omits unknown origins (tolerance)', () => {
    const graph = createGraph()
    mergeEgo(
      graph,
      ego({
        nodes: [node('a', 0), node('b', 1)],
        edges: [],
        struct_rels: ['references'],
        origins: [],
        structural_edges: [
          [0, 9, 0, 0], // unknown target index → skipped
          [0, 1, 7, 0], // unknown class index → skipped
          [0, 1, 0, 5], // unknown origin index → edge lands, origin omitted
        ],
      }),
      palette,
    )
    expect(graph.size).toBe(1)
    const k = structKey('a', 'b', 'references')
    expect(graph.getEdgeAttribute(k, 'kind')).toBe('structural')
    expect(graph.getEdgeAttribute(k, 'origin')).toBeUndefined()
  })

  it('re-merge with an unresolvable origin clears the stale value (shallow-merge trap)', () => {
    const graph = createGraph()
    const base = {
      nodes: [node('a', 0), node('b', 1)],
      edges: [] as EgoResponse['edges'],
      struct_rels: ['references'],
      structural_edges: [[0, 1, 0, 0]] as [number, number, number, number][],
    }
    mergeEgo(graph, ego({ ...base, origins: ['system'] }), palette)
    const k = structKey('a', 'b', 'references')
    expect(graph.getEdgeAttribute(k, 'origin')).toBe('system')
    // Second response: legend no longer resolves index 0 → origin must NOT
    // survive from the first merge (mergeDirectedEdgeWithKey shallow-merges;
    // an omitted key would keep the stale value).
    mergeEgo(graph, ego({ ...base, origins: [] }), palette)
    expect(graph.getEdgeAttribute(k, 'origin')).toBeUndefined()
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
      palette,
    )
    expect(graph.getNodeAttribute('a', 'loadedDeg')).toBe(2)
    expect(graph.getNodeAttribute('b', 'loadedDeg')).toBe(1)
  })
})

describe('evict (hard ceiling, design 05-§6.6)', () => {
  /** Star around the focus, hop-labelled in bands of `band` nodes. */
  function bigGraph(n: number, band = 10) {
    const graph = createGraph()
    const nodes = [node('focus', 0)]
    const edges: EgoResponse['edges'] = []
    for (let i = 1; i <= n; i++) {
      nodes.push(node(`n${i}`, Math.ceil(i / band)))
      edges.push([0, i, 0, 0.5])
    }
    mergeEgo(graph, ego({ focus: 'focus', nodes, edges }), palette)
    recomputeHops(graph, 'focus')
    return graph
  }

  it('does nothing under the caps', () => {
    const graph = bigGraph(20)
    expect(evict(graph, 'focus', { maxNodes: 50, maxEdges: 100, targetNodes: 10 })).toBe(0)
    expect(graph.order).toBe(21)
  })

  it('prunes to the hysteresis target once over the node cap', () => {
    const graph = bigGraph(30)
    const removed = evict(graph, 'focus', { maxNodes: 25, maxEdges: 1000, targetNodes: 12 })
    expect(removed).toBe(31 - 12)
    expect(graph.order).toBe(12)
    expect(graph.hasNode('focus')).toBe(true)
  })

  it('drops unreachable nodes first, then keeps the BFS-closest', () => {
    const graph = bigGraph(12, 4) // hops 1..3
    // An island with no edges — unreachable, must go first.
    mergeEgo(graph, ego({ focus: 'island', nodes: [node('island', 0)], edges: [] }), palette)
    recomputeHops(graph, 'focus')
    evict(graph, 'focus', { maxNodes: 5, maxEdges: 1000, targetNodes: 5 })
    expect(graph.hasNode('island')).toBe(false)
    // BFS here marks everything hop 1 (star) — survivors are focus + 4 nodes.
    expect(graph.order).toBe(5)
    expect(graph.hasNode('focus')).toBe(true)
  })

  it('never evicts pinned nodes and prefers stale ones (LRU tie-break)', () => {
    const graph = bigGraph(10, 10) // all hop 1
    graph.setNodeAttribute('n1', 'pinned', true)
    touch(graph, 'n2') // freshest of the band
    evict(graph, 'focus', { maxNodes: 4, maxEdges: 1000, targetNodes: 4 })
    expect(graph.hasNode('n1')).toBe(true)
    expect(graph.hasNode('n2')).toBe(true)
  })

  it('enforces the edge cap by dropping far nodes with their incidences', () => {
    const graph = bigGraph(30)
    evict(graph, 'focus', { maxNodes: 1000, maxEdges: 20, targetNodes: 1000 })
    expect(graph.size).toBeLessThanOrEqual(20)
  })
})

describe('remainingDegree', () => {
  it('reports the unloaded incidences and the 200+ cap', () => {
    expect(remainingDegree({ degree: 18, loadedDeg: 5 })).toBe('13')
    expect(remainingDegree({ degree: 5, loadedDeg: 5 })).toBeNull()
    expect(remainingDegree({ degree: 201, loadedDeg: 40 })).toBe('200+')
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
      palette,
    )
    recomputeHops(graph, 'c')
    expect(graph.getNodeAttribute('c', 'hopFromFocus')).toBe(0)
    expect(graph.getNodeAttribute('b', 'hopFromFocus')).toBe(1)
    expect(graph.getNodeAttribute('a', 'hopFromFocus')).toBe(2)
  })

  it('marks unreachable nodes Infinity', () => {
    const graph = createGraph()
    mergeEgo(graph, ego({ nodes: [node('a', 0), node('b', 1)], edges: [] }), palette)
    recomputeHops(graph, 'a')
    expect(graph.getNodeAttribute('b', 'hopFromFocus')).toBe(Infinity)
  })
})
