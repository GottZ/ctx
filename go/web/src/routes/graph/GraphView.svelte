<script lang="ts">
  import { onMount } from 'svelte'
  import Sigma from 'sigma'
  import type { DirectedGraph } from 'graphology'
  import type { EdgeAttrs, NodeAttrs } from '../../lib/graph/graph-client'

  let {
    graph,
    onnodeclick,
  }: {
    graph: DirectedGraph<NodeAttrs, EdgeAttrs>
    /** Single click — focus/expand handling lives in the page (W3 grows this). */
    onnodeclick?: (id: string) => void
  } = $props()

  let container: HTMLDivElement
  let renderer: Sigma<NodeAttrs, EdgeAttrs> | null = null

  // Mount/kill with the component (design 04/05: sigma renders the
  // graphology instance directly; data changes go through graph mutations,
  // sigma picks them up via graphology events — no Svelte reactivity here).
  onMount(() => {
    const r = new Sigma(graph, container, {
      labelRenderedSizeThreshold: 6,
      labelColor: { color: '#8a8d9c' },
      labelFont: 'ui-monospace, monospace',
      labelSize: 11,
      defaultEdgeColor: '#34344a',
      renderEdgeLabels: false,
    })
    r.on('clickNode', ({ node }) => onnodeclick?.(node))
    renderer = r
    return () => {
      r.kill()
      renderer = null
    }
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
