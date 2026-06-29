<script lang="ts">
  import type { DirectedGraph } from 'graphology'
  import { type EdgeAttrs, type NodeAttrs } from '../../lib/graph/graph-client'
  import BlockDetailContent from './BlockDetailContent.svelte'

  // Thin chrome wrapper (design 03-§3): the <aside> shell + close button live
  // here, the lazy block viewer is BlockDetailContent. Behaviour and render are
  // unchanged from the pre-extraction sidebar — the close button is injected
  // into the content header via the {headerActions} snippet so the header
  // layout (title + close, flex row) stays identical.
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
</script>

<aside class="sidebar" aria-label="block details">
  <BlockDetailContent {id} {graph} {onfocus} {onexpand}>
    {#snippet headerActions()}
      <button class="close" type="button" title="close" onclick={onclose}>×</button>
    {/snippet}
  </BlockDetailContent>
</aside>

<style>
  .sidebar {
    width: 22rem;
    max-width: 40vw;
    border: 1px solid var(--border);
    border-radius: var(--radius);
    background: var(--surface-1);
    padding: var(--space-3);
    overflow-y: auto;
  }

  .close {
    padding: 0 var(--space-2);
    font-size: 1rem;
    line-height: 1.4;
  }
</style>
