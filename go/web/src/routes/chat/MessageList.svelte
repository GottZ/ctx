<script lang="ts">
  // The message thread: persisted history (via buildRenderItems) plus the live
  // turn below it while streaming. Auto-scrolls to the newest content unless the
  // user has scrolled up (scroll-lock + a "jump to latest" affordance).
  import { buildRenderItems } from '../../lib/chat/render'
  import type { ChatStore } from '../../lib/chat/store.svelte'
  import BackendBadge from './BackendBadge.svelte'
  import ConfirmCard from './ConfirmCard.svelte'
  import MessageBubble from './MessageBubble.svelte'
  import SaturationNotice from './SaturationNotice.svelte'
  import ToolCallCard from './ToolCallCard.svelte'

  let { store }: { store: ChatStore } = $props()

  const items = $derived(buildRenderItems(store.messages))

  let scroller = $state<HTMLElement | null>(null)
  let pinned = $state(true)

  function onScroll(): void {
    if (!scroller) return
    pinned = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 48
  }
  function jump(): void {
    if (scroller) scroller.scrollTop = scroller.scrollHeight
    pinned = true
  }

  $effect(() => {
    // Re-run on any growth of the thread or the live turn.
    void items.length
    void store.liveAssistant
    void store.liveTools.length
    void store.streaming
    void store.liveQueued
    if (pinned && scroller) scroller.scrollTop = scroller.scrollHeight
  })
</script>

<div class="thread" bind:this={scroller} onscroll={onScroll}>
  {#if items.length === 0 && !store.streaming}
    <p class="empty">Ask the knowledge store anything. The assistant can search, browse and read your blocks.</p>
  {/if}

  {#each items as item (item.key)}
    {#if item.kind === 'tool'}
      <ToolCallCard name={item.name} args={item.args} result={item.result} />
      {#if item.result?.staged}
        <ConfirmCard staged={item.result.staged} />
      {/if}
    {:else if item.kind === 'user'}
      <MessageBubble role="user" content={item.content} sensitivity={item.sensitivity} />
    {:else}
      <MessageBubble role="assistant" content={item.content} sensitivity={item.sensitivity} />
      {#if item.canceled}<div class="canceled">— aborted —</div>{/if}
    {/if}
  {/each}

  {#if store.streaming}
    <div class="live">
      {#if store.liveQueued}
        <!-- MW8 queued keepalive: waiting for a free model slot, not thinking.
             The composer's Abort button cancels the wait (aborts the request →
             the server vacates the queue slot on disconnect). -->
        <div class="queued" role="status" aria-live="polite">
          <span class="q-dots" aria-hidden="true"><span></span><span></span><span></span></span>
          <span>Waiting in the queue — the model is busy with other requests. Your turn starts automatically when a slot frees up.</span>
        </div>
      {/if}
      <BackendBadge backend={store.liveBackend} />
      {#each store.liveTools as t (t.id)}
        <ToolCallCard name={t.name} args={t.arguments} result={t.result} />
        {#if t.result?.staged}
          <ConfirmCard staged={t.result.staged} />
        {/if}
      {/each}
      {#if !store.liveQueued && (store.liveAssistant !== '' || store.liveTools.length === 0)}
        <MessageBubble role="assistant" content={store.liveAssistant} streaming={true} />
      {/if}
    </div>
  {/if}

  {#if store.saturation}
    <SaturationNotice {store} />
  {:else if store.turnError}
    <div class="turn-error" role="alert">
      Turn failed: {store.turnError}. The partial above is kept — send again to retry.
    </div>
  {/if}
</div>

{#if !pinned}
  <button class="jump" type="button" onclick={jump}>↓ Newest</button>
{/if}

<style>
  .thread {
    flex: 1;
    overflow-y: auto;
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-3);
    min-height: 0;
  }
  .empty {
    margin: auto;
    max-width: 28rem;
    text-align: center;
    color: var(--text-faint);
    font-size: var(--fs-md);
  }
  .live {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  /* Queued keepalive indicator — a calm "waiting in line", distinct from the
     assistant's blinking type cursor (which means the model is producing). */
  .queued {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border: 1px dashed var(--border-strong);
    border-radius: var(--radius);
    color: var(--text-dim);
    font-size: var(--fs-sm);
  }
  .q-dots {
    flex: none;
    display: inline-flex;
    gap: 3px;
  }
  .q-dots span {
    width: 0.35rem;
    height: 0.35rem;
    border-radius: 50%;
    background: var(--text-faint);
    animation: q-bounce 1.2s ease-in-out infinite;
  }
  .q-dots span:nth-child(2) {
    animation-delay: 0.2s;
  }
  .q-dots span:nth-child(3) {
    animation-delay: 0.4s;
  }
  @keyframes q-bounce {
    0%,
    80%,
    100% {
      opacity: 0.3;
    }
    40% {
      opacity: 1;
    }
  }
  @media (prefers-reduced-motion: reduce) {
    .q-dots span {
      animation: none;
    }
  }
  .canceled {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    color: var(--warn);
    text-align: center;
  }
  .turn-error {
    border: 1px solid var(--danger-dim);
    color: var(--danger);
    background: var(--surface-1);
    border-radius: var(--radius);
    padding: var(--space-2) var(--space-3);
    font-size: var(--fs-sm);
  }
  .jump {
    position: sticky;
    bottom: var(--space-2);
    align-self: center;
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-3);
  }
</style>
