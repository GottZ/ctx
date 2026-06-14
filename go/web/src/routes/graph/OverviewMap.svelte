<script lang="ts">
  import { onMount, tick } from 'svelte'
  import Sigma from 'sigma'
  import { toApiError, type ApiError } from '../../lib/api'
  import { fetchOverview, type OverviewResponse } from '../../lib/graph/api'
  import { buildOverviewGraph, type MetaEdgeAttrs, type MetaNodeAttrs } from '../../lib/graph/overview-map'

  // Single click on a cluster → drill into its representative's ego net.
  let { onpick }: { onpick: (reprId: string) => void } = $props()

  let container: HTMLDivElement
  let renderer: Sigma<MetaNodeAttrs, MetaEdgeAttrs> | null = null
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
        const graph = buildOverviewGraph(resp)
        const r = new Sigma(graph, container, {
          labelRenderedSizeThreshold: 4, // meta-nodes are few — label generously
          labelColor: { color: '#8a8d9c' },
          labelFont: 'ui-monospace, monospace',
          labelSize: 11,
          defaultEdgeColor: '#34344a',
          renderEdgeLabels: false,
        })
        r.on('clickNode', ({ node }) => {
          const reprId = graph.getNodeAttribute(node, 'reprId')
          if (reprId) onpick(reprId)
        })
        renderer = r
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
    }
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
    <div class="msg">
      no clusters yet — the overview is rebuilt periodically by the dream-link clustering job
    </div>
  {/if}

  <!-- Container stays mounted so onMount's bind:this is always live. -->
  <div
    class="canvas"
    bind:this={container}
    class:hidden={loading || error !== null || (stats !== null && stats.nodes === 0)}
  ></div>

  <p class="hint">click a cluster to drill into its representative's ego net</p>
</div>

<style>
  .overview {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    flex: 1;
    min-height: 30rem;
  }
  .bar {
    display: flex;
    align-items: baseline;
    gap: var(--space-3);
  }
  .stat {
    margin-left: auto;
    color: var(--text-dim);
    font-family: var(--font-mono);
    font-size: 0.8rem;
  }
  .msg {
    color: var(--text-dim);
    font-size: 0.85rem;
    padding: var(--space-2) 0;
  }
  .canvas {
    flex: 1;
    min-height: 24rem;
    border: 1px solid var(--border);
    border-radius: var(--radius);
    overflow: hidden;
    background: var(--surface-0);
  }
  .canvas.hidden {
    display: none;
  }
  .hint {
    margin: 0;
    color: var(--text-faint);
    font-size: 0.875rem;
  }
</style>
