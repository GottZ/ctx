<script lang="ts">
  // The composer: a growing textarea, Enter to send (Shift+Enter for a newline),
  // and a button that flips to Abort while a turn streams. The abort is also
  // wired to beforeunload by ChatPage so navigating away frees the slot.
  import type { ChatStore } from '../../lib/chat/store.svelte'

  let { store }: { store: ChatStore } = $props()

  let text = $state('')

  function send(): void {
    const t = text.trim()
    if (t === '' || store.streaming) return
    text = ''
    void store.send(t)
  }

  function onKeydown(e: KeyboardEvent): void {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      send()
    }
  }
</script>

<div class="composer">
  <textarea
    bind:value={text}
    onkeydown={onKeydown}
    rows="2"
    placeholder="Ask the knowledge store… (Enter to send, Shift+Enter for a newline)"
    disabled={store.streaming}
    aria-label="message"
  ></textarea>
  {#if store.streaming}
    <button class="abort" type="button" onclick={() => store.abort()}>Abort</button>
  {:else}
    <button class="send" type="button" onclick={send} disabled={text.trim() === ''}>Send</button>
  {/if}
</div>

<style>
  .composer {
    display: flex;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border-top: 1px solid var(--border);
    background: var(--surface-1);
  }
  textarea {
    flex: 1;
    resize: vertical;
    min-height: 2.5rem;
    max-height: 12rem;
    font-family: var(--font-ui);
    font-size: var(--fs-md);
    padding: var(--space-2);
    background: var(--surface-0);
    color: var(--text);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
  textarea:disabled {
    opacity: 0.6;
  }
  button {
    align-self: flex-end;
  }
  .abort {
    color: var(--danger);
    border-color: var(--danger-dim);
  }
</style>
