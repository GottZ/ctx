<script lang="ts">
  // Session list (design 06 §3.9): updated_at DESC (server-ordered), the open
  // one highlighted, a "new chat" action, and per-row delete with a confirm.
  // No bits-ui in the project — window.confirm is the dependency-free guard.
  import type { ChatStore } from '../../lib/chat/store.svelte'

  let { store }: { store: ChatStore } = $props()

  // < sm drawer state (design 01-shell-layout §6). Presentational only — the
  // session/stream store is never touched here. The list is an inline column
  // ≥ sm; below sm it becomes an off-canvas drawer behind a toggle.
  let open = $state(false)

  // Opening / creating / deleting a session dismisses the drawer so the freshly
  // selected conversation is visible. Reads currentId (the store sets it on
  // those actions); never writes store state.
  $effect(() => {
    void store.currentId
    open = false
  })

  function del(id: string, title: string): void {
    if (confirm(`Delete "${title}"? This removes the session and its messages.`)) {
      void store.deleteSession(id)
    }
  }
</script>

<button
  class="drawer-toggle"
  type="button"
  aria-label="Sessions"
  aria-expanded={open}
  aria-controls="session-list"
  onclick={() => (open = !open)}>☰</button>

<aside id="session-list" class="sidebar" class:open>
  <button class="new" type="button" onclick={() => store.newSession()} disabled={store.streaming}>+ New chat</button>

  {#if store.loadingList && store.sessions.length === 0}
    <p class="hint" aria-busy="true">loading…</p>
  {:else if store.sessions.length === 0}
    <p class="hint">No sessions yet.</p>
  {:else}
    <ul>
      {#each store.sessions as s (s.id)}
        <li class:active={s.id === store.currentId}>
          <button class="row" type="button" onclick={() => store.selectSession(s.id)} disabled={store.streaming}>
            <span class="title">{s.title}</span>
            <span class="meta">
              {s.message_count} msg{s.message_count === 1 ? '' : 's'}
              {#if s.max_sensitivity === 'credentials'}<span class="lock" title="contains credentials content — full-trust backends only">🔒</span>{/if}
            </span>
          </button>
          <button class="del" type="button" title="delete" aria-label="delete session" onclick={() => del(s.id, s.title)} disabled={store.streaming}>×</button>
        </li>
      {/each}
    </ul>
  {/if}
</aside>

{#if open}
  <button class="scrim" type="button" aria-label="Close sessions" onclick={() => (open = false)}></button>
{/if}

<style>
  .sidebar {
    width: var(--session-w);
    flex: none;
    border-right: 1px solid var(--border);
    background: var(--surface-1);
    display: flex;
    flex-direction: column;
    overflow-y: auto;
  }

  /* Toggle + scrim are inert ≥ sm; the < sm container query (querying ChatPage's
     .chat region container, design 01 §6) turns the list into an off-canvas
     drawer behind the toggle. */
  .drawer-toggle,
  .scrim {
    display: none;
  }
  .new {
    margin: var(--space-2);
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
  }
  .hint {
    padding: var(--space-3);
    color: var(--text-faint);
    font-size: var(--fs-sm);
  }
  ul {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  li {
    display: flex;
    align-items: stretch;
    border-bottom: 1px solid var(--surface-2);
  }
  li.active {
    background: var(--surface-2);
  }
  .row {
    flex: 1;
    min-width: 0;
    text-align: left;
    background: none;
    border: none;
    padding: var(--space-2);
    cursor: pointer;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .title {
    color: var(--text);
    font-size: var(--fs-sm);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .meta {
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--label-size);
  }
  .del {
    border: none;
    background: none;
    color: var(--text-faint);
    cursor: pointer;
    padding: 0 var(--space-2);
    font-size: var(--fs-base);
  }
  .del:hover {
    color: var(--danger);
  }

  /* < sm: the region is too narrow for a fixed 15rem column beside the thread, so
     the list becomes a slide-in drawer overlaying the conversation (design 01
     §6). 40rem == breakpoints.ts SM, matching BlocksPage's split-stack query. */
  @container (max-width: 40rem) {
    .drawer-toggle {
      display: inline-flex;
      align-items: center;
      justify-content: center;
      position: absolute;
      top: var(--space-2);
      /* top-right: clear of the left-sliding drawer, so it stays the visible
         open/close affordance without overlapping the list's "+ New chat". */
      right: var(--space-2);
      z-index: calc(var(--z-drawer) + 2);
      padding: var(--space-1) var(--space-2);
      font-family: var(--font-mono);
      line-height: var(--lh-solid);
    }
    .sidebar {
      position: absolute;
      inset-block: 0;
      left: 0;
      width: min(var(--session-w), 80%);
      z-index: calc(var(--z-drawer) + 1);
      transform: translateX(-100%);
      visibility: hidden;
      transition: transform var(--dur-2) var(--ease), visibility var(--dur-2) var(--ease);
      box-shadow: var(--shadow-1); /* Q3: Ist war 2px 0 12px (Achsen-Tausch dokumentiert, design 05-§3.3) */
    }
    .sidebar.open {
      transform: none;
      visibility: visible;
    }
    .scrim {
      display: block;
      position: absolute;
      inset: 0;
      z-index: var(--z-drawer);
      padding: 0;
      border: 0;
      border-radius: 0;
      background: var(--backdrop); /* Q3: 0.45 → 0.55 konsolidiert */
      cursor: pointer;
    }
    /* the global button:hover would lighten the scrim — keep it constant */
    .scrim:hover:not(:disabled) {
      background: var(--backdrop); /* Q3: 0.45 → 0.55 konsolidiert */
    }
  }

  @media (prefers-reduced-motion: reduce) {
    .sidebar {
      transition: none;
    }
  }
</style>
