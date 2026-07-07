<script lang="ts">
  import { onMount } from 'svelte'
  import type { Snippet } from 'svelte'
  import { SM, onBelow } from '../../lib/layout/breakpoints'
  import type { WindowStore, WinState } from '../../lib/windows/windows.svelte'
  import FloatingWindow from './FloatingWindow.svelte'

  // The overlay layer (design 07-§D): position:absolute; inset:0 over the graph
  // stage. It is BOTH the positioned containing-block for every .window AND the
  // measured surface (store.setSurface) — same element → Surface == Containing-
  // Block, no clamp/placement drift. pointer-events:none makes the gaps between
  // windows click-through to the Sigma canvas (node-click + camera-pan survive);
  // the windows + minbar re-enable events on themselves.
  //
  // Graph-entkoppelt (U02, design 04-§4.6): statt einer DirectedGraph-Prop
  // injiziert der Host (a) `labelFor(id)` für Titel-Leiste + Minbar-Chips und
  // (b) das `content`-Snippet, das FloatingWindow im Body rendert. Der Graph
  // reicht BlockDetailContent samt graph durch; ein Board reicht später einen
  // IssueDetail-Renderer — dieselbe Fenster-Schicht, kein Graph-Wissen hier.
  // Keep-Mounted-Opt-in pro Host (U04-W3, design 04-§4.3): mit keepMinimized
  // behält der Host minimierte Fenster GEMOUNTET (FloatingWindow blendet sie per
  // display:none aus) statt sie aus dem keyed each zu werfen — Scroll/Content
  // überleben Minimize/Restore ohne Remount. Graph-Host = true (BlockDetailContent
  // hat KEINEN Live-Loader im Body). Board-Host = false (IssueDetailContent hält
  // eine LiveSource — SSE + 10s-Poll je Instanz; ein per display:none gemountet
  // gehaltenes minimiertes Board-Fenster hielte je eine langlebige SSE-Verbindung.
  // Destroy folgt dort der Sichtbarkeit, live.stop() schließt die Verbindung).
  let {
    store,
    labelFor,
    content,
    keepMinimized = false,
  }: {
    store: WindowStore
    /** Chip-/Titel-Label-Auflösung — ersetzt die frühere DirectedGraph-Prop. */
    labelFor: (id: string) => string
    /** Fenster-Inhalt; erhält (win, titleId) für aria-labelledby-Verdrahtung. */
    content: Snippet<[WinState, string]>
    /** Host-Opt-in: minimierte Fenster gemountet halten (display:none) statt
     * zerstören. Default false (Ist-destroy). Siehe Host-Doku oben. */
    keepMinimized?: boolean
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
</script>

<div class="wm-root" bind:clientWidth={rootW} bind:clientHeight={rootH}>
  {#if mobile}
    {#if topWin}
      <FloatingWindow win={topWin} {store} {content} sheet />
    {/if}
    {#if mobileChips.length > 0}
      <div class="minbar" aria-label="minimized windows">
        {#each mobileChips as w (w.id)}
          <button class="chip" type="button" onclick={() => store.restore(w.id)}>{labelFor(w.id)}</button>
        {/each}
      </div>
    {/if}
  {:else}
    <!-- keepMinimized: ALLE Fenster rendern (minimierte werden per display:none
         versteckt); sonst nur die offenen (Ist-destroy). openWins bleibt für den
         Board-Pfad in Gebrauch, minimizedWins treibt weiter die Chips. -->
    {#each (keepMinimized ? store.wins : openWins) as w (w.id)}
      <FloatingWindow win={w} {store} {content} />
    {/each}
    {#if minimizedWins.length > 0}
      <div class="minbar" aria-label="minimized windows">
        {#each minimizedWins as w (w.id)}
          <button class="chip" type="button" onclick={() => store.restore(w.id)}>{labelFor(w.id)}</button>
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
    font-size: var(--fs-xs);
    padding: var(--space-1) var(--space-2);
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .chip:hover {
    border-color: var(--text-dim);
  }
</style>
