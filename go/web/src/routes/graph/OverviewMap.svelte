<script lang="ts">
  import { onMount, tick } from 'svelte'
  import type { UndirectedGraph } from 'graphology'
  import Sigma from 'sigma'
  import { toApiError, type ApiError } from '../../lib/api'
  import { fetchOverview, type OverviewResponse } from '../../lib/graph/api'
  import { buildOverviewGraph, type MetaEdgeAttrs, type MetaNodeAttrs } from '../../lib/graph/overview-map'
  import { recolorOverviewGraph, type GraphPalette } from '../../lib/graph/graph-theme'
  import EmptyState from '../../lib/ui/EmptyState.svelte'

  // Single click on a cluster → drill into its representative's ego net.
  let { onpick, palette }: { onpick: (reprId: string) => void; palette: GraphPalette } = $props()

  let container: HTMLDivElement
  let renderer: Sigma<MetaNodeAttrs, MetaEdgeAttrs> | null = null
  // Retained for the G2 theme re-color: the meta-graph instance + the wire
  // response it was built from (the meta-nodes carry no category, so recolor
  // re-derives node colors from the response — see recolorOverviewGraph).
  let metaGraph: UndirectedGraph<MetaNodeAttrs, MetaEdgeAttrs> | null = null
  let overviewResp: OverviewResponse | null = null
  let loading = $state(true)
  let error = $state<ApiError | null>(null)
  let stats = $state<OverviewResponse['stats'] | null>(null)

  onMount(() => {
    let killed = false
    void (async () => {
      try {
        const resp = await fetchOverview()
        if (killed) return
        stats = resp.stats
        loading = false
        if (resp.nodes.length === 0) return
        // Flush the DOM (loading=false un-hides the canvas) BEFORE Sigma
        // measures the container — otherwise it is still display:none and
        // Sigma throws "Container has no width". Svelte reactivity is async,
        // so a tick() is required between the state change and the mount.
        await tick()
        if (killed) return
        const graph = buildOverviewGraph(resp, palette)
        metaGraph = graph
        overviewResp = resp
        const r = new Sigma(graph, container, {
          labelRenderedSizeThreshold: 4, // meta-nodes are few — label generously
          labelColor: { color: palette.labelColor },
          labelFont: 'ui-monospace, monospace',
          labelSize: 11,
          defaultEdgeColor: palette.edgeColor,
          renderEdgeLabels: false,
        })
        r.on('clickNode', ({ node }) => {
          const reprId = graph.getNodeAttribute(node, 'reprId')
          if (reprId) onpick(reprId)
        })
        renderer = r
        // Test-hook (design 03-§4/§6) — see GraphView; dev/test only.
        if ((import.meta.env.DEV || import.meta.env.VITE_E2E) && typeof window !== 'undefined') {
          ;(window as unknown as { __ctxGraph?: unknown }).__ctxGraph = { renderer: r, graph }
        }
      } catch (err) {
        if (!killed) {
          error = toApiError(err)
          loading = false
        }
      }
    })()
    return () => {
      killed = true
      renderer?.kill()
      renderer = null
      metaGraph = null
      overviewResp = null
    }
  })

  // G2: theme switch hands down a NEW palette (GraphPage reassigns its $state on
  // THEME_CHANGE_EVENT). This map owns its own sigma instance + meta-graph, so
  // it re-bakes its node/edge color attrs (recolorOverviewGraph, from the
  // retained response) and pushes the label/edge Sigma settings + refresh().
  // No remount, no FA2 re-layout — positions stay put. Skips until the async
  // load has populated the graph (the initial build already used this palette).
  $effect(() => {
    // Read `palette` FIRST so it is always a tracked dependency. renderer/
    // metaGraph/overviewResp are non-$state lets populated by the async onMount
    // load, so on the effect's first run (load still pending) they are null —
    // guarding on them before touching `palette` meant palette never registered
    // as a dependency and the re-color never fired on a later theme switch
    // (caught by the e2e graph-palette smoke). Hoisting the read fixes tracking.
    const p = palette
    const r = renderer
    const g = metaGraph
    const resp = overviewResp
    if (!r || !g || !resp) return
    recolorOverviewGraph(g, resp, p)
    r.setSetting('labelColor', { color: p.labelColor })
    r.setSetting('defaultEdgeColor', p.edgeColor)
    r.refresh()
  })

  // Local date format; the timestamp is the last rebuild, not "now".
  function builtLabel(iso: string): string {
    return new Date(iso).toLocaleString()
  }
</script>

<div class="overview">
  <div class="bar">
    <strong>Cluster map</strong>
    {#if stats}
      <span class="stat">
        {stats.nodes} clusters · {stats.edges} links{stats.computed_at
          ? ` · built ${builtLabel(stats.computed_at)}`
          : ''}{stats.truncated ? ' · truncated' : ''}
      </span>
    {/if}
  </div>

  {#if error}
    <div class="msg" role="alert">
      cluster map unavailable: {error.message}
    </div>
  {:else if loading}
    <div class="msg" aria-busy="true">building map…</div>
  {:else if stats && stats.nodes === 0}
    <div class="empty-overlay">
      <EmptyState
        title="No clusters yet"
        copy="The cluster map is rebuilt periodically by the dream-link clustering job. As your corpus grows, the next run groups related blocks into the clusters that appear here."
      />
    </div>
  {/if}

  <!-- Container stays mounted so onMount's bind:this is always live.
       data-e2e-mask: sigma/ForceAtlas2 pixel output is not seed-stable — the
       visual gate masks this region; graph SEMANTICS are asserted via the
       __ctxGraph test hook instead (design 06 §4.3). -->
  <div
    class="canvas"
    data-e2e-mask
    bind:this={container}
    class:hidden={loading || error !== null || (stats !== null && stats.nodes === 0)}
  ></div>

  <p class="hint">click a cluster to drill into its representative's ego net</p>
</div>

<style>
  /* G3 (design 03-§4): the cluster map is full-bleed. .overview is the
     positioning stage that fills the graph region (definite height via the
     parent .stage); the canvas sits absolute inset:0 (no min-height floor that
     would clip on a short region — Sigma's own ResizeObserver re-measures), and
     bar/hint/msg float as absolute overlays on top. */
  .overview {
    position: relative;
    flex: 1;
    min-width: 0;
    min-height: 0;
    width: 100%;
    height: 100%;
  }
  .canvas {
    position: absolute;
    inset: 0;
    overflow: hidden;
    background: var(--graph-bg);
  }
  .canvas.hidden {
    display: none;
  }
  /* Cluster title + stats — overlay top-right; non-interactive → click-through
     so the canvas pans under it. */
  .bar {
    position: absolute;
    top: var(--space-3);
    right: var(--space-3);
    z-index: var(--z-overlay);
    display: flex;
    align-items: baseline;
    gap: var(--space-3);
    max-width: min(32rem, calc(100% - 2 * var(--space-3)));
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    background: var(--surface-1);
    pointer-events: none;
  }
  .stat {
    margin-left: auto;
    color: var(--text-dim);
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
  }
  /* N8 empty-state: the same centred-overlay framing as .msg, but the title +
     copy come from the shared EmptyState; this wrapper supplies only the
     positioning + card backing so it stays legible over the (hidden) canvas. */
  .empty-overlay {
    position: absolute;
    top: 50%;
    left: 50%;
    transform: translate(-50%, -50%);
    z-index: var(--z-overlay);
    max-width: min(34rem, calc(100% - 2 * var(--space-3)));
    border: 1px solid var(--border);
    border-radius: var(--radius);
    background: var(--surface-1);
  }
  /* Loading / error — centered overlay over the (hidden) canvas. */
  .msg {
    position: absolute;
    top: 50%;
    left: 50%;
    transform: translate(-50%, -50%);
    z-index: var(--z-overlay);
    max-width: min(34rem, calc(100% - 2 * var(--space-3)));
    text-align: center;
    color: var(--text-dim);
    font-size: var(--fs-sm);
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    background: var(--surface-1);
  }
  .hint {
    position: absolute;
    bottom: var(--space-3);
    left: 50%;
    transform: translateX(-50%);
    z-index: var(--z-overlay);
    margin: 0;
    max-width: calc(100% - 2 * var(--space-3));
    padding: var(--space-1) var(--space-3);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    background: var(--surface-1);
    color: var(--text-faint);
    font-size: var(--fs-sm);
    text-align: center;
    pointer-events: none;
  }
</style>
