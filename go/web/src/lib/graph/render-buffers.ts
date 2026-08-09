// Gemeinsame graphology→TypedArray-Extraktion für die Buffer-Renderer
// (cosmos/deck/three, RV1). Sigma liest graphology direkt — die neuen Renderer
// arbeiten auf flachen Arrays (GPU-Upload-freundlich, ein Objekt-Walk pro
// graphRev statt pro Frame). Reine Node-taugliche Funktionen ohne WebGL —
// vitest-abgedeckt (render-buffers.test.ts).

import type { MultiDirectedGraph } from 'graphology'
import { edgeVisible, nodeVisible, type GraphFilters } from './filters'
import type { EdgeAttrs, NodeAttrs } from './graph-client'

export interface NodeBuffers {
  /** Index → Node-ID (stabile Reihenfolge des Extraktions-Walks). */
  ids: string[]
  /** Node-ID → Index (für Kanten-Paare und Picking-Rückweg). */
  index: Map<string, number>
  /** xy-Paare (Layout-Positionen; z entsteht erst im Renderer semantisch). */
  positions: Float32Array
  /** rgba-Quadrupel 0..1, aus dem gebackenen attrs.color (Hex). */
  colors: Float32Array
  sizes: Float32Array
  /** 1 = Filter-sichtbar. Buffer-Renderer blenden über Alpha/Skip aus. */
  visible: Uint8Array
  labels: string[]
  /** hopFromFocus (Infinity → -1) — semantische Z-Achse „hop". */
  hops: Float32Array
  /** createdAt als Epoch-ms (NaN bei unparsebarem Datum) — Z-Achse „time". */
  createdAt: Float64Array
}

export interface EdgeBuffers {
  /** Quell-/Ziel-Indexpaare in NodeBuffers-Reihenfolge. */
  pairs: Uint32Array
  /** rgba pro Kante (aus attrs.color). */
  colors: Float32Array
  /** 1 = Kante UND beide Endpunkte Filter-sichtbar. */
  visible: Uint8Array
}

/** '#rrggbb' → [r,g,b,a] in 0..1. Tolerant: unparsebare Strings → Grau
 *  (Renderer dürfen nie an einer Farbe sterben). */
export function hexToRgba(hex: string, alpha = 1): [number, number, number, number] {
  const m = /^#([0-9a-f]{6})$/i.exec(hex)
  if (m === null) return [0.5, 0.5, 0.5, alpha]
  const v = parseInt(m[1], 16)
  return [((v >> 16) & 0xff) / 255, ((v >> 8) & 0xff) / 255, (v & 0xff) / 255, alpha]
}

export function buildNodeBuffers(
  graph: MultiDirectedGraph<NodeAttrs, EdgeAttrs>,
  filters: GraphFilters,
): NodeBuffers {
  const n = graph.order
  const ids = new Array<string>(n)
  const index = new Map<string, number>()
  const positions = new Float32Array(n * 2)
  const colors = new Float32Array(n * 4)
  const sizes = new Float32Array(n)
  const visible = new Uint8Array(n)
  const labels = new Array<string>(n)
  const hops = new Float32Array(n)
  const createdAt = new Float64Array(n)
  let i = 0
  graph.forEachNode((id, attrs) => {
    ids[i] = id
    index.set(id, i)
    positions[i * 2] = attrs.x
    positions[i * 2 + 1] = attrs.y
    const [r, g, b, a] = hexToRgba(attrs.color)
    colors[i * 4] = r
    colors[i * 4 + 1] = g
    colors[i * 4 + 2] = b
    colors[i * 4 + 3] = a
    sizes[i] = attrs.size
    visible[i] = nodeVisible(attrs, filters) ? 1 : 0
    labels[i] = attrs.label
    hops[i] = Number.isFinite(attrs.hopFromFocus) ? attrs.hopFromFocus : -1
    createdAt[i] = Date.parse(attrs.createdAt)
    i++
  })
  return { ids, index, positions, colors, sizes, visible, labels, hops, createdAt }
}

export function buildEdgeBuffers(
  graph: MultiDirectedGraph<NodeAttrs, EdgeAttrs>,
  filters: GraphFilters,
  nodes: NodeBuffers,
): EdgeBuffers {
  const m = graph.size
  const pairs = new Uint32Array(m * 2)
  const colors = new Float32Array(m * 4)
  const visible = new Uint8Array(m)
  let i = 0
  graph.forEachEdge((_id, attrs, source, target) => {
    const s = nodes.index.get(source)
    const t = nodes.index.get(target)
    // Der Walk läuft auf derselben Instanz wie buildNodeBuffers — fehlende
    // Endpunkte sind strukturell unmöglich; der Guard hält TS strict + einen
    // etwaigen Race auf einer mutierten Instanz tolerant.
    if (s === undefined || t === undefined) return
    pairs[i * 2] = s
    pairs[i * 2 + 1] = t
    const [r, g, b, a] = hexToRgba(attrs.color)
    colors[i * 4] = r
    colors[i * 4 + 1] = g
    colors[i * 4 + 2] = b
    colors[i * 4 + 3] = a
    visible[i] = edgeVisible(attrs, filters) && nodes.visible[s] === 1 && nodes.visible[t] === 1 ? 1 : 0
    i++
  })
  return { pairs, colors, visible }
}

/** Schneller Positions-Nachzug (Layout-Tick): nur x/y in-place — Farben,
 *  Filter und Topologie bleiben stehen. Voraussetzung: keine Knoten-Mutation
 *  seit buildNodeBuffers (GraphPage bumpt bei Mutation graphRev → Neubau). */
export function refreshPositions(
  graph: MultiDirectedGraph<NodeAttrs, EdgeAttrs>,
  nodes: NodeBuffers,
): void {
  graph.forEachNode((id, attrs) => {
    const i = nodes.index.get(id)
    if (i === undefined) return
    nodes.positions[i * 2] = attrs.x
    nodes.positions[i * 2 + 1] = attrs.y
  })
}
