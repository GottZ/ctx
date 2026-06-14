<script lang="ts">
  import { onMount } from 'svelte'
  import { Router, isActiveLink } from 'sv-router'
  import Login from './Login.svelte'
  import Wordmark from './Wordmark.svelte'
  import { session } from './lib/auth.svelte'
  // Importing the router module instantiates createRouter before <Router/>
  // mounts; the SPA shell does not navigate programmatically yet.
  import './router'

  const areas = [
    { href: '/settings', label: 'Settings' },
    { href: '/status', label: 'Status' },
    { href: '/graph', label: 'Graph' },
    { href: '/blocks', label: 'Blocks' },
    { href: '/chat', label: 'Chat' },
  ]

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
  <div class="shell">
    <header class="topbar">
      <a class="home" href="/status"><Wordmark /></a>
      <nav aria-label="Areas">
        {#each areas as area (area.href)}
          <a href={area.href} {@attach isActiveLink({ startsWith: true })}>{area.label}</a>
        {/each}
      </nav>
      <div class="identity">
        {#if session.label}
          <span class="key-label" title="API key label">{session.label}</span>
        {/if}
        {#if !session.admin}
          <span class="badge" title="This key has no admin tier — mutating areas render read-only.">
            read-only
          </span>
        {/if}
        <button class="logout" type="button" onclick={() => session.logout()}>Log out</button>
      </div>
    </header>
    <main>
      <Router />
    </main>
  </div>
{/if}

<style>
  .boot {
    min-height: 100vh;
    display: grid;
    place-content: center;
    justify-items: center;
    gap: var(--space-2);
    font-size: 1.25rem;
  }
  .boot p {
    margin: 0;
    color: var(--text-faint);
    font-size: 0.85rem;
    font-family: var(--font-mono);
  }

  .shell {
    min-height: 100vh;
    display: flex;
    flex-direction: column;
  }

  .topbar {
    display: flex;
    align-items: center;
    gap: var(--space-4);
    padding: 0 var(--space-3);
    height: 3rem;
    background: var(--surface-1);
    border-bottom: 1px solid var(--border);
  }

  .home {
    text-decoration: none;
    font-size: 1.05rem;
  }

  nav {
    display: flex;
    gap: var(--space-1);
    align-self: stretch;
  }
  nav a {
    display: flex;
    align-items: center;
    padding: 0 var(--space-3);
    color: var(--text-dim);
    text-decoration: none;
    font-family: var(--font-mono);
    font-size: 0.85rem;
    border-bottom: 1px solid transparent;
    margin-bottom: -1px; /* sit on the topbar border */
    transition: color 120ms ease;
  }
  nav a:hover {
    color: var(--text);
  }
  nav a:global(.is-active) {
    color: var(--accent);
    border-bottom-color: var(--accent);
  }

  .identity {
    margin-left: auto;
    display: flex;
    align-items: center;
    gap: var(--space-2);
    min-width: 0;
  }
  .key-label {
    font-family: var(--font-mono);
    font-size: 0.8rem;
    color: var(--text-dim);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .badge {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--warn);
    border: 1px solid var(--warn);
    border-radius: var(--radius);
    padding: 0.05rem var(--space-2);
    white-space: nowrap;
  }
  .logout {
    font-size: 0.8rem;
    padding: var(--space-1) var(--space-2);
  }

  main {
    flex: 1;
    width: min(70rem, 100%);
    margin: 0 auto;
    padding: var(--space-4) var(--space-3);
    box-sizing: border-box;
  }
</style>
