<script lang="ts">
  import { onMount, tick } from 'svelte'
  // GC1 §4.2: edge-curve-Import nur in Svelte-Komponenten (vitest-node-frei).
  import { DEFAULT_EDGE_CURVATURE, indexParallelEdgesIndex } from '@sigma/edge-curve'
  import { toApiError, type ApiError } from '../../lib/api'
  import { fetchCategoryHues, fetchEgo, fetchGraphAll, isEgoFocusNotFound } from '../../lib/graph/api'
  import { LayoutRunner } from '../../lib/graph/fa2'
  import { defaultFilters, toEgoQuery } from '../../lib/graph/filters'
  import { createGraph, evict, mergeEgo, recomputeHops, touch } from '../../lib/graph/graph-client'
  import { readGraphPalette, recolorGraph } from '../../lib/graph/graph-theme'
  import {
    RENDERERS,
    RENDERER_IDS,
    loadRendererPref,
    saveRendererPref,
    type RendererComponent,
    type RendererExports,
    type RendererId,
  } from '../../lib/graph/renderers'
  import { THEME_CHANGE_EVENT } from '../../lib/theme/theme.svelte'
  import { WindowStore } from '../../lib/windows/windows.svelte'
  import BlockDetailContent from './BlockDetailContent.svelte'
  import FilterPanel from './FilterPanel.svelte'
  import OverviewMap from './OverviewMap.svelte'
  import SearchBox from './SearchBox.svelte'
  import WindowManager from './WindowManager.svelte'

  // ONE graphology instance per page visit = single source of truth
  // (design 05-§3.5). Deliberately NOT $state — sigma observes the graph
  // through graphology events, Svelte only tracks the scalars below.
  const graph = createGraph()
  const layout = new LayoutRunner()

  let view = $state<RendererExports | null>(null)
  let focus = $state<string | null>(null)
  // RV1: Renderer-Wahl (Meta-Row-Select), in localStorage persistiert. Die
  // Komponente lädt lazy — nur der aktive Renderer landet im Bundle; Sigma
  // bleibt Default (e2e-visual-Baselines). threeD gilt nur für caps.threeD-
  // Renderer (deck); three ist immer 3D und ignoriert den Toggle.
  let rendererId = $state<RendererId>(loadRendererPref())
  let RendererComp = $state<RendererComponent | null>(null)
  let threeD = $state(false)
  const rendererCaps = $derived(RENDERERS[rendererId].caps)
  let busy = $state(false)
  let error = $state<ApiError | null>(null)
  // Soft fallback notice (fix C): a focus/expand 404 drops back to the topic
  // map with this banner instead of a hard error — see isEgoFocusNotFound.
  let notice = $state<string | null>(null)
  let truncated = $state(false)
  // Explicit counters: graph.order/size are non-reactive by design.
  let nodeCount = $state(0)
  let edgeCount = $state(0)
  // Structural-edge count (GA2): tracked from the merge on; the stats line
  // renders it in GC3 together with the legend/a11y wave.
  let structCount = $state(0)
  // W4: filters drive the reducers instantly AND the params of new expands.
  let filters = $state(defaultFilters())
  // Display option, deliberately NOT part of GraphFilters: labels touch no
  // reducer predicate and no ego-query param — isDefault()/reset stay filter-
  // pure. Session-scoped like the graph itself (resets with the page visit).
  let showLabels = $state(true)
  // G5: node-click opens a floating window (multiple blocks open at once) instead
  // of a single-selection sidebar scalar. The store holds windows as logical
  // units; WindowManager renders them over the canvas. openIds/topId drive the
  // node highlight. In-memory per page-visit (mirrors the ephemeral graph).
  const store = new WindowStore()
  let viewportEl = $state<HTMLElement>()
  // U03-W1 (§5.2): bind:this auf die Focus-Stage-Region. `.wm-root` ist inset:0
  // dieser Stage (box-gleich), also misst clientWidth/clientHeight dieselbe Box
  // wie der WindowManager. setFocus misst hier VOR store.open die frische
  // Surface (timing-unabhängig von der WindowManager-Mount-Ordnung).
  let stageEl = $state<HTMLElement>()
  let loadedCategories = $state<string[]>([])
  // Structural classes loaded in the client (GC2) — panel checkbox source +
  // toEgoQuery's known set for the unified link_class channel (GB5 contract).
  let loadedStructClasses = $state<string[]>([])
  // Merge revision (GC4): the graphology instance is deliberately non-reactive
  // — open windows re-derive their graph-sourced lists off THIS counter, or
  // they would freeze at window-mount (design 03-§4.5 invalidation source).
  let graphRev = $state(0)
  // Theme-aware graph colors read once from the CSS tokens (dark fallback if
  // the theme axis hasn't shipped). G1 only seeds merges/Sigma from this; the
  // re-color-on-theme-switch listener is G2 (design 03-§4).
  let palette = $state(readGraphPalette())
  // AM-2 (design 02a §A3): aufgelöste sparse Override-Map {Kategorie → HSL-Hue}
  // für den eigenen {_global, Tenant}-View. Beim Mount FIRE-AND-FORGET parallel
  // geladen (NIE vor fetchEgo/fetchOverview awaited — kein Wasserfall auf First
  // Paint); trifft sie nach dem ersten Render ein, ist der einmalige Seed-Farb-
  // Flash + ein Recolor-Pass akzeptiert. Leere Map = alles auf Seed.
  let categoryHues = $state<Map<string, number>>(new Map())

  // a11y: a node-click window has no triggerEl (the Sigma node has no DOM
  // target) — close() must return focus to the graph region, never <body>.
  // The focusable .viewport (tabindex=-1) is that fallback.
  $effect(() => {
    store.fallbackFocusEl = viewportEl ?? null
  })

  // RV1: Renderer lazy laden + Wahl persistieren. Der alive-Guard verwirft
  // ein spät auflösendes Modul nach einem weiteren Switch (letzter gewinnt).
  // Layout-Ownership wandert mit: cosmos simuliert selbst auf der GPU → der
  // FA2-Worker stoppt; zurück auf einen FA2-Renderer kickt das Layout neu an.
  $effect(() => {
    const id = rendererId
    saveRendererPref(id)
    let alive = true
    void RENDERERS[id].load().then((m) => {
      if (!alive) return
      RendererComp = m.default
      if (RENDERERS[id].caps.ownsLayout) layout.stop()
      else if (graph.order > 1) layout.run(graph)
    })
    return () => {
      alive = false
    }
  })

  // Deep-link sync: /graph?focus=<uuid> survives reload and is shareable.
  onMount(() => {
    const fromUrl = new URLSearchParams(location.search).get('focus')
    // U03-W2 (§4.4): ?focus=<uuid> bedeutet einheitlich „diesen Block zeigen" =
    // Ego-Netz + geöffnetes Detail-Fenster (kein neuer Query-Param, ToolCallCard
    // bleibt unangetastet). openTrigger bleibt weg → Close-Fokus fällt auf
    // fallbackFocusEl = .viewport, identisch zum Canvas-Klick-Fenster (§4.3/§4.5).
    if (fromUrl) void setFocus(fromUrl, { pushUrl: false, open: true })
    // AM-2 Override-Fetch: parallel + fire-and-forget (kein await vor dem ersten
    // Render). Erfolg → Map setzen, den bereits geseedeten Ego-Graph nachfärben
    // (recolor-on-arrival) + View-Refresh; die OverviewMap zieht die Map über
    // ihren overrides-$effect nach. Fehlschlag → Seed bleibt stehen (Overrides
    // sind eine Zusatzschicht, kein First-Paint-Blocker).
    void fetchCategoryHues()
      .then((r) => {
        categoryHues = new Map(Object.entries(r.hues))
        recolorGraph(graph, palette, categoryHues)
        view?.refresh()
      })
      .catch(() => {
        /* Overrides optional — der Hash-Seed trägt den Graph ohne sie. */
      })
    // G2: live re-color on theme switch (design 03-§4). The theme controller
    // writes data-theme THEN fires THEME_CHANGE_EVENT, so readGraphPalette()
    // here already sees the new --graph-* tokens. Re-bake the baked color attrs
    // on the live graphology instance (recolorGraph) and reassign the palette
    // $state — the latter flows to GraphView/OverviewMap, which push the new
    // Sigma label/edge settings + refresh(). No remount, no re-layout.
    const onThemeChange = (): void => {
      palette = readGraphPalette()
      // Die Overrides MITFÜHREN (design 02a §A3) — sonst verliert ein Theme-
      // Wechsel jeden Kategorie-Hue-Override und fällt auf den Seed zurück.
      recolorGraph(graph, palette, categoryHues)
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
    // RV1: cosmos besitzt das Layout selbst (GPU-Simulation auf eigenen
    // Buffern) — der FA2-Worker würde auf denselben graphology-Koordinaten
    // gegenarbeiten. Alle anderen Renderer lesen FA2-Positionen.
    if (!rendererCaps.ownsLayout) layout.run(graph)
    nodeCount = graph.order
    edgeCount = graph.size
    graphRev++
    let structural = 0
    const structCls = new Set<string>()
    graph.forEachEdge((_id, attrs) => {
      if (attrs.kind === 'structural') {
        structural++
        structCls.add(attrs.rel)
      }
    })
    structCount = structural
    loadedStructClasses = [...structCls].sort()
    // GC1 §4.2: struct↔struct-Parallelkanten (references UND duplicate-of auf
    // demselben Paar, PK M076) trennen sich per parallel-index-skalierter
    // Curvature — mit identischem Default rendern sie pixel-identisch
    // übereinander. dream↔structural trennt schon Kurve-vs-Linie; dream-Kanten
    // (line-Programm) lesen curvature nicht, nur structural wird geschrieben.
    // O(size) ≤ 20k pro Merge — gleiche Klasse wie die Sweeps oben (§6);
    // ohne structural-Kanten (Alt-Server) entfällt der Pass komplett.
    if (structural > 0) {
      indexParallelEdgesIndex(graph)
      graph.forEachEdge((id, attrs) => {
        if (attrs.kind !== 'structural') return
        // parallelIndex ist um 0 symmetrisch (Paket vergibt -(n-1)/2 + i für
        // gleichgerichtete Gruppen, null für Solo-Kanten). Die Abbildung ist
        // vorzeichen-erhaltend und NULLFREI (negativ → Gegenbogen): eine
        // curvature 0 wäre eine GERADE structural-Kante — pixel-identisch
        // unter einer dream-line, exakt der Bruchpfad, den die Kurve trennt.
        const idx = typeof attrs.parallelIndex === 'number' ? attrs.parallelIndex : 0
        graph.setEdgeAttribute(id, 'curvature', DEFAULT_EDGE_CURVATURE * (idx >= 0 ? 1 + idx : idx))
      })
    }
    const cats = new Set<string>()
    graph.forEachNode((_id, attrs) => cats.add(attrs.category))
    loadedCategories = [...cats].sort()
    // No selected-scalar cleanup (G5): a window survives an evicted node — its
    // content loads from the API and BlockDetailContent guards on hasNode.
  }

  // U03-W1 (§4.1): setFocus lernt `open` — nach Ego-Erfolg öffnet es das
  // Node-Detail-Fenster über DENSELBEN Pfad wie der Canvas-Klick (store.open),
  // Erfolgs-gekoppelt (der catch öffnet nie → kein Zombie-Fenster). `openTrigger`
  // ist das Fokus-Rückgabe-Ziel beim Close (Such-Input, §4.2).
  async function setFocus(
    id: string,
    opts: { pushUrl?: boolean; open?: boolean; openTrigger?: HTMLElement | null } = {},
  ): Promise<void> {
    if (busy) return
    busy = true
    error = null
    notice = null
    try {
      const resp = await fetchEgo(id, { hops: 2, ...toEgoQuery(filters, loadedStructClasses) })
      focus = id
      mergeEgo(graph, resp, palette, categoryHues)
      truncated = resp.stats.truncated
      settle()
      if (opts.pushUrl !== false) {
        const url = new URL(location.href)
        url.searchParams.set('focus', id)
        history.replaceState(null, '', url)
      }
      view?.resetCamera()
      if (opts.open) {
        // §5.2: nach tick() ist die Focus-Stage im DOM committed → die Stage-
        // Surface synchron und FRISCH messen (1 lu = 1 CSS-px, DomProjector-
        // Identität) und in den Store pushen, BEVOR spawnRect sie liest. Damit
        // hängt die Erst-Platzierung an der eigenen Messung, nicht an einer
        // Mount-/Effect-Reihenfolge des WindowManagers.
        await tick()
        if (stageEl) store.setSurface({ wLu: stageEl.clientWidth, hLu: stageEl.clientHeight })
        store.open(id, opts.openTrigger ?? null)
      }
    } catch (err) {
      const apiErr = toApiError(err)
      if (isEgoFocusNotFound(apiErr)) {
        // The focus block does not exist or is not graph-visible (archived,
        // retrieval-excluded type like system-meta, foreign scope) — the ego
        // route answers one identical 404 for all three (no existence
        // oracle). A hard error banner would be a dead end for a shareable
        // deep link; fall back to the topic map with a soft notice instead.
        backToOverview()
        notice = 'Block is not visible in the graph — showing the topic map.'
      } else {
        error = apiErr
      }
    } finally {
      busy = false
    }
  }

  /** Double-click expand: +1 hop around the node, focus stays put. */
  async function expand(id: string): Promise<void> {
    if (busy) return
    busy = true
    error = null
    notice = null
    try {
      touch(graph, id)
      const resp = await fetchEgo(id, { hops: 1, ...toEgoQuery(filters, loadedStructClasses) })
      mergeEgo(graph, resp, palette, categoryHues)
      truncated = resp.stats.truncated
      settle()
    } catch (err) {
      const apiErr = toApiError(err)
      if (isEgoFocusNotFound(apiErr)) {
        // Same fallback as setFocus: the expanded block vanished from the
        // visible graph mid-session (archived/scope-shifted) — back to the
        // topic map instead of a dead-end banner.
        backToOverview()
        notice = 'Block is not visible in the graph — showing the topic map.'
      } else {
        error = apiErr
      }
    } finally {
      busy = false
    }
  }

  /** Load-all: merge the whole visible corpus (server-capped, newest-first)
   *  into the live graph. Same merge/settle path as expand — the focus stays,
   *  hops recompute against it (disconnected nodes read Infinity, which is
   *  exactly the evict-farthest-first order). Camera resets: the result is a
   *  corpus view, not a neighborhood. */
  async function loadAll(): Promise<void> {
    if (busy) return
    busy = true
    error = null
    notice = null
    try {
      const resp = await fetchGraphAll(toEgoQuery(filters, loadedStructClasses))
      mergeEgo(graph, resp, palette, categoryHues)
      truncated = resp.stats.truncated
      settle()
      view?.resetCamera()
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
    notice = null
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
    <div class="stage" bind:this={stageEl}>
      <div class="viewport" bind:this={viewportEl} tabindex="-1">
        {#if RendererComp !== null}
          <!-- RV1: {#key} remountet beim Switch — jeder Renderer baut seinen
               eigenen Canvas/GL-Kontext auf und räumt ihn im onMount-Teardown
               wieder ab; ein Props-morphender Übergang zwischen GL-Stacks
               existiert nicht. -->
          {#key rendererId}
            <RendererComp
              bind:this={view}
              {graph}
              {filters}
              {palette}
              {showLabels}
              {graphRev}
              {threeD}
              highlightedIds={store.openIds}
              topId={store.topId}
              onnodeclick={(id) => store.open(id, null)}
              onnodedoubleclick={(id) => void expand(id)}
            />
          {/key}
        {/if}
      </div>
      <!-- U02 (design 04-§4.6): die Fenster-Schicht ist graph-frei; DIESE
           Kompositions-Wurzel injiziert die beiden Graph-Kopplungen —
           labelFor (Titel/Chips) und das BlockDetailContent-Snippet (Body). -->
      <WindowManager
        {store}
        keepMinimized
        labelFor={(id) => (graph.hasNode(id) ? graph.getNodeAttribute(id, 'label') : id.slice(0, 8))}
      >
        {#snippet content(win, titleId)}
          <BlockDetailContent
            id={win.id}
            {graph}
            {graphRev}
            onfocus={(id) => void setFocus(id)}
            onexpand={(id) => void expand(id)}
            {titleId}
          />
        {/snippet}
      </WindowManager>
    </div>
  {:else}
    <div class="stage">
      <OverviewMap onpick={(id) => void setFocus(id)} {palette} overrides={categoryHues} />
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

  {#if notice}
    <div class="notice" role="status">
      <p>{notice}</p>
      <button type="button" class="notice-close" aria-label="dismiss" onclick={() => (notice = null)}>×</button>
    </div>
  {/if}

  <!-- Left overlay column: search (always mounted) + focus controls (meta-row,
       filters). pointer-events:none on the column lets the gaps pan the canvas;
       each card re-enables events on itself. -->
  <div class="chrome-left">
    <div class="card search-card">
      <SearchBox
        pageBusy={busy}
        onpick={(id, origin) => void setFocus(id, { open: true, openTrigger: origin })}
      />
    </div>
    {#if focus !== null}
      <div class="card meta-row">
        <button class="back" type="button" onclick={backToOverview}>← map</button>
        <button
          class="back"
          type="button"
          disabled={busy}
          title="load every visible block (server-capped)"
          onclick={() => void loadAll()}>load all</button
        >
        <button
          class="back"
          class:on={showLabels}
          type="button"
          disabled={!rendererCaps.labels}
          aria-pressed={showLabels}
          title={rendererCaps.labels
            ? 'node labels on the canvas — hover and open windows keep theirs'
            : 'labels: sigma renderer only'}
          onclick={() => (showLabels = !showLabels)}>labels</button
        >
        <!-- RV1: Renderer-Switch — vier Engines über derselben graphology-
             Instanz; Wahl überlebt den Seitenbesuch (localStorage). -->
        <select
          class="renderer-select"
          aria-label="graph renderer"
          title="graph renderer — switch live, same data"
          bind:value={rendererId}
        >
          {#each RENDERER_IDS as id (id)}
            <option value={id}>{RENDERERS[id].label}</option>
          {/each}
        </select>
        {#if rendererCaps.threeD && rendererId === 'deck'}
          <button
            class="back"
            class:on={threeD}
            type="button"
            aria-pressed={threeD}
            title="orbit the graph in 3D (z = semantic axis)"
            onclick={() => (threeD = !threeD)}>3D</button
          >
        {/if}
        <code class="focus" title="focused block">{focus}</code>
        {#if busy}
          <span class="loading" aria-busy="true">loading…</span>
        {/if}
        <span class="stats">
          <!-- GC3: structural count surfaces without a click; class-neutral
               wording ("structural", not "refs") — the count spans ALL
               registry classes (design 03-§4.4). -->
          {nodeCount} nodes · {edgeCount} edges{structCount > 0 ? ` · ${structCount} structural` : ''}{truncated
            ? ' · server truncated'
            : ''}
        </span>
      </div>
      <FilterPanel {filters} categories={loadedCategories} structClasses={loadedStructClasses} onchange={(next) => (filters = next)} />
    {/if}
  </div>

  {#if focus !== null}
    <p class="hint">
      click a node for details · double-click to expand (+1 hop) · “· +N” = links not loaded yet · curved arrow =
      reference
    </p>
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
    font-size: var(--fs-sm);
  }
  .error p {
    margin: 0;
    color: var(--danger);
  }
  /* Soft fallback notice (fix C): same overlay slot as .error, but neutral
     info tone — a focus 404 is not a failure of the graph, just a block the
     map cannot open. Dismissible; also cleared by any next focus/expand. */
  .notice {
    position: absolute;
    top: var(--space-3);
    left: 50%;
    transform: translateX(-50%);
    z-index: var(--z-overlay);
    max-width: min(40rem, calc(100% - 2 * var(--space-3)));
    display: flex;
    align-items: center;
    gap: var(--space-3);
    border: 1px solid var(--accent);
    border-radius: var(--radius);
    background: var(--surface-1);
    padding: var(--space-2) var(--space-3);
    font-size: var(--fs-sm);
  }
  .notice p {
    margin: 0;
    color: var(--text);
  }
  .notice-close {
    border: none;
    background: none;
    color: var(--text-dim);
    font-size: var(--fs-lg);
    line-height: var(--lh-solid);
    cursor: pointer;
    padding: 0 var(--space-1);
  }
  .notice-close:hover {
    color: var(--text);
  }
  .request-id {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    color: var(--text-dim) !important;
  }

  .meta-row {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    min-height: 1.4rem;
  }
  .focus {
    font-size: var(--fs-xs);
    /* Ellipsize the UUID visually — the DOM keeps the full id (copyable via
       select-all/title, asserted by the GC4 spec), the row stays compact. */
    max-width: 9rem;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .back {
    border: 1px solid var(--border);
    border-radius: var(--radius);
    background: var(--surface-2);
    color: var(--text);
    cursor: pointer;
    font-size: var(--fs-xs);
    padding: 0.15rem 0.55rem;
  }
  .back:hover {
    border-color: var(--text-dim);
  }
  .back:disabled {
    cursor: default;
    opacity: 0.5;
  }
  /* Pressed state of the labels toggle — same accent grammar as the Pin
     button (BlockDetailContent) and the filtered MultiSelect trigger. */
  .back.on {
    border-color: var(--accent);
    color: var(--accent);
  }
  /* RV1: Renderer-Select — gleiche Formensprache wie die .back-Buttons. */
  .renderer-select {
    border: 1px solid var(--border);
    border-radius: var(--radius);
    background: var(--surface-2);
    color: var(--text);
    cursor: pointer;
    font-size: var(--fs-xs);
    padding: 0.15rem 0.35rem;
  }
  .renderer-select:hover {
    border-color: var(--text-dim);
  }
  .loading {
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
  }
  .stats {
    margin-left: auto;
    color: var(--text-dim);
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
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
    font-size: var(--fs-sm);
    text-align: center;
    pointer-events: none;
  }
</style>
