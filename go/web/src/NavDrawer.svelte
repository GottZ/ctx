<script lang="ts">
  // Off-canvas navigation for narrow viewports (< sm, design 01-shell-layout §3
  // / §4 S3). Renders a slim mobile top-bar with a hamburger; toggling it slides
  // the rail in from the left over a backdrop. The panel reuses <NavRail/> (forced
  // expanded) so there is one nav source of truth — this component owns only the
  // mobile chrome: the trigger, the backdrop, the focus trap and Escape/close.
  //
  // a11y (§3 contract): the hamburger carries aria-expanded (drawer state) +
  // aria-controls (panel id); the open panel is a labelled aria-modal dialog with
  // a Tab focus-trap, Escape closes, and focus returns to the hamburger on close.
  // Selecting a nav item closes the drawer (onNavigate) so the SPA navigation
  // lands on a clean full-screen page.
  import { tick } from 'svelte'
  import Wordmark from './Wordmark.svelte'
  import NavRail from './NavRail.svelte'

  let open = $state(false)
  let panel = $state<HTMLElement | null>(null)
  let trigger = $state<HTMLButtonElement | null>(null)

  const FOCUSABLE = 'a[href], button:not([disabled]), [tabindex]:not([tabindex="-1"])'

  async function openDrawer(): Promise<void> {
    open = true
    await tick()
    panel?.querySelector<HTMLElement>(FOCUSABLE)?.focus()
  }

  function closeDrawer(): void {
    if (!open) return
    open = false
    trigger?.focus()
  }

  function toggle(): void {
    if (open) closeDrawer()
    else void openDrawer()
  }

  // Window-level so the panel <div> carries no event handler (a11y-lint clean)
  // while Escape + the Tab focus-trap stay active only while the drawer is open.
  function onWindowKeydown(e: KeyboardEvent): void {
    if (!open) return
    if (e.key === 'Escape') {
      closeDrawer()
      return
    }
    if (e.key !== 'Tab' || panel === null) return
    const focusable = [...panel.querySelectorAll<HTMLElement>(FOCUSABLE)]
    if (focusable.length === 0) return
    const first = focusable[0]
    const last = focusable[focusable.length - 1]
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault()
      last.focus()
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault()
      first.focus()
    }
  }
</script>

<svelte:window onkeydown={onWindowKeydown} />

<header class="mobile-bar">
  <button
    class="hamburger"
    type="button"
    bind:this={trigger}
    onclick={toggle}
    aria-expanded={open}
    aria-controls="nav-drawer"
    aria-label={open ? 'Close navigation menu' : 'Open navigation menu'}
  >
    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      stroke-width="1.8"
      stroke-linecap="round"
      aria-hidden="true"
    >
      {#if open}
        <path d="M6 6l12 12M18 6L6 18" />
      {:else}
        <path d="M4 7h16M4 12h16M4 17h16" />
      {/if}
    </svg>
  </button>
  <a class="home" href="/status"><Wordmark /></a>
</header>

{#if open}
  <button class="backdrop" type="button" aria-label="Close navigation menu" onclick={closeDrawer}
  ></button>
  <div
    class="drawer-panel"
    id="nav-drawer"
    role="dialog"
    aria-modal="true"
    aria-label="Navigation"
    bind:this={panel}
  >
    <NavRail expanded onNavigate={closeDrawer} />
  </div>
{/if}

<style>
  .mobile-bar {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    height: 3rem;
    padding: 0 var(--space-2);
    background: var(--surface-1);
    border-bottom: 1px solid var(--border);
  }
  .hamburger {
    display: flex;
    align-items: center;
    justify-content: center;
    padding: var(--space-1);
    background: transparent;
    border: 1px solid transparent;
  }
  .hamburger svg {
    width: 1.4rem;
    height: 1.4rem;
  }
  .home {
    display: flex;
    align-items: center;
    text-decoration: none;
    font-size: 1.05rem;
  }

  .backdrop {
    position: fixed;
    inset: 0;
    width: 100%;
    padding: 0;
    border: none;
    border-radius: 0;
    background: var(--backdrop); /* Q3: 0.5 → 0.55 konsolidiert (design 05-§3.3) */
    cursor: default;
    z-index: var(--z-drawer);
  }
  .drawer-panel {
    position: fixed;
    inset-block: 0;
    inset-inline-start: 0;
    width: min(var(--rail-w), 80vw);
    z-index: calc(var(--z-drawer) + 1);
  }
</style>
