<script lang="ts">
  // Web-chat route (design 06 §F6-C5). Owns the ChatStore, loads the session
  // list on mount, and lays out sidebar + thread + composer. The turn is
  // aborted on unmount / page-hide so navigating away frees the llama.cpp slot
  // (the abort is final — no resume, §3.9).
  import { onMount } from 'svelte'
  import { session } from '../../lib/auth.svelte'
  import { ChatStore } from '../../lib/chat/store.svelte'
  import ChatInput from './ChatInput.svelte'
  import MessageList from './MessageList.svelte'
  import SessionSidebar from './SessionSidebar.svelte'

  const store = new ChatStore(() => session.key)

  onMount(() => {
    void store.loadSessions()
    const onHide = (): void => store.abort()
    window.addEventListener('beforeunload', onHide)
    return () => {
      window.removeEventListener('beforeunload', onHide)
      store.abort()
    }
  })
</script>

<div class="chat">
  <SessionSidebar {store} />
  <section class="conversation">
    {#if store.loadError}
      <div class="load-error" role="alert">
        {store.loadError.message}
        <button type="button" onclick={() => store.loadSessions()}>Retry</button>
      </div>
    {/if}
    <MessageList {store} />
    <ChatInput {store} />
  </section>
</div>

<style>
  .chat {
    display: flex;
    /* S3 stopgap (design 01-shell-layout §6): the topbar is gone, so the phantom
       -3rem is removed and 100vh → 100dvh. Fills the content region minus its
       own --space-4 vertical padding; S7 replaces this with height:100%;min-height:0
       once the thread mode owns a definite-height region. */
    height: calc(100dvh - 2 * var(--space-4));
    min-height: 24rem;
    border: 1px solid var(--border);
    border-radius: var(--radius);
    overflow: hidden;
  }
  .conversation {
    flex: 1;
    display: flex;
    flex-direction: column;
    min-width: 0;
  }
  .load-error {
    margin: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--danger-dim);
    color: var(--danger);
    border-radius: var(--radius);
    font-size: 0.85rem;
    display: flex;
    gap: var(--space-2);
    align-items: center;
  }
</style>
