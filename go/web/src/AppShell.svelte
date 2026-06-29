<script lang="ts">
  // The authenticated app shell (design 01-shell-layout §3/§4 S3) — replaces the
  // old topbar+main `{:else}` branch of App.svelte. A 100dvh grid: nav rail (or
  // off-canvas drawer below sm) on the left, the routed content region on the
  // right. The content region's data-mode is resolved synchronously from the
  // pathname (areaMode, no lazy-navigation flash); S3 wires the attribute and
  // keeps every mode on the reading default (the 70rem cap survives on
  // --measure-wide so every area stays a coherent pause state — BlocksPage/Graph
  // still scroll). S4-S7 specialise the per-mode full-bleed/scroll chains keyed
  // on this already-wired attribute.
  //
  // Importing ./router here instantiates createRouter before <Router/> mounts
  // (the SPA does not navigate programmatically yet) and exposes the reactive
  // `route` whose pathname drives both data-mode and the rail's active state.
  import { Router } from 'sv-router'
  import { route } from './router'
  import { areaMode } from './lib/layout/modes'
  import { MD, SM, below, onBelow } from './lib/layout/breakpoints'
  import NavRail from './NavRail.svelte'
  import NavDrawer from './NavDrawer.svelte'

  const PIN_KEY = 'ctx.nav-rail'
  type Pin = 'expanded' | 'collapsed' | null

  function readPin(): Pin {
    try {
      const v = localStorage.getItem(PIN_KEY)
      return v === 'expanded' || v === 'collapsed' ? v : null
    } catch {
      return null
    }
  }

  // Seed responsive state synchronously (matchMedia is read at init, not in the
  // effect) so the first paint already matches the viewport — no desktop-rail
  // flash on a phone. The effect only subscribes to later crossings.
  let pin = $state<Pin>(readPin())
  let narrow = $state(below(MD)) // < md → auto icon-rail
  let mobile = $state(below(SM)) // < sm → drawer

  $effect(() => {
    const offMd = onBelow(MD, (b) => (narrow = b))
    const offSm = onBelow(SM, (b) => (mobile = b))
    return () => {
      offMd()
      offSm()
    }
  })

  // Expanded labelled rail: never on mobile (the drawer takes over); otherwise
  // honour an explicit pin, else auto-collapse below md (design 01 §7.1).
  const expanded = $derived(!mobile && (pin === null ? !narrow : pin === 'expanded'))

  function toggleRail(): void {
    pin = expanded ? 'collapsed' : 'expanded'
    try {
      localStorage.setItem(PIN_KEY, pin)
    } catch {
      /* storage unavailable — pin is in-memory only this session */
    }
  }

  const mode = $derived(areaMode(route.pathname))
  const railWidth = $derived(expanded ? 'var(--rail-w)' : 'var(--rail-w-icon)')
</script>

<div
  class="shell"
  style="--rail-w-cur: {railWidth}; grid-template-columns: {mobile
    ? '1fr'
    : 'var(--rail-w-cur) 1fr'}; grid-template-rows: {mobile ? 'auto 1fr' : '1fr'};"
>
  {#if mobile}
    <NavDrawer />
  {:else}
    <NavRail {expanded} onToggle={toggleRail} />
  {/if}

  <main class="content" data-mode={mode}>
    <Router />
  </main>
</div>

<style>
  .shell {
    display: grid;
    height: 100dvh;
  }

  .content {
    min-width: 0;
    min-height: 0;
    box-sizing: border-box;
    /* Every mode scrolls — nothing clips (design 01 §4 S4). The contract's
       sketch caps canvas/split/thread with overflow:hidden + height:100%, but
       that is deferred to S5/S6/S7: GraphPage/BlocksPage/ChatPage still lack the
       inner min-height:0 scroll chains, so clamping their height now would cut
       off the canvas / hit-list / thread. Until those waves land the whole
       region scrolls — full width, fully browsable, no abscission. */
    overflow-y: auto;
  }

  /* reading (Settings/Status): a single column capped on the wide reading
     measure (the old 70rem blanket cap, now scoped to this one mode) and
     padded. canvas/split/thread get neither the cap nor the padding → they run
     full-bleed across the whole content region (design 01 §4 S4,
     Content-Region-Contract). */
  .content[data-mode='reading'] {
    padding: var(--space-4) var(--shell-gutter);
  }

  .content[data-mode='reading'] > :global(.area) {
    max-width: var(--measure-wide);
    margin-inline: auto;
  }
</style>
