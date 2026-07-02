<script lang="ts">
  // model_map line editor (design §3.5): one (role → provider-model) row per
  // entry plus the special `default` role. Controlled (rows in, onchange out);
  // params (rare) ride along on the row and are preserved, not edited here. The
  // dialog converts rows ↔ the wire object form at save time.
  import type { ModelMapRow } from '../../../lib/backends'

  let {
    rows,
    disabled = false,
    onchange,
  }: { rows: ModelMapRow[]; disabled?: boolean; onchange: (rows: ModelMapRow[]) => void } = $props()

  function patch(i: number, next: Partial<ModelMapRow>): void {
    onchange(rows.map((r, j) => (j === i ? { ...r, ...next } : r)))
  }
  function add(): void {
    onchange([...rows, { role: '', model: '' }])
  }
  function remove(i: number): void {
    onchange(rows.filter((_, j) => j !== i))
  }
</script>

<div class="mm" class:disabled>
  {#if rows.length === 0}
    <p class="empty">no model_map — each core role this backend serves needs an entry or a <code>default</code></p>
  {/if}
  {#each rows as row, i (i)}
    <div class="mrow">
      <input
        class="role"
        type="text"
        placeholder="role / default"
        spellcheck="false"
        {disabled}
        value={row.role}
        oninput={(e) => patch(i, { role: e.currentTarget.value })}
      />
      <span class="arrow">→</span>
      <input
        class="model"
        type="text"
        placeholder="provider model id"
        spellcheck="false"
        {disabled}
        value={row.model}
        oninput={(e) => patch(i, { model: e.currentTarget.value })}
      />
      {#if row.params}
        <span class="params" title="custom params preserved (edit via CLI)">+params</span>
      {/if}
      <button type="button" class="rm" title="remove row" {disabled} onclick={() => remove(i)}>×</button>
    </div>
  {/each}
  <button type="button" class="add" {disabled} onclick={add}>+ add role mapping</button>
</div>

<style>
  .mm {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .empty {
    margin: 0;
    font-size: var(--fs-xs);
    color: var(--text-faint);
  }
  .mrow {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .role {
    width: 9rem;
    flex: none;
  }
  .model {
    flex: 1;
    min-width: 0;
  }
  .mrow input {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-2);
  }
  .arrow {
    color: var(--text-faint);
  }
  .params {
    font-size: var(--fs-2xs);
    color: var(--text-faint);
    font-family: var(--font-mono);
  }
  .rm {
    background: transparent;
    border: none;
    color: var(--text-dim);
    cursor: pointer;
    font-size: var(--fs-base);
    line-height: var(--lh-solid);
    padding: 0 var(--space-1);
  }
  .add {
    align-self: flex-start;
    font-size: var(--fs-xs);
    padding: var(--space-1) var(--space-2);
    background: transparent;
  }
  .disabled {
    opacity: 0.6;
  }
</style>
