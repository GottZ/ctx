<script lang="ts">
  // Backend badge (design 06 §3.9): which backend served the turn, whether
  // tools were offered, and — when not — why (the trust tier). Display only;
  // the trust boundary is enforced server-side in the sensitivity gate (§2.3).
  import type { BackendEvent } from '../../lib/chat/types'

  let { backend }: { backend: BackendEvent | null } = $props()

  const tone = $derived(
    backend == null ? 'idle' : backend.fallback ? 'warn' : backend.trust === 'full-trust' ? 'ok' : 'info',
  )
</script>

{#if backend}
  <span class="badge {tone}" title={backend.fallback ? 'serving from a fallback backend' : backend.trust}>
    <span class="dot"></span>
    <span class="name">{backend.backend}</span>
    <span class="sep">·</span>
    <span class="model">{backend.model}</span>
    <span class="sep">·</span>
    {#if backend.tools_active}
      <span class="tools on">tools on</span>
    {:else}
      <span class="tools off" title="tools are offered only to a full-trust backend (§2.3)">
        tools off · {backend.trust}
      </span>
    {/if}
  </span>
{/if}

<style>
  .badge {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    font-family: var(--font-mono);
    font-size: var(--label-size);
    color: var(--text-dim);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    padding: 0.1rem var(--space-2);
    max-width: 100%;
    overflow: hidden;
  }
  .dot {
    width: 0.5rem;
    height: 0.5rem;
    border-radius: 50%;
    flex: none;
  }
  .ok .dot {
    background: var(--ok);
  }
  .warn .dot {
    background: var(--warn);
  }
  .info .dot {
    background: var(--accent);
  }
  .idle .dot {
    background: var(--text-faint);
  }
  .name {
    color: var(--text);
    white-space: nowrap;
  }
  .sep {
    color: var(--text-faint);
  }
  .model {
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .tools.on {
    color: var(--ok);
  }
  .tools.off {
    color: var(--warn);
  }
</style>
