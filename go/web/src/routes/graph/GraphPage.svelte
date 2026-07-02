<script lang="ts">
  import { onMount } from 'svelte'
  import { toApiError, type ApiError } from '../../lib/api'
  import { fetchEgo } from '../../lib/graph/api'
  import { LayoutRunner } from '../../lib/graph/fa2'
  import { defaultFilters, toEgoQuery } from '../../lib/graph/filters'
  import { createGraph, evict, mergeEgo, recomputeHops, touch } from '../../lib/graph/graph-client'
  import { readGraphPalette, recolorGraph } from '../../lib/graph/graph-theme'
  import { THEME_CHANGE_EVENT } from '../../lib/theme/theme.svelte'
  import { WindowStore } from '../../lib/windows/windows.svelte'
  import BlockDetailContent from './BlockDetailContent.svelte'
  import FilterPanel from './FilterPanel.svelte'
  import GraphView from './GraphView.svelte'
  import OverviewMap from './OverviewMap.svelte'
  import SearchBox from './SearchBox.svelte'
  import WindowManager from './WindowManager.svelte'

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
  // G5: node-click opens a floating window (multiple blocks open at once) instead
  // of a single-selection sidebar scalar. The store holds windows as logical
  // units; WindowManager renders them over the canvas. openIds/topId drive the
  // node highlight. In-memory per page-visit (mirrors the ephemeral graph).
  const store = new WindowStore()
  let viewportEl = $state<HTMLElement>()
  let loadedCategories = $state<string[]>([])
  // Theme-aware graph colors read once from the CSS tokens (dark fallback if
  // the theme axis hasn't shipped). G1 only seeds merges/Sigma from this; the
  // re-color-on-theme-switch listener is G2 (design 03-§4).
  let palette = $state(readGraphPalette())

  // a11y: a node-click window has no triggerEl (the Sigma node has no DOM
  // target) — close() must return focus to the graph region, never <body>.
  // The focusable .viewport (tabindex=-1) is that fallback.
  $effect(() => {
    store.fallbackFocusEl = viewportEl ?? null
  })

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
    // No selected-scalar cleanup (G5): a window survives an evicted node — its
    // content loads from the API and BlockDetailContent guards on hasNode.
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
    store.closeAll()
    error = null
    const url = new URL(location.href)
    url.searchParams.delete('focus')
    history.replaceState(null, '', url)
  }
</script>

<section class="area">
  <!-- Base canvas layer — fills the whole content region (S5 definite-height
       chain); the former in-page chrome floats as overlays on top (G3). G5:
       the former DetailSidebar <aside> slot is replaced by the WindowManager
       overlay (floating windows over the canvas); the viewport is a focusable
       fallback target for window-close focus return. -->
  {#if focus !== null}
    <div class="stage">
      <div class="viewport" bind:this={viewportEl} tabindex="-1">
        <GraphView
          bind:this={view}
          {graph}
          {filters}
          {palette}
          highlightedIds={store.openIds}
          topId={store.topId}
          onnodeclick={(id) => store.open(id, null)}
          onnodedoubleclick={(id) => void expand(id)}
        />
      </div>
      <!-- U02 (design 04-§4.6): die Fenster-Schicht ist graph-frei; DIESE
           Kompositions-Wurzel injiziert die beiden Graph-Kopplungen —
           labelFor (Titel/Chips) und das BlockDetailContent-Snippet (Body). -->
      <WindowManager
        {store}
        labelFor={(id) => (graph.hasNode(id) ? graph.getNodeAttribute(id, 'label') : id.slice(0, 8))}
      >
        {#snippet content(win, titleId)}
          <BlockDetailContent
            id={win.id}
            {graph}
            onfocus={(id) => void setFocus(id)}
            onexpand={(id) => void expand(id)}
            {titleId}
          />
        {/snippet}
      </WindowManager>
    </div>
  {:else}
    <div class="stage">
      <OverviewMap onpick={(id) => void setFocus(id)} {palette} />
    </div>
  {/if}

  {#if error}
    <div class="error" role="alert">
      <p>{error.message}</p>
      {#if error.requestId}
        <p class="request-id">request {error.requestId}</p>
      {/if}
    </div>
  {/if}

  <!-- Left overlay column: search (always mounted) + focus controls (meta-row,
       filters). pointer-events:none on the column lets the gaps pan the canvas;
       each card re-enables events on itself. -->
  <div class="chrome-left">
    <div class="card search-card">
      <SearchBox onpick={(id) => void setFocus(id)} />
    </div>
    {#if focus !== null}
      <div class="card meta-row">
        <button class="back" type="button" onclick={backToOverview}>← map</button>
        <code class="focus" title="focused block">{focus}</code>
        {#if busy}
          <span class="loading" aria-busy="true">loading…</span>
        {/if}
        <span class="stats">
          {nodeCount} nodes · {edgeCount} edges{truncated ? ' · server truncated' : ''}
        </span>
      </div>
      <FilterPanel {filters} categories={loadedCategories} onchange={(next) => (filters = next)} />
    {/if}
  </div>

  {#if focus !== null}
    <p class="hint">click a node for details · double-click to expand (+1 hop) · “· +N” = links not loaded yet</p>
  {/if}
</section>

<style>
  /* G3 canvas-first (design 03-§4/§6). S5 gave the content region a definite
     height (AppShell 100dvh grid); .area is now the positioning STAGE for a
     full-bleed graph/overview canvas, and the former in-page chrome (h1/.sub
     gone — the shell carries the area title; search/filters/stats/hint) floats
     as absolute overlays on top. overflow:hidden clips overlay shadows/edges to
     the region. */
  .area {
    position: relative;
    width: 100%;
    height: 100%;
    min-height: 0;
    overflow: hidden;
  }

  /* Base canvas layer: absolute inset:0 → definite size = whole region. The
     focus stage holds the viewport (GraphView) + the WindowManager overlay
     (floating windows, G5); the overview stage holds the full-bleed OverviewMap.
     Overlays stack above via z-index (.area is the containing block; the windows
     use --z-window > --z-overlay). */
  .stage {
    position: absolute;
    inset: 0;
    display: flex;
    gap: var(--space-3);
    min-height: 0;
  }
  .viewport {
    flex: 1;
    min-width: 0;
    min-height: 0;
    overflow: hidden;
  }

  /* ── Overlay chrome ────────────────────────────────────────────────── */
  .chrome-left {
    position: absolute;
    top: var(--space-3);
    left: var(--space-3);
    z-index: var(--z-overlay);
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    width: min(30rem, calc(100% - 2 * var(--space-3)));
    pointer-events: none;
  }
  /* Re-enable events on the cards; the column's gaps stay click-through. */
  .chrome-left > :global(*) {
    pointer-events: auto;
  }

  .card {
    border: 1px solid var(--border);
    border-radius: var(--radius);
    background: var(--surface-1);
    padding: var(--space-2) var(--space-3);
  }
  /* SearchBox renders its form + results dropdown; the card frames both so they
     read over the canvas. Tighter padding — the form/list bring their own. */
  .search-card {
    padding: var(--space-2);
  }

  .error {
    position: absolute;
    top: var(--space-3);
    left: 50%;
    transform: translateX(-50%);
    z-index: var(--z-overlay);
    max-width: min(40rem, calc(100% - 2 * var(--space-3)));
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
    background: var(--surface-2);
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

  /* Dezenter Onboarding-Hint als Overlay unten-zentriert; click-through. */
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
    font-size: 0.8rem;
    text-align: center;
    pointer-events: none;
  }
</style>
