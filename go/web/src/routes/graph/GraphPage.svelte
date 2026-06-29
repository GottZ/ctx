<script lang="ts">
  import { onMount } from 'svelte'
  import { toApiError, type ApiError } from '../../lib/api'
  import { fetchEgo } from '../../lib/graph/api'
  import { LayoutRunner } from '../../lib/graph/fa2'
  import { defaultFilters, toEgoQuery } from '../../lib/graph/filters'
  import { createGraph, evict, mergeEgo, recomputeHops, touch } from '../../lib/graph/graph-client'
  import { readGraphPalette, recolorGraph } from '../../lib/graph/graph-theme'
  import { THEME_CHANGE_EVENT } from '../../lib/theme/theme.svelte'
  import DetailSidebar from './DetailSidebar.svelte'
  import FilterPanel from './FilterPanel.svelte'
  import GraphView from './GraphView.svelte'
  import OverviewMap from './OverviewMap.svelte'
  import SearchBox from './SearchBox.svelte'

  // ONE graphology instance per page visit = single source of truth
  // (design 05-§3.5). Deliberately NOT $state — sigma observes the graph
  // through graphology events, Svelte only tracks the scalars below.
  const graph = createGraph()
  const layout = new LayoutRunner()

  let view = $state<ReturnType<typeof GraphView> | null>(null)
  let focus = $state<string | null>(null)
  let busy = $state(false)
  let error = $state<ApiError | null>(null)
  let truncated = $state(false)
  // Explicit counters: graph.order/size are non-reactive by design.
  let nodeCount = $state(0)
  let edgeCount = $state(0)
  // W4: filters drive the reducers instantly AND the params of new expands.
  let filters = $state(defaultFilters())
  let selected = $state<string | null>(null)
  let loadedCategories = $state<string[]>([])
  // Theme-aware graph colors read once from the CSS tokens (dark fallback if
  // the theme axis hasn't shipped). G1 only seeds merges/Sigma from this; the
  // re-color-on-theme-switch listener is G2 (design 03-§4).
  let palette = $state(readGraphPalette())

  // Deep-link sync: /graph?focus=<uuid> survives reload and is shareable.
  onMount(() => {
    const fromUrl = new URLSearchParams(location.search).get('focus')
    if (fromUrl) void setFocus(fromUrl, { pushUrl: false })
    // G2: live re-color on theme switch (design 03-§4). The theme controller
    // writes data-theme THEN fires THEME_CHANGE_EVENT, so readGraphPalette()
    // here already sees the new --graph-* tokens. Re-bake the baked color attrs
    // on the live graphology instance (recolorGraph) and reassign the palette
    // $state — the latter flows to GraphView/OverviewMap, which push the new
    // Sigma label/edge settings + refresh(). No remount, no re-layout.
    const onThemeChange = (): void => {
      palette = readGraphPalette()
      recolorGraph(graph, palette)
    }
    window.addEventListener(THEME_CHANGE_EVENT, onThemeChange)
    return () => {
      window.removeEventListener(THEME_CHANGE_EVENT, onThemeChange)
      layout.dispose()
    }
  })

  /** Post-merge bookkeeping shared by focus + expand: BFS hops → eviction
   *  (hard 5k/20k ceiling, §6.6) → layout kick → reactive counters. */
  function settle(): void {
    if (focus !== null) {
      recomputeHops(graph, focus)
      evict(graph, focus)
    }
    layout.run(graph)
    nodeCount = graph.order
    edgeCount = graph.size
    const cats = new Set<string>()
    graph.forEachNode((_id, attrs) => cats.add(attrs.category))
    loadedCategories = [...cats].sort()
    if (selected !== null && !graph.hasNode(selected)) selected = null
  }

  async function setFocus(id: string, opts: { pushUrl?: boolean } = {}): Promise<void> {
    if (busy) return
    busy = true
    error = null
    try {
      const resp = await fetchEgo(id, { hops: 2, ...toEgoQuery(filters) })
      focus = id
      mergeEgo(graph, resp, palette)
      truncated = resp.stats.truncated
      settle()
      if (opts.pushUrl !== false) {
        const url = new URL(location.href)
        url.searchParams.set('focus', id)
        history.replaceState(null, '', url)
      }
      view?.resetCamera()
    } catch (err) {
      error = toApiError(err)
    } finally {
      busy = false
    }
  }

  /** Double-click expand: +1 hop around the node, focus stays put. */
  async function expand(id: string): Promise<void> {
    if (busy) return
    busy = true
    error = null
    try {
      touch(graph, id)
      const resp = await fetchEgo(id, { hops: 1, ...toEgoQuery(filters) })
      mergeEgo(graph, resp, palette)
      truncated = resp.stats.truncated
      settle()
    } catch (err) {
      error = toApiError(err)
    } finally {
      busy = false
    }
  }

  /** Back to the cluster map (clears the focus; the ego graph stays in memory). */
  function backToOverview(): void {
    focus = null
    selected = null
    error = null
    const url = new URL(location.href)
    url.searchParams.delete('focus')
    history.replaceState(null, '', url)
  }
</script>

<section class="area">
  <header>
    <h1>Graph</h1>
    <p class="sub">
      dream-link graph — start from the cluster map or search a block; click to focus, double-click
      a node to expand (read-only, no LLM touched)
    </p>
  </header>

  <SearchBox onpick={(id) => void setFocus(id)} />

  {#if error}
    <div class="error" role="alert">
      <p>{error.message}</p>
      {#if error.requestId}
        <p class="request-id">request {error.requestId}</p>
      {/if}
    </div>
  {/if}

  {#if focus !== null}
    <FilterPanel {filters} categories={loadedCategories} onchange={(next) => (filters = next)} />
    <div class="meta-row">
      <button class="back" type="button" onclick={backToOverview}>← map</button>
      <code class="focus" title="focused block">{focus}</code>
      {#if busy}
        <span class="loading" aria-busy="true">loading…</span>
      {/if}
      <span class="stats">
        {nodeCount} nodes · {edgeCount} edges{truncated ? ' · server truncated' : ''}
      </span>
    </div>
    <div class="stage">
      <div class="viewport">
        <GraphView
          bind:this={view}
          {graph}
          {filters}
          {palette}
          {selected}
          onnodeclick={(id) => (selected = id)}
          onnodedoubleclick={(id) => void expand(id)}
        />
      </div>
      {#if selected !== null}
        <DetailSidebar
          id={selected}
          {graph}
          onfocus={(id) => void setFocus(id)}
          onexpand={(id) => void expand(id)}
          onclose={() => (selected = null)}
        />
      {/if}
    </div>
    <p class="hint">click a node for details · double-click to expand (+1 hop) · “· +N” = links not loaded yet</p>
  {:else}
    <OverviewMap onpick={(id) => void setFocus(id)} {palette} />
  {/if}
</section>

<style>
  .area {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    height: 100%;
  }

  header {
    border-bottom: 1px solid var(--border);
    padding-bottom: var(--space-2);
  }
  h1 {
    margin: 0;
    font-size: 1.35rem;
    font-weight: 600;
    letter-spacing: 0.01em;
  }
  .sub {
    margin: var(--space-1) 0 0;
    color: var(--text-dim);
    font-size: 0.85rem;
  }

  .error {
    border: 1px solid var(--danger);
    border-radius: var(--radius);
    background: var(--danger-dim);
    padding: var(--space-2) var(--space-3);
    font-size: 0.85rem;
  }
  .error p {
    margin: 0;
    color: var(--danger);
  }
  .request-id {
    font-family: var(--font-mono);
    font-size: 0.75rem;
    color: var(--text-dim) !important;
  }

  .meta-row {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    min-height: 1.4rem;
  }
  .focus {
    font-size: 0.75rem;
  }
  .back {
    border: 1px solid var(--border);
    border-radius: var(--radius);
    background: var(--surface-0);
    color: var(--text);
    cursor: pointer;
    font-size: 0.75rem;
    padding: 0.15rem 0.55rem;
  }
  .back:hover {
    border-color: var(--text-dim);
  }
  .loading {
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: 0.8rem;
  }
  .stats {
    margin-left: auto;
    color: var(--text-dim);
    font-family: var(--font-mono);
    font-size: 0.8rem;
  }

  .stage {
    flex: 1;
    display: flex;
    gap: var(--space-3);
    min-height: 30rem;
  }
  .viewport {
    flex: 1;
    min-width: 0;
    border: 1px solid var(--border);
    border-radius: var(--radius);
    overflow: hidden;
  }

  .hint {
    margin: 0;
    color: var(--text-faint);
    font-size: 0.875rem;
  }
</style>
