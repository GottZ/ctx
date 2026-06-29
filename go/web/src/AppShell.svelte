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
    overflow-y: auto;
    padding: var(--space-4) var(--shell-gutter);
    box-sizing: border-box;
  }

  /* S3 pause state: reading-default carries every area (design 01 §4 S3 — "alle
     Modi rendern reading-default"). The 70rem blanket cap survives on
     --measure-wide; data-mode is already wired (above) for the S4-S7 per-mode
     full-bleed/scroll-chain work — applying overflow:hidden+height:100% to
     split/canvas/thread now would clip the un-touched BlocksPage list (the break
     §6 defers to S6). ChatPage owns its own dvh height (S3 stopgap). */
  .content > :global(.area) {
    max-width: var(--measure-wide);
    margin-inline: auto;
  }
</style>
