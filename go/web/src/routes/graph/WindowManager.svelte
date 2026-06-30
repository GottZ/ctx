<script lang="ts">
  import { onMount } from 'svelte'
  import type { DirectedGraph } from 'graphology'
  import { type EdgeAttrs, type NodeAttrs } from '../../lib/graph/graph-client'
  import { SM, onBelow } from '../../lib/layout/breakpoints'
  import type { WindowStore } from '../../lib/graph/windows.svelte'
  import FloatingWindow from './FloatingWindow.svelte'

  // The overlay layer (design 07-§D): position:absolute; inset:0 over the graph
  // stage. It is BOTH the positioned containing-block for every .window AND the
  // measured surface (store.setSurface) — same element → Surface == Containing-
  // Block, no clamp/placement drift. pointer-events:none makes the gaps between
  // windows click-through to the Sigma canvas (node-click + camera-pan survive);
  // the windows + minbar re-enable events on themselves.
  let {
    graph,
    store,
    onfocus,
    onexpand,
  }: {
    graph: DirectedGraph<NodeAttrs, EdgeAttrs>
    store: WindowStore
    onfocus: (id: string) => void
    onexpand: (id: string) => void
  } = $props()

  // Reactive surface measurement of THIS overlay root (= containing block).
  let rootW = $state(0)
  let rootH = $state(0)
  let mobile = $state(false)

  // Single surface source: push the measured root extent into the store. Guard
  // on a real measurement so the safe store default ({1280,720}) is never
  // clobbered by the pre-bind 0×0 frame.
  $effect(() => {
    if (rootW > 0 && rootH > 0) store.setSurface({ wLu: rootW, hLu: rootH })
  })

  // Desktop↔Mobile switch via the shared breakpoint (consistent with AppShell).
  onMount(() => onBelow(SM, (b) => (mobile = b)))

  const openWins = $derived(store.wins.filter((w) => !w.minimized))
  const minimizedWins = $derived(store.wins.filter((w) => w.minimized))
  // Mobile: the top window is the full-bleed sheet; everything else is a chip.
  const topWin = $derived(store.wins.find((w) => w.id === store.topId) ?? null)
  const mobileChips = $derived(store.wins.filter((w) => w.id !== store.topId))

  function chipLabel(id: string): string {
    return graph.hasNode(id) ? graph.getNodeAttribute(id, 'label') : id.slice(0, 8)
  }
</script>

<div class="wm-root" bind:clientWidth={rootW} bind:clientHeight={rootH}>
  {#if mobile}
    {#if topWin}
      <FloatingWindow win={topWin} {store} {graph} {onfocus} {onexpand} sheet />
    {/if}
    {#if mobileChips.length > 0}
      <div class="minbar" aria-label="minimized windows">
        {#each mobileChips as w (w.id)}
          <button class="chip" type="button" onclick={() => store.restore(w.id)}>{chipLabel(w.id)}</button>
        {/each}
      </div>
    {/if}
  {:else}
    {#each openWins as w (w.id)}
      <FloatingWindow win={w} {store} {graph} {onfocus} {onexpand} />
    {/each}
    {#if minimizedWins.length > 0}
      <div class="minbar" aria-label="minimized windows">
        {#each minimizedWins as w (w.id)}
          <button class="chip" type="button" onclick={() => store.restore(w.id)}>{chipLabel(w.id)}</button>
        {/each}
      </div>
    {/if}
  {/if}
</div>

<style>
  .wm-root {
    position: absolute; /* aus dem Flex-Fluss von .stage → stoert .viewport{flex:1} NICHT */
    inset: 0; /* fuellt die Stage → positionierter Containing-Block fuer jedes .window */
    pointer-events: none; /* Luecken zwischen den Fenstern click-through → Sigma-Canvas */
    /* KEIN eigenes z-index: die per-Window-Inline-z (calc(--z-window + z))
       ordnen die Fenster ueber chrome-left (--z-overlay). */
  }

  /* Minimieren-Leiste: re-enabled Events auf sich selbst (wie chrome-left-Cards). */
  .minbar {
    position: absolute;
    left: var(--space-3);
    bottom: var(--space-3);
    z-index: var(--z-window);
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
    max-width: calc(100% - 2 * var(--space-3));
    pointer-events: auto;
  }
  .chip {
    max-width: 12rem;
    overflow: hidden;
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    background: var(--surface-2);
    color: var(--text);
    cursor: pointer;
    font-size: 0.78rem;
    padding: var(--space-1) var(--space-2);
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .chip:hover {
    border-color: var(--text-dim);
  }
</style>
