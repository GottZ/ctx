<script lang="ts">
  // The message thread: persisted history (via buildRenderItems) plus the live
  // turn below it while streaming. Auto-scrolls to the newest content unless the
  // user has scrolled up (scroll-lock + a "jump to latest" affordance).
  import { buildRenderItems } from '../../lib/chat/render'
  import type { ChatStore } from '../../lib/chat/store.svelte'
  import BackendBadge from './BackendBadge.svelte'
  import ConfirmCard from './ConfirmCard.svelte'
  import MessageBubble from './MessageBubble.svelte'
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
      <BackendBadge backend={store.liveBackend} />
      {#each store.liveTools as t (t.id)}
        <ToolCallCard name={t.name} args={t.arguments} result={t.result} />
        {#if t.result?.staged}
          <ConfirmCard staged={t.result.staged} />
        {/if}
      {/each}
      {#if store.liveAssistant !== '' || store.liveTools.length === 0}
        <MessageBubble role="assistant" content={store.liveAssistant} streaming={true} />
      {/if}
    </div>
  {/if}

  {#if store.turnError}
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
