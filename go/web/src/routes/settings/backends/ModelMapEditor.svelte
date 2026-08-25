<script module lang="ts">
  // Draft-text → params projection, exported for tests (mount() is
  // unavailable in this env — see VaultForm.svelte.test.ts for the pattern).
  // ok:false = keep the last valid state; ok:true with params:undefined =
  // clear the row's params.
  export function parseParamsDraft(
    text: string,
  ): { ok: true; params: Record<string, unknown> | undefined } | { ok: false } {
    const trimmed = text.trim()
    if (trimmed === '' || trimmed === '{}') {
      return { ok: true, params: undefined }
    }
    try {
      const parsed: unknown = JSON.parse(trimmed)
      if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
        return { ok: false }
      }
      return { ok: true, params: parsed as Record<string, unknown> }
    } catch {
      return { ok: false }
    }
  }
</script>

<script lang="ts">
  // model_map line editor (design §3.5): one (role → provider-model) row per
  // entry plus the special `default` role. Controlled (rows in, onchange out).
  // params are edited per row as a free JSON object — since the generic wire
  // passthrough (#39) any key is meaningful (chat_template_kwargs, think,
  // max_tokens, provider knobs), so a schema form would be wrong here. The
  // dialog converts rows ↔ the wire object form at save time.
  import type { ModelMapRow } from '../../../lib/backends'

  let {
    rows,
    disabled = false,
    onchange,
  }: { rows: ModelMapRow[]; disabled?: boolean; onchange: (rows: ModelMapRow[]) => void } = $props()

  // Per-row editor state, index-keyed like the rows themselves. The draft
  // text lives here (not derived from rows) so invalid JSON can sit in the
  // textarea while typing without being clobbered; only valid JSON reaches
  // onchange. Row removal resets all editors — indexes shift.
  let open = $state<Record<number, boolean>>({})
  let drafts = $state<Record<number, string>>({})
  let invalid = $state<Record<number, boolean>>({})

  function patch(i: number, next: Partial<ModelMapRow>): void {
    onchange(rows.map((r, j) => (j === i ? { ...r, ...next } : r)))
  }
  function add(): void {
    onchange([...rows, { role: '', model: '' }])
  }
  function remove(i: number): void {
    open = {}
    drafts = {}
    invalid = {}
    onchange(rows.filter((_, j) => j !== i))
  }

  function toggleParams(i: number): void {
    if (open[i]) {
      open = { ...open, [i]: false }
      return
    }
    drafts = {
      ...drafts,
      [i]: rows[i].params ? JSON.stringify(rows[i].params, null, 2) : '',
    }
    invalid = { ...invalid, [i]: false }
    open = { ...open, [i]: true }
  }

  function editParams(i: number, text: string): void {
    drafts = { ...drafts, [i]: text }
    const res = parseParamsDraft(text)
    invalid = { ...invalid, [i]: !res.ok }
    if (res.ok) {
      patch(i, { params: res.params })
    }
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
      <button
        type="button"
        class="params-toggle"
        class:active={open[i]}
        class:has-params={!!row.params}
        title={row.params ? 'edit params (set on this row)' : 'add params (temperature, max_tokens, think, chat_template_kwargs, …)'}
        {disabled}
        onclick={() => toggleParams(i)}
      >
        params{row.params ? ' ●' : ''}
      </button>
      <button type="button" class="rm" title="remove row" {disabled} onclick={() => remove(i)}>×</button>
    </div>
    {#if open[i]}
      <div class="pedit">
        <textarea
          class:invalid={invalid[i]}
          rows="4"
          spellcheck="false"
          placeholder={'{\n  "max_tokens": 600,\n  "think": false,\n  "chat_template_kwargs": { "enable_thinking": false }\n}'}
          {disabled}
          value={drafts[i] ?? ''}
          oninput={(e) => editParams(i, e.currentTarget.value)}
        ></textarea>
        <p class="phint" class:err={invalid[i]}>
          {#if invalid[i]}
            invalid JSON — last valid state is kept
          {:else}
            JSON object, sent verbatim as model_map params (empty = none). Unknown keys reach the wire on OpenAI-protocol backends.
          {/if}
        </p>
      </div>
    {/if}
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
  .params-toggle {
    flex: none;
    font-size: var(--fs-2xs);
    font-family: var(--font-mono);
    color: var(--text-dim);
    background: transparent;
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    cursor: pointer;
  }
  .params-toggle.has-params {
    color: var(--text);
  }
  .params-toggle.active {
    border-color: var(--accent);
    color: var(--accent);
  }
  .pedit {
    margin-left: 9rem;
    padding-left: var(--space-2);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .pedit textarea {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    padding: var(--space-1) var(--space-2);
    resize: vertical;
    min-height: 4.5rem;
  }
  .pedit textarea.invalid {
    border-color: var(--danger, #c33);
  }
  .phint {
    margin: 0;
    font-size: var(--fs-2xs);
    color: var(--text-faint);
  }
  .phint.err {
    color: var(--danger, #c33);
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
