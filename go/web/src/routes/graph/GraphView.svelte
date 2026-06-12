<script lang="ts">
  import { onMount } from 'svelte'
  import Sigma from 'sigma'
  import type { DirectedGraph } from 'graphology'
  import { edgeVisible, nodeVisible, type GraphFilters } from '../../lib/graph/filters'
  import { remainingDegree, type EdgeAttrs, type NodeAttrs } from '../../lib/graph/graph-client'

  let {
    graph,
    filters,
    selected = null,
    onnodeclick,
    onnodedoubleclick,
  }: {
    graph: DirectedGraph<NodeAttrs, EdgeAttrs>
    /** Instant client-side filtering via the reducers — no server roundtrip. */
    filters: GraphFilters
    /** Highlighted node (sidebar selection). */
    selected?: string | null
    /** Single click — opens the detail sidebar. */
    onnodeclick?: (id: string) => void
    /** Double click = expand (+1 hop), design 05-§2. */
    onnodedoubleclick?: (id: string) => void
  } = $props()

  let container: HTMLDivElement
  let renderer: Sigma<NodeAttrs, EdgeAttrs> | null = null

  // Mount/kill with the component (design 04/05: sigma renders the
  // graphology instance directly; data changes go through graph mutations,
  // sigma picks them up via graphology events — no Svelte reactivity here).
  onMount(() => {
    const r = new Sigma(graph, container, {
      labelRenderedSizeThreshold: 6,
      labelDensity: 0.7,
      hideEdgesOnMove: true, // LOD (W4): edges drop during pan/zoom
      labelColor: { color: '#8a8d9c' },
      labelFont: 'ui-monospace, monospace',
      labelSize: 11,
      defaultEdgeColor: '#34344a',
      renderEdgeLabels: false,
      // Filters hide, selection highlights, degree badge ("· +N" = visible
      // incidences not loaded yet) decorates. Props are reactive getters —
      // the reducers always read the current filter object.
      nodeReducer: (node, data) => {
        if (!nodeVisible(data, filters)) return { ...data, hidden: true }
        const badge = remainingDegree(data)
        const label = badge === null ? data.label : `${data.label} · +${badge}`
        return { ...data, label, highlighted: node === selected }
      },
      // sigma skips edges with hidden endpoints on its own — this only
      // applies the edge-level class/confidence gate.
      edgeReducer: (_edge, data) => (edgeVisible(data, filters) ? data : { ...data, hidden: true }),
    })
    r.on('clickNode', ({ node }) => onnodeclick?.(node))
    r.on('doubleClickNode', ({ node, event }) => {
      event.preventSigmaDefault() // keep the default double-click zoom away
      onnodedoubleclick?.(node)
    })
    renderer = r
    return () => {
      r.kill()
      renderer = null
    }
  })

  // Filter/selection changes re-run the reducers (filters arrives as a NEW
  // object from the panel, so reading the references is enough tracking).
  $effect(() => {
    void filters
    void selected
    renderer?.refresh()
  })

  /** Re-center the camera (page calls this after a focus jump). */
  export function resetCamera(): void {
    renderer?.getCamera().animatedReset({ duration: 300 })
  }
</script>

<div class="canvas" bind:this={container}></div>

<style>
  .canvas {
    width: 100%;
    height: 100%;
    min-height: 24rem;
    background: var(--surface-0);
  }
  /* sigma creates its own canvases inside; nothing to style beyond the host. */
</style>
