<script lang="ts">
  // Session list (design 06 §3.9): updated_at DESC (server-ordered), the open
  // one highlighted, a "new chat" action, and per-row delete with a confirm.
  // No bits-ui in the project — window.confirm is the dependency-free guard.
  import type { ChatStore } from '../../lib/chat/store.svelte'

  let { store }: { store: ChatStore } = $props()

  function del(id: string, title: string): void {
    if (confirm(`Delete "${title}"? This removes the session and its messages.`)) {
      void store.deleteSession(id)
    }
  }
</script>

<aside class="sidebar">
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

<style>
  .sidebar {
    width: 15rem;
    flex: none;
    border-right: 1px solid var(--border);
    background: var(--surface-1);
    display: flex;
    flex-direction: column;
    overflow-y: auto;
  }
  .new {
    margin: var(--space-2);
    font-family: var(--font-mono);
    font-size: 0.85rem;
  }
  .hint {
    padding: var(--space-3);
    color: var(--text-faint);
    font-size: 0.85rem;
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
    font-size: 0.85rem;
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
    font-size: 1.1rem;
  }
  .del:hover {
    color: var(--danger);
  }
</style>
