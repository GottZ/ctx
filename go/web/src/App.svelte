<script lang="ts">
  import { onMount } from 'svelte'
  import Login from './Login.svelte'
  import Wordmark from './Wordmark.svelte'
  import AppShell from './AppShell.svelte'
  import { session } from './lib/auth.svelte'
  // AppShell statically imports ./router, instantiating createRouter before the
  // <Router/> inside it mounts; the SPA shell does not navigate programmatically.

  onMount(() => {
    void session.restore()
  })
</script>

{#if session.restoring}
  <div class="boot" aria-busy="true">
    <Wordmark />
    <p>restoring session…</p>
  </div>
{:else if !session.active}
  <Login />
{:else}
  <AppShell />
{/if}

<style>
  /* Boot splash only — the authenticated shell's chrome lives in AppShell. */
  .boot {
    min-height: 100dvh;
    display: grid;
    place-content: center;
    justify-items: center;
    gap: var(--space-2);
    font-size: var(--fs-xl);
  }
  .boot p {
    margin: 0;
    color: var(--text-faint);
    font-size: var(--fs-sm);
    font-family: var(--font-mono);
  }
</style>
