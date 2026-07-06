<script lang="ts">
  // Capacity-block notice (MW8b, DECISIONS B2). Shown when the store enters the
  // saturation state — a pre-stream HTTP 429 (dispatcher rejection / per-scope
  // turn cap) or a mid-stream `saturated` event. Neither is a fault: the copy
  // is a calm "system busy" rather than the red "turn failed", and it offers a
  // retry + cancel. With a Retry-After the store runs a jittered countdown and
  // auto-retries on expiry; this component only reflects store state (the DOM
  // shell — the timing/jitter logic lives in the store, unit-tested there).
  import type { ChatStore } from '../../lib/chat/store.svelte'

  let { store }: { store: ChatStore } = $props()

  const countingDown = $derived(store.saturation?.secondsLeft != null)
</script>

{#if store.saturation}
  <div class="saturation" role="status" aria-live="polite">
    <span class="pulse" aria-hidden="true"></span>
    <span class="msg">
      {#if countingDown}
        System busy — no free model slot. Retrying automatically in {store.saturation.secondsLeft}s.
      {:else}
        System is saturated — no free model slot right now. Please retry shortly.
      {/if}
    </span>
    <span class="actions">
      <button type="button" class="retry" onclick={() => store.retryLast()}>Retry now</button>
      <button type="button" class="cancel" onclick={() => store.cancelSaturation()}>Cancel</button>
    </span>
  </div>
{/if}

<style>
  .saturation {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
    border: 1px solid var(--warn);
    background: var(--surface-1);
    color: var(--text);
    border-radius: var(--radius);
    padding: var(--space-2) var(--space-3);
    font-size: var(--fs-sm);
  }
  .msg {
    flex: 1;
    min-width: 12rem;
  }
  .pulse {
    flex: none;
    width: 0.6rem;
    height: 0.6rem;
    border-radius: 50%;
    background: var(--warn);
    animation: sat-pulse 1.4s ease-in-out infinite;
  }
  .actions {
    display: flex;
    gap: var(--space-2);
  }
  .retry {
    color: var(--accent);
    border-color: var(--accent);
  }
  .cancel {
    color: var(--text-dim);
  }
  @keyframes sat-pulse {
    0%,
    100% {
      opacity: 1;
    }
    50% {
      opacity: 0.3;
    }
  }
  @media (prefers-reduced-motion: reduce) {
    .pulse {
      animation: none;
    }
  }
</style>
