<script lang="ts">
  import { onMount } from 'svelte'
  // cosmos.gl (vormals @cosmograph/cosmos): GPU-Force-Layout + GPU-Rendering in
  // einem — dieser Renderer BESITZT das Layout (caps.ownsLayout, renderers.ts);
  // den FA2-Worker der Seite stoppt GraphPage beim Aktivieren.
  import { Graph as CosmosGraph } from '@cosmos.gl/graph'
  import type { MultiDirectedGraph } from 'graphology'
  import type { GraphFilters } from '../../../lib/graph/filters'
  import type { EdgeAttrs, NodeAttrs } from '../../../lib/graph/graph-client'
  import type { GraphPalette } from '../../../lib/graph/graph-theme'
  import { buildEdgeBuffers, buildNodeBuffers } from '../../../lib/graph/render-buffers'

  let {
    graph,
    filters,
    palette,
    highlightedIds,
    topId = null,
    graphRev = 0,
    onnodeclick,
    onnodedoubleclick,
  }: {
    graph: MultiDirectedGraph<NodeAttrs, EdgeAttrs>
    /** Client-side filtering — cosmos kennt kein hidden, wir verdichten (s.u.). */
    filters: GraphFilters
    /** Theme-aware colors; die Node-/Edge-attrs sind bereits palette-gebacken
     *  (recolorGraph), hier steuert palette nur Config-Farben (Hover-Ring). */
    palette: GraphPalette
    /** Nodes with an open floating window — rendered with a size bump (G5-Analog). */
    highlightedIds?: Set<string>
    /** Top (focused) window's node — stärkster Bump. */
    topId?: string | null
    /** Merge-Revision aus GraphPage: graphology ist bewusst NICHT reaktiv, dieser
     *  Tick ist das Signal „Topologie/Attrs haben sich geändert → Buffer neu". */
    graphRev?: number
    /** Single click — opens a floating window for the node. */
    onnodeclick?: (id: string) => void
    /** Double click = expand (+1 hop) — cosmos hat kein dblclick-Event, wir
     *  erkennen es manuell (zweiter Klick auf denselben Punkt < 300 ms). */
    onnodedoubleclick?: (id: string) => void
  } = $props()

  // cosmos bekommt einen EIGENEN inneren Host (.gl): die Instanz hängt Canvas +
  // Attribution-Div selbst in ihr div — Svelte-Anker (Tooltip-{#if}) teilen sich
  // besser keinen Parent mit fremdverwaltetem DOM.
  let glHost: HTMLDivElement
  // Instanz außerhalb der Svelte-Reaktivität (GraphView-Muster): kein $state —
  // die Effects lesen sie nur, getriggert wird über graphRev/filters/palette.
  let cosmos: CosmosGraph | null = null

  // Kompakter Sicht-Stand des letzten rebuild(): cosmos adressiert Punkte über
  // Indizes in den hochgeladenen Arrays — das Mapping cosmos-Index ↔ node-id
  // ist der Picking-Rückweg (Klick/Hover) bzw. Hinweg (Highlight-Bumps).
  let visIds: string[] = []
  let visLabels: string[] = []
  let visIndex = new Map<string, number>()
  /** Ungebumpte Größen aus den Buffern — Basis für sizesWithBumps(). */
  let baseSizes = new Float32Array(0)

  // Manuelle Doppelklick-Erkennung (cosmos hat nur onPointClick): der zweite
  // Klick auf DENSELBEN Punkt < DBL_MS feuert doubleclick STATT des zweiten
  // Single-Handlings (sonst würde jedes Expand auch ein zweites Fenster öffnen).
  const DBL_MS = 300
  let lastClickIdx = -1
  let lastClickAt = 0

  // Hover-Tooltip als HTML-Overlay: cosmos rendert keine Labels (caps.labels
  // false) — der Tooltip ist der einzige Label-Zugang dieses Renderers.
  // Geometrie als style:-Direktive (erlaubt), Farben/Typo im style-Block.
  // (Kein wörtliches style-TAG in Script-Kommentaren erwähnen: svelte2tsx
  // extrahiert Tags per Regex und fehl-matcht daran — svelte-check bräche.)
  let tip = $state<{ x: number; y: number; label: string } | null>(null)

  /** --graph-bg aufgelöst (cosmos frisst Farb-STRINGS, kein var()) — dieselbe
   *  Token-Quelle wie der Host-Hintergrund und die Kontrast-Gates (U02-W4).
   *  Fallback = Dark-Wert aus tokens.css :root (SSR/Token-Divergenz). */
  function readGraphBg(): string {
    if (typeof document === 'undefined') return '#0b0b0f'
    const raw = getComputedStyle(document.documentElement).getPropertyValue('--graph-bg').trim()
    return raw || '#0b0b0f'
  }

  function handlePointClick(index: number): void {
    const id = visIds[index]
    if (id === undefined) return
    const now = performance.now()
    if (index === lastClickIdx && now - lastClickAt < DBL_MS) {
      lastClickIdx = -1 // verbraucht — ein Dreifachklick startet die Erkennung neu
      onnodedoubleclick?.(id)
      return
    }
    lastClickIdx = index
    lastClickAt = now
    onnodeclick?.(id)
  }

  /** cosmos liefert die SPACE-Position des Punkts; spaceToScreenPosition mappt
   *  in Canvas-Pixel — der Tooltip sitzt absolut im Host, der deckungsgleich
   *  mit dem Canvas ist. Refeuert laut API auch bei Punkt-Bewegung unterm
   *  stehenden Cursor (Simulation läuft!) — die Position bleibt so frisch. */
  function handlePointOver(index: number, spacePos: [number, number]): void {
    const c = cosmos
    const label = visLabels[index]
    if (c === null || label === undefined) return
    const [x, y] = c.spaceToScreenPosition(spacePos)
    tip = { x, y, label }
  }

  /** Größen-Array mit Selektions-Bumps: offene Fenster ×1.3, Top-Fenster ×1.5
   *  (cosmos hat kein `highlighted`-Displayfeld wie sigma — greyout-Config wäre
   *  invertierte Semantik, der Bump entspricht dem sigma-Verhalten). */
  function sizesWithBumps(): Float32Array {
    const sizes = new Float32Array(baseSizes)
    if (highlightedIds !== undefined) {
      for (const id of highlightedIds) {
        const j = visIndex.get(id)
        if (j !== undefined) sizes[j] = baseSizes[j] * 1.3
      }
    }
    if (topId !== null && topId !== undefined) {
      const j = visIndex.get(topId)
      if (j !== undefined) sizes[j] = baseSizes[j] * 1.5
    }
    return sizes
  }

  /**
   * Voller Buffer-Neuaufbau: graphology → TypedArrays (render-buffers) → auf
   * die Filter-SICHTBAREN Punkte/Kanten verdichten → cosmos-Upload → Simulation
   * (re)starten. Positionen seeden aus graphology-x/y — beim Umschalten vom
   * Sigma-Renderer bleibt so die Ego-Form erhalten (rescalePositions mappt
   * form-erhaltend in den cosmos-Space). cosmos-Positionen werden bewusst NIE
   * nach graphology zurückgeschrieben (Layout-Besitz endet an dieser Grenze) —
   * Konsequenz: jeder rebuild re-seedet, die Simulation formt neu aus.
   */
  function rebuild(): void {
    const c = cosmos
    if (c === null) return
    const nb = buildNodeBuffers(graph, filters)
    const eb = buildEdgeBuffers(graph, filters, nb)

    // Verdichtung: alter Buffer-Index → kompakter cosmos-Index (-1 = gefiltert).
    const n = nb.ids.length
    const remap = new Int32Array(n).fill(-1)
    let k = 0
    for (let i = 0; i < n; i++) if (nb.visible[i] === 1) remap[i] = k++

    const positions = new Float32Array(k * 2)
    const colors = new Float32Array(k * 4)
    baseSizes = new Float32Array(k)
    visIds = new Array<string>(k)
    visLabels = new Array<string>(k)
    visIndex = new Map()
    for (let i = 0; i < n; i++) {
      const j = remap[i]
      if (j < 0) continue
      positions[j * 2] = nb.positions[i * 2]
      positions[j * 2 + 1] = nb.positions[i * 2 + 1]
      colors.set(nb.colors.subarray(i * 4, i * 4 + 4), j * 4)
      baseSizes[j] = nb.sizes[i]
      visIds[j] = nb.ids[i]
      visLabels[j] = nb.labels[i]
      visIndex.set(nb.ids[i], j)
    }

    // Kanten: eb.visible verlangt bereits BEIDE Endpunkte sichtbar
    // (render-buffers) — remap liefert für sichtbare Kanten garantiert ≥ 0.
    const m = eb.visible.length
    let ke = 0
    for (let e = 0; e < m; e++) if (eb.visible[e] === 1) ke++
    const links = new Float32Array(ke * 2)
    const linkColors = new Float32Array(ke * 4)
    let w = 0
    for (let e = 0; e < m; e++) {
      if (eb.visible[e] !== 1) continue
      links[w * 2] = remap[eb.pairs[e * 2]]
      links[w * 2 + 1] = remap[eb.pairs[e * 2 + 1]]
      linkColors.set(eb.colors.subarray(e * 4, e * 4 + 4), w * 4)
      w++
    }

    c.setPointPositions(positions)
    c.setPointColors(colors)
    c.setPointSizes(sizesWithBumps())
    c.setLinks(links)
    c.setLinkColors(linkColors)
    // Indizes sind ab jetzt neu — ein stehender Tooltip / halber Doppelklick
    // würde sonst auf den falschen Punkt zeigen.
    tip = null
    lastClickIdx = -1
    c.render()
    // Nach jedem Daten-Update neu anwerfen: start() reheated eine laufende
    // Simulation (alpha-Reset) bzw. startet eine gestoppte/pausierte.
    c.start()
  }

  // onMount VOR den $effects deklariert: beide sind intern Effects und laufen
  // in Deklarations-Reihenfolge — beim ersten Effect-Lauf steht die Instanz.
  onMount(() => {
    const c = new CosmosGraph(glHost, {
      // Der Existenzzweck dieses Renderers: GPU-Simulation AN.
      enableSimulation: true,
      backgroundColor: readGraphBg(),
      // Daten-Updates snappen: die 800-ms-Default-Transition würde bei jedem
      // setPointPositions die laufende Simulation AUTOMATISCH pausieren
      // (config.d.ts §transitionDuration) — für einen Force-Renderer, der nach
      // jedem Merge weiterformen soll, kontraproduktiv.
      transitionDuration: 0,
      // graphology-Koordinaten (Jitter/FA2 um 0, auch negativ) form-erhaltend
      // in den cosmos-Space skalieren — mit enableSimulation wäre der Default
      // „nicht rescalen" und die Seeds lägen außerhalb des Simulationsraums.
      rescalePositions: true,
      fitViewOnInit: true,
      hoveredPointCursor: 'pointer',
      // Hover-Affordanz analog sigmas Hover-Kasten: Ring in der Hover-Farbe.
      renderHoveredPointRing: true,
      hoveredPointRingColor: palette.hoverStroke,
      onPointClick: (index) => handlePointClick(index),
      onPointMouseOver: (index, pointPosition) => handlePointOver(index, pointPosition),
      onPointMouseOut: () => {
        tip = null
      },
    })
    cosmos = c
    return () => {
      c.destroy()
      cosmos = null
      tip = null
    }
  })

  // Daten-Effect: graphRev (Merge/Evict), filters (Panel liefert NEUE Objekte)
  // und palette (Theme-Wechsel: GraphPage re-backt die attr-Farben und reicht
  // ein NEUES palette-Objekt) → voller Buffer-Neuaufbau. Hintergrund + Ring
  // sind cosmos-CONFIG, keine Buffer — explizit nachziehen (setConfigPartial
  // lässt alle anderen Werte stehen, anders als setConfig).
  $effect(() => {
    void graphRev
    void filters
    const c = cosmos
    if (c === null) return
    c.setConfigPartial({ backgroundColor: readGraphBg(), hoveredPointRingColor: palette.hoverStroke })
    rebuild()
  })

  // Selektions-Effect: nur Größen-Arrays tauschen — KEIN rebuild, der würde
  // die Positionen aus graphology re-seeden und das laufende Layout wegwerfen.
  $effect(() => {
    void highlightedIds
    void topId
    const c = cosmos
    if (c === null || baseSizes.length === 0) return
    c.setPointSizes(sizesWithBumps())
    c.render()
  })

  /** Kamera auf alle Punkte zentrieren (Seite ruft das nach einem Fokus-Sprung). */
  export function resetCamera(): void {
    cosmos?.fitView(300)
  }

  /** Buffer komplett neu aufbauen (Contract, renderers.ts) — GraphPage ruft das
   *  nach Off-Band-Recolor (AM-2 hues). Achtung Layout-Besitz: re-seedet die
   *  Positionen aus graphology, die Simulation formt danach neu aus. */
  export function refresh(): void {
    rebuild()
  }
</script>

<!-- data-e2e-mask: GPU-Force-Layout ist nicht seed-stabil (cosmos randomSeed
     deckt nicht die GPU-Reduktionen) — das Visual-Gate maskiert die Region,
     analog GraphView. -->
<div class="canvas" data-e2e-mask>
  <div class="gl" bind:this={glHost}></div>
  {#if tip !== null}
    <!-- Geometrie inline (erlaubt), Farben/Typo im style-Block (Lint-Gate). -->
    <div class="tip" style:left={tip.x + 12 + 'px'} style:top={tip.y + 12 + 'px'}>{tip.label}</div>
  {/if}
</div>

<style>
  .canvas {
    position: relative; /* Anker für .gl und den absolut positionierten Tooltip */
    width: 100%;
    height: 100%;
    min-height: 0;
    overflow: hidden; /* Tooltip am Rand darf die Seite nicht aufscrollen */
    /* U02-W4-Analog GraphView: Host-Hintergrund aus DERSELBEN Token-Quelle wie
       die Kontrast-Gates (--graph-bg) — cosmos' Canvas malt denselben Wert. */
    background: var(--graph-bg);
  }
  .gl {
    position: absolute;
    inset: 0; /* deckungsgleich mit .canvas → Canvas-Pixel = Host-Koordinaten */
  }
  .tip {
    position: absolute;
    z-index: var(--z-popover);
    max-width: 20rem;
    overflow: hidden;
    white-space: nowrap;
    text-overflow: ellipsis;
    pointer-events: none; /* nie das Hover-Picking unterm Cursor stören */
    padding: 0.15rem 0.45rem;
    background: var(--surface-1);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    color: var(--text);
    font-size: var(--fs-xs);
    font-family: var(--font-mono);
  }
</style>
