<script lang="ts">
  import type { Snippet } from 'svelte'
  import type { MultiDirectedGraph } from 'graphology'
  import { toApiError, type ApiError } from '../../lib/api'
  import { getBlock, type BlockDetail } from '../../lib/graph/api'
  import { remainingDegree, type EdgeAttrs, type NodeAttrs } from '../../lib/graph/graph-client'

  // Reusable scope-gated block viewer (design 03-§3/§5d). Hosts (DetailSidebar
  // G4, FloatingWindow G5, Blocks master-detail) supply the chrome and any
  // header-side actions via the {headerActions} snippet; this component owns the
  // block identity (title), the lazy content load and the intrinsic node
  // actions (Focus/Expand/Pin).
  let {
    id,
    graph,
    onfocus,
    onexpand,
    headerActions,
    titleId,
  }: {
    id: string
    graph: MultiDirectedGraph<NodeAttrs, EdgeAttrs>
    onfocus: (id: string) => void
    onexpand: (id: string) => void
    /** Host-supplied chrome actions rendered in the header (e.g. close/minimize). */
    headerActions?: Snippet
    /** Host-supplied id for the <h2> so a window can wire aria-labelledby (G5). */
    titleId?: string
  } = $props()

  // Metadata straight from the loaded attrs; full content arrives lazily
  // through the existing scope-checked manage-get (design 05-§3.1) — the
  // graph payload itself never carries content.
  const attrs = $derived(graph.hasNode(id) ? graph.getNodeAttributes(id) : null)
  const badge = $derived(attrs === null ? null : remainingDegree(attrs))

  let pinned = $state(false)
  let detail = $state<BlockDetail | null>(null)
  let error = $state<ApiError | null>(null)
  let loading = $state(false)

  // Effect 1: nur der attrs→pinned-Sync. Getrennt vom Load, damit eine
  // Invalidierung (WinState-.map bei focus/move/resize/restore) diesen billigen
  // Sync auslösen darf, ohne den Content-Load anzustoßen.
  $effect(() => {
    pinned = attrs?.pinned ?? false
  })

  // Effect 2 (Load): idempotent gegenüber Invalidierungen mit wertgleicher id
  // (design 04-§4.1/W1). `requestedId` ist bewusst ein plain let, KEIN $state —
  // sonst würde das Schreiben in requestedId den Effect selbst re-tracken. Bei
  // gleicher id kehrt der Effect vor detail=null um → <pre.content> bleibt im DOM,
  // scrollTop bleibt, kein Refetch, kein Lade-Flackern. Nur ein echter id-Wechsel
  // (anderer Node = eigenes Fenster = eigener Mount) lädt frisch.
  let requestedId: string | null = null
  $effect(() => {
    if (id === requestedId) return
    requestedId = id
    const rid = id // Race-Anker für Spät-Antworten nach einem id-Wechsel
    detail = null
    error = null
    loading = true
    void getBlock(rid)
      .then((res) => {
        if (rid === requestedId) detail = res.block
      })
      .catch((err) => {
        if (rid === requestedId) error = toApiError(err)
      })
      .finally(() => {
        if (rid === requestedId) loading = false
      })
  })

  function togglePin(): void {
    if (!graph.hasNode(id)) return
    pinned = !pinned
    graph.setNodeAttribute(id, 'pinned', pinned) // eviction exemption (W3)
  }
</script>

<!-- U04-W5 (design 04-§4.5): header + Meta tragen data-window-drag → in einem
     FloatingWindow-Host ziehen diese Kopf-Freiflächen das Fenster (Drag-Region-
     Vertrag). data-window-drag-exempt nimmt die kopier-relevante UUID und die
     Tags wieder aus (selektierbar/scrollbar). In Nicht-Fenster-Hosts (Sidebar
     G4, Master-Detail) ist data-window-drag inert — kein FloatingWindow-Listener. -->
<div class="detail-body">
  <header data-window-drag>
    <h2 id={titleId}>{detail?.title ?? attrs?.label ?? id}</h2>
    {#if headerActions}
      {@render headerActions()}
    {/if}
  </header>

  {#if attrs}
    <dl class="meta" data-window-drag>
      <dt>id</dt>
      <dd data-window-drag-exempt><code>{id}</code></dd>
      <dt>category</dt>
      <dd>{attrs.category}</dd>
      <dt>scope</dt>
      <dd>{attrs.scope}</dd>
      <dt>degree</dt>
      <dd>{attrs.degree >= 201 ? '200+' : attrs.degree}{badge ? ` (+${badge} not loaded)` : ''}</dd>
      <dt>created</dt>
      <dd>{attrs.createdAt.slice(0, 10)}</dd>
      {#if detail}
        <dt>updated</dt>
        <dd>{detail.updated_at.slice(0, 10)}</dd>
        {#if detail.tags.length > 0}
          <dt>tags</dt>
          <dd data-window-drag-exempt>{detail.tags.join(', ')}</dd>
        {/if}
      {/if}
    </dl>
  {/if}

  <div class="actions">
    <button type="button" onclick={() => onfocus(id)}>Focus</button>
    <button type="button" onclick={() => onexpand(id)}>Expand</button>
    <button type="button" class:pinned onclick={togglePin} title="pinned nodes survive eviction">
      {pinned ? 'Unpin' : 'Pin'}
    </button>
  </div>

  {#if loading}
    <p class="state" aria-busy="true">loading content…</p>
  {:else if error}
    <p class="problem" role="alert">{error.message}</p>
  {:else if detail}
    <!-- Always a text node, never {@html} — repo convention (design 04-§3.4). -->
    <pre class="content">{detail.content}</pre>
  {/if}
</div>

<style>
  /* Self-contained vertical rhythm so the component drops into the sidebar
     (G4), a floating window (G5) or the blocks master-detail without the host
     having to supply the flex column. */
  .detail-body {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  /* U04-W5 (design 04-§4.5): header + Meta sind Drag-Regionen (data-window-drag).
     touch-action:none → der Browser nimmt den Touch als Drag statt als Scroll;
     user-select:none → gezogen statt Text selektiert; cursor:move als sichtbare
     Affordanz. In Nicht-Fenster-Hosts bleibt das kosmetisch (Marker inert). Der
     Volltext <pre> und die übrigen Flächen bleiben unmarkiert (Touch scrollt,
     Maus selektiert — Ist). */
  header,
  .meta {
    touch-action: none;
    user-select: none;
    cursor: move;
  }
  /* Exempt-Werte (kopier-relevante UUID + Tags) restaurieren Selektion/Scroll. */
  dd[data-window-drag-exempt] {
    touch-action: auto;
    user-select: text;
    cursor: auto;
  }
  header {
    display: flex;
    align-items: flex-start;
    gap: var(--space-2);
  }
  h2 {
    margin: 0;
    font-size: var(--fs-md);
    font-weight: var(--fw-semibold);
    line-height: var(--lh-heading);
    flex: 1;
  }

  .meta {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: 0.15rem var(--space-3);
    margin: 0;
    font-size: var(--fs-sm);
  }
  dt {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-faint);
    align-self: baseline;
  }
  dd {
    margin: 0;
    color: var(--text-dim);
    overflow-wrap: anywhere;
  }
  dd code {
    font-size: var(--fs-2xs);
  }

  .actions {
    display: flex;
    gap: var(--space-2);
  }
  .actions button {
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-2);
  }
  .actions button.pinned {
    border-color: var(--accent);
    color: var(--accent);
  }

  .state {
    margin: 0;
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
  }
  .problem {
    margin: 0;
    color: var(--danger);
    font-size: var(--fs-sm);
  }

  .content {
    margin: 0;
    padding: var(--space-2);
    background: var(--surface-0);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    font-size: var(--fs-xs);
    line-height: var(--lh-body);
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    /* Keep the reading line length readable no matter how wide the host gets
       (free-resizable window G5 / wide master-detail). Meta + actions above
       stay full-width; only the prose body is clamped. Token lives in
       tokens.css; 46rem fallback covers a deploy ahead of the layout token. */
    max-width: var(--measure-prose, 46rem);
  }
</style>
