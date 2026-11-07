<script lang="ts">
  // One row of the /gateway consumer list (W05-05 markup, wave W05-06 adds the
  // row actions, design/05 §4.5/§4.6): Revoke per live key and Deactivate —
  // revoking is always allowed to whoever reads the row (tenant-admin of the
  // consumer's tenant or server-admin), peer rows included — and Reactivate
  // only where the server would grant it (canReactivate: server-admin, or a
  // tenant-admin with self-service on an app consumer — never a granting
  // control on a ctx-peer row for a tenant-admin, V-01-b mirror). The buttons
  // only REQUEST: the page opens the ConfirmDialog (typed 4-hint-character gate
  // for revoke/deactivate) and the server decides. No status is derived here —
  // every badge is the server's verdict (B-11).
  //
  // Selectors for the live isolation probe (W05-18) are data-testid:
  // consumer-row, consumer-label, key-revoke, consumer-deactivate,
  // consumer-reactivate.
  import type { ConsumerView, KeyView } from '../../lib/api/types'
  import { STATUS_LABEL, consumerTone, fmtDay, fmtMinute, keyTone } from './format'

  let {
    consumer: c,
    crossTenant,
    reactivatable,
    busy,
    onrevoke,
    ondeactivate,
    onreactivate,
  }: {
    consumer: ConsumerView
    /** server-admin view: the tenant column */
    crossTenant: boolean
    /** canReactivate(c, caps) — the UI mirror of the server's granting rule */
    reactivatable: boolean
    /** a mutation of this row is in flight */
    busy: boolean
    onrevoke: (k: KeyView) => void
    ondeactivate: () => void
    onreactivate: () => void
  } = $props()
</script>

<tr data-consumer-row data-testid="consumer-row" class:off={!c.active}>
  <td class="label">
    <a class="label-text" data-testid="consumer-label" href="/gateway/consumers/{c.id}">{c.label}</a>
    {#if c.description}<span class="desc">{c.description}</span>{/if}
  </td>
  <td><span class="badge" class:peer={c.kind === 'ctx-peer'}>{c.kind}</span></td>
  <td>
    <span class="badge {consumerTone(c.status)}" data-consumer-status>{STATUS_LABEL[c.status] ?? c.status}</span>
  </td>
  {#if crossTenant}<td><span class="chip">{c.tenant_slug}</span></td>{/if}
  <td class="keys">
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
          </li>
        {/each}
      </ul>
    {/if}
  </td>
  <td class="when">
    {#if c.last_used_at}
      {fmtMinute(c.last_used_at)}
      <span class="approx" title="recorded at one-minute precision">±1 min</span>
    {:else}
      <span class="dim">never</span>
    {/if}
  </td>
  <td class="when">{fmtDay(c.created_at)}</td>
  <td class="actions">
    {#if c.active}
      <button
        type="button"
        class="act danger"
        data-testid="consumer-deactivate"
        aria-label={`Deactivate ${c.label}`}
        disabled={busy}
        onclick={ondeactivate}>Deactivate</button
      >
    {:else if reactivatable}
      <button
        type="button"
        class="act"
        data-testid="consumer-reactivate"
        aria-label={`Reactivate ${c.label}`}
        disabled={busy}
        onclick={onreactivate}>Reactivate</button
      >
    {:else}
      <span class="dim">—</span>
    {/if}
  </td>
</tr>

<style>
  tr.off .label-text {
    opacity: 0.55;
  }
  .label {
    font-family: var(--font-mono);
  }
  .label-text {
    white-space: nowrap;
  }
  .desc {
    display: block;
    color: var(--text-dim);
    font-family: var(--font-ui);
    font-size: var(--fs-xs);
  }
  .chip {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    color: var(--text-dim);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
    white-space: nowrap;
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
  .badge.peer {
    color: var(--accent);
    border-color: var(--accent);
  }
  .keys ul {
    margin: 0;
    padding: 0;
    list-style: none;
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .keys li {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    white-space: nowrap;
  }
  .hint {
    color: var(--text);
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
  }
  .dim {
    color: var(--text-faint);
  }
  .when {
    color: var(--text-dim);
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }
  .approx {
    font-size: var(--fs-xs);
    color: var(--text-dim);
  }
  .actions {
    white-space: nowrap;
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
