<script lang="ts">
  import { onMount } from 'svelte'
  import { Deck, OrbitView, OrthographicView } from '@deck.gl/core'
  import type { Color, OrbitViewState, OrthographicViewState, PickingInfo } from '@deck.gl/core'
  import { LineLayer, ScatterplotLayer } from '@deck.gl/layers'
  import type { MultiDirectedGraph } from 'graphology'
  import type { GraphFilters } from '../../../lib/graph/filters'
  import type { EdgeAttrs, NodeAttrs } from '../../../lib/graph/graph-client'
  import type { GraphPalette } from '../../../lib/graph/graph-theme'
  import {
    buildEdgeBuffers,
    buildNodeBuffers,
    refreshPositions,
    type EdgeBuffers,
    type NodeBuffers,
  } from '../../../lib/graph/render-buffers'

  // deck.gl-Renderer (RV1): Buffer-basiert statt graphology-direkt — die
  // TypedArrays aus render-buffers sind die Datenquelle, graphology bleibt die
  // Wahrheit. 2D = OrthographicView, 3D = OrbitView (2.5D: die Z-Achse ist
  // SEMANTISCH — hopFromFocus-Treppen, kein 3D-Layout). Das Layout kommt
  // weiter vom FA2-Worker der Seite; wir ziehen Positionen über
  // graphology-Events nach (rAF-gedrosselt).
  let {
    graph,
    filters,
    palette,
    highlightedIds,
    topId = null,
    graphRev = 0,
    threeD = false,
    onnodeclick,
    onnodedoubleclick,
  }: {
    graph: MultiDirectedGraph<NodeAttrs, EdgeAttrs>
    /** Client-side filtering — Buffer-Neubau statt Sigma-Reducer. */
    filters: GraphFilters
    /** Nur Rebuild-Trigger: die Farben liegen gebacken in den graph-Attrs
     *  (GraphPage recolorGraph), der Buffer-Walk liest sie von dort. */
    palette: GraphPalette
    /** Nodes with an open floating window — radius bump ×1.3 (G5-Analogon). */
    highlightedIds?: Set<string>
    /** Top (focused) window's node — strongest bump ×1.5. */
    topId?: string | null
    /** Merge-Revision aus GraphPage — jeder Tick = kompletter Buffer-Neubau. */
    graphRev?: number
    /** 2D (Orthographic) ↔ 3D (Orbit) — der eine Schalter dieses Renderers. */
    threeD?: boolean
    onnodeclick?: (id: string) => void
    onnodedoubleclick?: (id: string) => void
  } = $props()
  // showLabels wird bewusst NICHT deklariert (caps.labels=false): überzählige
  // Props sind in Svelte 5 wirkungslos, der Toggle ist in der Meta-Row disabled.

  // Semantische Z-Treppe: Fokus (hop 0) auf z=0, jeder weitere Hop-Ring eine
  // Ebene tiefer. 90 Welt-Einheiten ≈ der jitter-Ring-Abstand in graph-client
  // (hop*80) — die Treppenhöhe liest sich damit wie ein Layout-Ring.
  const HOP_Z = 90
  /** Kanten-Alpha 0–255: Linien unter den Knoten leicht transparent, damit
   *  dichte Bündel nicht zur Fläche verklumpen (Sigma-Optik-Analogon). */
  const EDGE_ALPHA = 140

  let container: HTMLDivElement
  let canvasEl: HTMLCanvasElement

  // Deck-Instanz + Buffer leben AUSSERHALB der Svelte-Reaktivität (GraphView-
  // Muster): der runes-Proxy auf 5k-Knoten-Arrays wäre reiner Overhead, und
  // deck.gl diff't seine Props selbst.
  let deck: Deck<OrthographicView | OrbitView> | null = null
  let nodeBufs: NodeBuffers | null = null
  let edgeBufs: EdgeBuffers | null = null
  /** Buffer-Indizes der Filter-sichtbaren Knoten/Kanten — die data-Arrays der
   *  Layer (deck erkennt Rebuilds an der neuen Array-Referenz). */
  let nodeIdx: number[] = []
  let edgeIdx: number[] = []
  /** Tiefster ECHTER Hop im aktuellen Buffer — unerreichbare Knoten (-1)
   *  liegen eine Ebene DARUNTER (Auftrag: Fokus oben, Treppe nach unten). */
  let maxHop = 0
  /** Positions-Revision: refreshPositions mutiert die Float32Arrays in-place,
   *  deck sieht keine neue data-Referenz — der Tick in updateTriggers zwingt
   *  die Positions-Accessoren zur Neu-Auswertung. */
  let posRev = 0

  // Hover-Tooltip ist das EINZIGE reaktive Stück: ein kleines DOM-Overlay,
  // deck rendert selbst keine Labels (caps.labels=false).
  let tip = $state<{ x: number; y: number; label: string } | null>(null)

  /** OrthographicView (2D) bzw. OrbitView (3D) — beide unter DERSELBEN id,
   *  damit ein flacher initialViewState ohne per-View-Map greift. flipY:false
   *  = y wächst nach OBEN (Mathe-Konvention wie sigma) — die Renderer zeigen
   *  dieselbe Layout-Orientierung. orbitAxis 'Z': die Kamera kreist um die
   *  Hop-Achse, rotationX kippt auf die Treppe. */
  function makeView(is3D: boolean): OrthographicView | OrbitView {
    return is3D
      ? new OrbitView({ id: 'main', orbitAxis: 'Z', controller: true })
      : new OrthographicView({ id: 'main', flipY: false, controller: true })
  }

  /** ViewState auf die Daten-Bounds fitten: target = Mittelpunkt der
   *  sichtbaren Knoten, zoom = log2(Pixel je Welt-Einheit) der engeren Achse,
   *  minus Padding-Marge. deck resettet die Kamera auf JEDE (deep-ungleiche)
   *  neue initialViewState-Referenz (deck.js setProps, deepEqual-Guard) —
   *  genau der Hebel für resetCamera und den 2D↔3D-Wechsel. */
  function fitViewState(is3D: boolean): OrthographicViewState | OrbitViewState {
    let minX = Infinity
    let minY = Infinity
    let maxX = -Infinity
    let maxY = -Infinity
    const nb = nodeBufs
    if (nb !== null) {
      for (const i of nodeIdx) {
        const x = nb.positions[i * 2]
        const y = nb.positions[i * 2 + 1]
        if (x < minX) minX = x
        if (x > maxX) maxX = x
        if (y < minY) minY = y
        if (y > maxY) maxY = y
      }
    }
    if (minX > maxX) {
      // Leerer/ungebauter Graph → neutrale Kamera statt NaN-Propagation.
      minX = maxX = 0
      minY = maxY = 0
    }
    const w = container?.clientWidth || 800
    const h = container?.clientHeight || 600
    // Span-Floor 1 hält den Ein-Knoten-Fall endlich; -0.35 ≈ ×0.78 Padding.
    const zoom =
      Math.log2(Math.min(w / Math.max(maxX - minX, 1), h / Math.max(maxY - minY, 1))) - 0.35
    const target: [number, number, number] = [(minX + maxX) / 2, (minY + maxY) / 2, 0]
    if (is3D) {
      // rotationX ~30°: die Hop-Treppe liest sich als Terrassen-Perspektive.
      return { target, zoom, rotationX: 30, rotationOrbit: 0 } satisfies OrbitViewState
    }
    return { target, zoom } satisfies OrthographicViewState
  }

  /** Kompletter Buffer-Neubau (graphRev/Filter/Palette/Selektion) — die
   *  Helfer aus render-buffers machen den graphology-Walk, hier entstehen nur
   *  die Sichtbarkeits-Indexlisten + der Hop-Deckel für die Z-Treppe. */
  function rebuild(): void {
    const nb = buildNodeBuffers(graph, filters)
    const eb = buildEdgeBuffers(graph, filters, nb)
    nodeBufs = nb
    edgeBufs = eb
    nodeIdx = []
    maxHop = 0
    for (let i = 0; i < nb.ids.length; i++) {
      if (nb.visible[i] === 1) nodeIdx.push(i)
      if (nb.hops[i] > maxHop) maxHop = nb.hops[i]
    }
    edgeIdx = []
    for (let i = 0; i < eb.visible.length; i++) {
      if (eb.visible[i] === 1) edgeIdx.push(i)
    }
    pushLayers()
  }

  /** Layer neu erzeugen + an deck geben. deck matcht über die Layer-id und
   *  diff't: neue data-Referenz (rebuild) = Voll-Update aller Accessoren,
   *  gleiche Referenz + posRev-Tick (Layout-Nachzug) = nur Positionen. */
  function pushLayers(): void {
    const d = deck
    const nb = nodeBufs
    const eb = edgeBufs
    if (d === null || nb === null || eb === null) return
    const is3D = threeD
    const hi = highlightedIds
    const top = topId
    // z semantisch: 2D flach; 3D = Hop-Treppe nach unten, unerreichbare
    // Knoten (-1) eine Ebene unter dem tiefsten echten Hop.
    const unreachableZ = -(maxHop + 1) * HOP_Z
    const zOf = (i: number): number => {
      if (!is3D) return 0
      const hop = nb.hops[i]
      return hop >= 0 ? -hop * HOP_Z : unreachableZ
    }
    const pos = (i: number): [number, number, number] => [
      nb.positions[i * 2],
      nb.positions[i * 2 + 1],
      zOf(i),
    ]
    d.setProps({
      layers: [
        // Kanten ZUERST im Array → gezeichnet UNTER den Knoten.
        new LineLayer<number>({
          id: 'deck-edges',
          data: edgeIdx,
          widthUnits: 'pixels',
          getWidth: 1,
          getSourcePosition: (e) => pos(eb.pairs[e * 2]),
          getTargetPosition: (e) => pos(eb.pairs[e * 2 + 1]),
          getColor: (e): Color => [
            Math.round(eb.colors[e * 4] * 255),
            Math.round(eb.colors[e * 4 + 1] * 255),
            Math.round(eb.colors[e * 4 + 2] * 255),
            EDGE_ALPHA,
          ],
          updateTriggers: {
            getSourcePosition: [posRev, is3D],
            getTargetPosition: [posRev, is3D],
          },
        }),
        new ScatterplotLayer<number>({
          id: 'deck-nodes',
          data: nodeIdx,
          pickable: true,
          radiusUnits: 'pixels',
          getPosition: pos,
          // Radius-Staffel wie sigma (design 07-§E): top ×1.5 > Fenster ×1.3.
          getRadius: (i) => {
            const id = nb.ids[i]
            const base = nb.sizes[i]
            if (id === top) return base * 1.5
            return hi?.has(id) === true ? base * 1.3 : base
          },
          getFillColor: (i): Color => [
            Math.round(nb.colors[i * 4] * 255),
            Math.round(nb.colors[i * 4 + 1] * 255),
            Math.round(nb.colors[i * 4 + 2] * 255),
            Math.round(nb.colors[i * 4 + 3] * 255),
          ],
          updateTriggers: { getPosition: [posRev, is3D] },
        }),
      ],
    })
  }

  // Layout-Nachzug: der FA2-Worker mutiert graphology-x/y kontinuierlich.
  // Beide Attribut-Event-Formen abonnieren (Namen gegen graphology-types
  // index.d.ts:280/282 verifiziert): setNodeAttribute feuert
  // nodeAttributesUpdated, updateEachNodeAttributes (Worker-Sync) feuert
  // eachNodeAttributesUpdated. rAF-Drossel: viele Events je Tick → EIN
  // refreshPositions + Layer-Push pro Frame.
  let rafId = 0
  const onLayoutTick = (): void => {
    if (rafId !== 0) return
    rafId = requestAnimationFrame(() => {
      rafId = 0
      const nb = nodeBufs
      if (nb === null) return
      refreshPositions(graph, nb)
      posRev++
      pushLayers()
    })
  }

  /** Buffer-Index aus einem Pick: data-Items SIND Buffer-Indizes (number),
   *  info.object trägt sie direkt — kein Umweg über info.index nötig. */
  function pickedNodeId(info: PickingInfo | null): string | null {
    if (info === null || info.layer?.id !== 'deck-nodes') return null
    const nb = nodeBufs
    if (nb === null || typeof info.object !== 'number') return null
    return nb.ids[info.object] ?? null
  }

  // Doppelklick = Expand: deck hat kein onDblClick — DOM-Listener auf dem
  // Container + synchroner pickObject an der Event-Koordinate (Radius 4px
  // Toleranz, nur der Node-Layer). pickObject ist in 9.x als "WebGL only"
  // deprecated — wir laufen auf dem WebGL2-Default-Device, und der synchrone
  // Pfad passt zum synchronen DOM-Event.
  const onDblClick = (ev: MouseEvent): void => {
    const d = deck
    if (d === null) return
    const rect = container.getBoundingClientRect()
    const id = pickedNodeId(
      d.pickObject({
        x: ev.clientX - rect.left,
        y: ev.clientY - rect.top,
        radius: 4,
        layerIds: ['deck-nodes'],
      }),
    )
    if (id !== null) onnodedoubleclick?.(id)
  }

  onMount(() => {
    // Buffer VOR der Deck-Erzeugung bauen: so ist der initiale ViewState-Fit
    // schon auf echte Daten-Bounds gerechnet (kein Kamera-Sprung nach Frame 1).
    rebuild()
    const d = new Deck<OrthographicView | OrbitView>({
      // Eigener Canvas im Host-Div; Clear-Color bleibt deck-Default-transparent
      // ([0,0,0,0]) → var(--graph-bg) des Hosts scheint durch, Theme gratis.
      canvas: canvasEl,
      views: makeView(threeD),
      initialViewState: fitViewState(threeD),
      layers: [],
      onClick: (info) => {
        const id = pickedNodeId(info)
        if (id !== null) onnodeclick?.(id)
      },
      onHover: (info) => {
        const id = pickedNodeId(info)
        const nb = nodeBufs
        tip =
          id !== null && nb !== null && typeof info.object === 'number'
            ? { x: info.x, y: info.y, label: nb.labels[info.object] }
            : null
      },
    })
    deck = d
    pushLayers()
    // Kein eigener ResizeObserver: width/height-Default '100%' + lumas
    // CanvasContext (deck 9 / luma v9) beobachten den Canvas selbst
    // (deck.d.ts: "luma.gl owns resize observation … for Deck-created
    // canvases"; gilt ebenso für per props.canvas ATTACHTE Canvases —
    // nur rohe gl-Contexts brauchen Spiegelung).
    graph.on('nodeAttributesUpdated', onLayoutTick)
    graph.on('eachNodeAttributesUpdated', onLayoutTick)
    container.addEventListener('dblclick', onDblClick)
    return () => {
      container.removeEventListener('dblclick', onDblClick)
      graph.off('nodeAttributesUpdated', onLayoutTick)
      graph.off('eachNodeAttributesUpdated', onLayoutTick)
      if (rafId !== 0) cancelAnimationFrame(rafId)
      d.finalize()
      deck = null
    }
  })

  // Rebuild-Trigger: JEDER dieser Props-Wechsel ändert Buffer-Inhalt oder
  // Accessor-Ergebnis → kompletter Neubau (data-Referenz wechselt, deck
  // wertet alle Accessoren neu). Der erste Lauf direkt nach onMount wird
  // übersprungen — mount hat mit identischen Werten bereits gebaut.
  let firstRebuildRun = true
  $effect(() => {
    void graphRev
    void filters
    void palette
    void threeD
    void highlightedIds
    void topId
    if (firstRebuildRun) {
      firstRebuildRun = false
      return
    }
    rebuild()
  })

  // 2D↔3D-Wechsel: neue View-Klasse + frischer Bounds-Fit in EINEM setProps.
  // deck erkennt die deep-ungleiche initialViewState und resettet die intern
  // getrackte Kamera (deck.js:323ff) — Orbit-Rotationen lecken so nie in den
  // Orthographic-State und umgekehrt. Erster Lauf = mount-Zustand → skip.
  let firstViewRun = true
  $effect(() => {
    const is3D = threeD === true
    if (firstViewRun) {
      firstViewRun = false
      return
    }
    deck?.setProps({ views: makeView(is3D), initialViewState: fitViewState(is3D) })
  })

  /** Re-center + Re-Fit auf die aktuellen Daten-Bounds (Fokus-Sprung). */
  export function resetCamera(): void {
    deck?.setProps({ initialViewState: fitViewState(threeD === true) })
  }

  /** Voll-Neubau von außen (GraphPage recolor-on-arrival, AM-2). */
  export function refresh(): void {
    rebuild()
  }
</script>

<!-- data-e2e-mask: FA2-Positionen sind nicht seed-stabil — das Visual-Gate
     maskiert die Region (gleiches Muster wie GraphView/sigma). -->
<div class="canvas" data-e2e-mask bind:this={container}>
  <canvas bind:this={canvasEl}></canvas>
  {#if tip !== null}
    <!-- Inline-Styles NUR Geometrie (Lint-Gate) — Farben im style-Block. -->
    <div class="tip" style:left={tip.x + 10 + 'px'} style:top={tip.y + 10 + 'px'}>
      {tip.label}
    </div>
  {/if}
</div>

<style>
  .canvas {
    position: relative;
    width: 100%;
    height: 100%;
    min-height: 0;
    /* Gleiche Kontrast-Quelle wie GraphView (U02-W4): --graph-bg, nicht
       --surface-0 — deck zeichnet transparent, der Host trägt das Theme. */
    background: var(--graph-bg);
    overflow: hidden;
  }
  canvas {
    position: absolute;
    inset: 0;
    width: 100%;
    height: 100%;
    display: block;
  }
  .tip {
    position: absolute;
    pointer-events: none;
    /* Ordnet nur gegen den Geschwister-Canvas im lokalen Stacking-Context. */
    z-index: var(--z-popover);
    max-width: 18rem;
    padding: 2px 6px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    background: var(--surface-1);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    color: var(--text);
    font-size: var(--fs-xs);
  }
</style>
