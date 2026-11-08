<script lang="ts">
  // The live keys of one consumer with their actions (design/05 §4.6 key row,
  // §5 B-20; wave W05-08 — RC-D C5; reusable by the consumer detail of C6
  // W05-09). One entry per key: hint, the SERVER's status badge
  // (data-key-status, B-11), expiry, "Revoke" (the page asks for the 4 hint
  // characters). In status `rotating` ONLY the predecessor
  // (rotate.endGraceTarget — the one key the server marks superseded)
  // carries "End grace period" (data-testid="key-end-grace"; the accessible
  // name "End grace period of key <hint>" starts with the visible text —
  // axe label-content-name-mismatch); its click
  // hands THAT key to onendgrace, the page revokes its key_id — never the
  // consumer id, never the successor. "Rotate key" (data-testid="key-rotate")
  // only where rotate.canRotate allows (the page computes `rotatable`).
  import type { ConsumerView, KeyView } from '../../lib/api/types'
  import { fmtDay, keyTone } from './format'
  import { endGraceTarget } from './rotate'

  export interface KeyChainProps {
    consumer: ConsumerView
    busy: boolean
    /** rotate.canRotate(consumer, caps) */
    rotatable: boolean
    onrevoke: (k: KeyView) => void
    /** the predecessor whose grace window ends now (model-key-revoke with ITS key_id) */
    onendgrace: (k: KeyView) => void
    onrotate: () => void
  }
  let { consumer: c, busy, rotatable, onrevoke, onendgrace, onrotate }: KeyChainProps = $props()

  const predecessor = $derived(endGraceTarget(c))
</script>

<div class="chain" data-testid="key-chain">
  {#if c.keys.length === 0}
    <span class="dim">no live key</span>
  {:else}
    <ul>
      {#each c.keys as k (k.id)}
        <li>
          <code class="hint">{k.hint}</code>
          <span class="badge {keyTone(k.status)}" data-key-status>{k.status}</span>
          <span class="when">{k.expires_at ? `expires ${fmtDay(k.expires_at)}` : 'no expiry'}</span>
          <button
            type="button"
            class="act danger"
            data-testid="key-revoke"
            aria-label={`Revoke key ${k.hint}`}
            disabled={busy}
            onclick={() => onrevoke(k)}>Revoke</button
          >
          {#if predecessor !== null && predecessor.id === k.id}
            <button
              type="button"
              class="act"
              data-testid="key-end-grace"
              aria-label={`End grace period of key ${k.hint}`}
              disabled={busy}
              onclick={() => onendgrace(k)}>End grace period</button
            >
          {/if}
        </li>
      {/each}
    </ul>
  {/if}
  {#if rotatable}
    <button
      type="button"
      class="act rotate"
      data-testid="key-rotate"
      aria-label={`Rotate key of ${c.label}`}
      disabled={busy}
      onclick={onrotate}>Rotate key</button
    >
  {/if}
</div>

<style>
  .chain {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    align-items: flex-start;
  }
  ul {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  li {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-1) var(--space-2);
  }
  .hint {
    color: var(--text);
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
  }
  .when {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    color: var(--text-dim);
    white-space: nowrap;
  }
  .dim {
    color: var(--text-faint);
  }
  .badge {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    text-transform: uppercase;
    letter-spacing: var(--label-tracking);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
    color: var(--text-dim);
    white-space: nowrap;
  }
  .badge.ok {
    color: var(--ok);
    border-color: var(--ok);
  }
  .badge.warn {
    color: var(--warn);
    border-color: var(--warn);
  }
  .act {
    min-height: 24px;
    font-family: var(--font-ui);
    font-size: var(--fs-xs);
    padding: 0 var(--space-2);
    background: var(--surface-2);
    color: var(--text);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    cursor: pointer;
    white-space: nowrap;
  }
  .act:hover:not(:disabled) {
    border-color: var(--accent);
    color: var(--accent);
  }
  .act.danger:hover:not(:disabled) {
    border-color: var(--danger);
    color: var(--danger);
  }
  .act:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
</style>
