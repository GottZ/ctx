<script lang="ts">
  import type { DirectedGraph } from 'graphology'
  import { toApiError, type ApiError } from '../../lib/api'
  import { getBlock, type BlockDetail } from '../../lib/graph/api'
  import { remainingDegree, type EdgeAttrs, type NodeAttrs } from '../../lib/graph/graph-client'

  let {
    id,
    graph,
    onfocus,
    onexpand,
    onclose,
  }: {
    id: string
    graph: DirectedGraph<NodeAttrs, EdgeAttrs>
    onfocus: (id: string) => void
    onexpand: (id: string) => void
    onclose: () => void
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

  $effect(() => {
    pinned = attrs?.pinned ?? false
    detail = null
    error = null
    loading = true
    void getBlock(id)
      .then((res) => (detail = res.block))
      .catch((err) => (error = toApiError(err)))
      .finally(() => (loading = false))
  })

  function togglePin(): void {
    if (!graph.hasNode(id)) return
    pinned = !pinned
    graph.setNodeAttribute(id, 'pinned', pinned) // eviction exemption (W3)
  }
</script>

<aside class="sidebar" aria-label="block details">
  <header>
    <h2>{detail?.title ?? attrs?.label ?? id}</h2>
    <button class="close" type="button" title="close" onclick={onclose}>×</button>
  </header>

  {#if attrs}
    <dl class="meta">
      <dt>id</dt>
      <dd><code>{id}</code></dd>
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
          <dd>{detail.tags.join(', ')}</dd>
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
</aside>

<style>
  .sidebar {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    width: 22rem;
    max-width: 40vw;
    border: 1px solid var(--border);
    border-radius: var(--radius);
    background: var(--surface-1);
    padding: var(--space-3);
    overflow-y: auto;
  }

  header {
    display: flex;
    align-items: flex-start;
    gap: var(--space-2);
  }
  h2 {
    margin: 0;
    font-size: 0.95rem;
    font-weight: 600;
    line-height: 1.35;
    flex: 1;
  }
  .close {
    padding: 0 var(--space-2);
    font-size: 1rem;
    line-height: 1.4;
  }

  .meta {
    display: grid;
    grid-template-columns: auto 1fr;
    gap: 0.15rem var(--space-3);
    margin: 0;
    font-size: 0.8rem;
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
    font-size: 0.7rem;
  }

  .actions {
    display: flex;
    gap: var(--space-2);
  }
  .actions button {
    font-size: 0.8rem;
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
    font-size: 0.8rem;
  }
  .problem {
    margin: 0;
    color: var(--danger);
    font-size: 0.8rem;
  }

  .content {
    margin: 0;
    padding: var(--space-2);
    background: var(--surface-0);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    font-size: 0.78rem;
    line-height: 1.5;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
</style>
