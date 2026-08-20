<script lang="ts">
  import type { SettingView } from '../../lib/api/types'
  import { highlightRanges, type FuzzyResult } from '../../lib/fuzzy'
  import Highlight from '../../lib/components/Highlight.svelte'
  import {
    formatValue,
    isEditable,
    mutabilityNote,
    selectOptions,
    typeHint,
    widgetFor,
  } from '../../lib/settings'
  import type { SettingsModel } from './model.svelte'

  let {
    setting,
    model,
    result = null,
  }: { setting: SettingView; model: SettingsModel; result?: FuzzyResult | null } = $props()

  const key = $derived(setting.key)
  const keyRanges = $derived(result === null ? [] : highlightRanges(result.hits, 'key'))
  const descRanges = $derived(result === null ? [] : highlightRanges(result.hits, 'description'))
  const widget = $derived(widgetFor(setting.type))
  const editable = $derived(isEditable(setting))
  const note = $derived(mutabilityNote(setting))
  const dirty = $derived(model.isDirty(key))
  const error = $derived(model.errors[key])
  const warnings = $derived(model.warnings[key] ?? [])
  const issues = $derived(model.issuesFor(key))
  const placeholder = $derived(
    setting.sensitive || model.drafts[key] === '' ? formatValue(setting.value) : (typeHint(setting.type) ?? ''),
  )
  // The empty select option exists only as a current state (ScalarValue
  // rejects ""), so it renders disabled — reachable by reset, not by edit.
  const showEmptyOption = $derived(widget === 'select' && model.drafts[key] === '')
</script>

<div class="field" class:dirty>
  <div class="head">
    <label class="key" for={key}><Highlight text={key} ranges={keyRanges} /></label>
    <span class="badge source-{setting.source}" title="effective-value source">{setting.source}</span>
    {#if setting.sensitive}
      <span class="badge sensitive" title="secret-class key — values are masked everywhere">sensitive</span>
    {/if}
    {#if setting.superseded}
      <span class="badge locked" title={note}>superseded</span>
    {:else if !editable}
      <span class="badge locked" title={note}>{setting.mutability}</span>
    {/if}
    {#if setting.env_var}
      <code class="env">{setting.env_var}</code>
    {/if}
  </div>

  {#if setting.description}
    <p class="desc"><Highlight text={setting.description} ranges={descRanges} /></p>
  {/if}

  <div class="row">
    {#if widget === 'switch' && !setting.sensitive}
      <input
        id={key}
        type="checkbox"
        role="switch"
        disabled={!editable || model.saving}
        checked={model.drafts[key] === 'true'}
        onchange={(e) => (model.drafts[key] = e.currentTarget.checked ? 'true' : 'false')}
      />
    {:else if widget === 'number' && !setting.sensitive}
      <!-- one-way value + oninput: bind:value on type=number coerces the
           draft to a number, breaking the model's string invariant. -->
      <input
        id={key}
        type="number"
        step={setting.type === 'int' ? '1' : 'any'}
        disabled={!editable || model.saving}
        {placeholder}
        value={model.drafts[key]}
        oninput={(e) => (model.drafts[key] = e.currentTarget.value)}
      />
    {:else if widget === 'select' && !setting.sensitive}
      <select id={key} disabled={!editable || model.saving} bind:value={model.drafts[key]}>
        {#if showEmptyOption}
          <option value="" disabled>(unset)</option>
        {/if}
        {#each selectOptions(setting.type) as option (option)}
          <option value={option}>{option}</option>
        {/each}
      </select>
    {:else if widget === 'readonly'}
      <span class="readonly-value" title="unknown registry type {setting.type} — read-only in this UI version">
        {formatValue(setting.value)}
      </span>
    {:else}
      <input
        id={key}
        type="text"
        spellcheck="false"
        autocomplete="off"
        disabled={!editable || model.saving}
        {placeholder}
        bind:value={model.drafts[key]}
      />
    {/if}

    {#if dirty}
      <button class="ghost" type="button" title="discard this edit" onclick={() => model.revertDraft(key)}>
        undo
      </button>
    {/if}
    {#if setting.source === 'db' && editable}
      <button
        class="ghost"
        type="button"
        title="DELETE the override — revert to {setting.env_var ? 'env' : 'default'}"
        disabled={model.saving}
        onclick={() => void model.reset(key)}
      >
        reset
      </button>
    {/if}
  </div>

  {#if setting.sensitive && editable}
    <p class="hint">takes a secret <em>name</em> (create it via <code>ctx secrets set</code>), never the value</p>
  {:else if note}
    <p class="hint">{note}</p>
  {/if}

  {#if error}
    <p class="problem error" role="alert">{error}</p>
  {/if}
  {#each warnings as warning (warning)}
    <p class="problem warn" role="status">{warning}</p>
  {/each}
  {#each issues as issue (issue.message)}
    <p class="problem {issue.severity}" role="status">{issue.message}</p>
  {/each}
</div>

<style>
  .field {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    padding: var(--space-2) var(--space-3);
    border-left: 2px solid transparent;
  }
  .field.dirty {
    border-left-color: var(--accent);
    background: var(--accent-dim);
  }

  .head {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
    flex-wrap: wrap;
  }
  .key {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    color: var(--text);
  }
  .env {
    margin-left: auto;
    font-size: var(--fs-2xs);
    color: var(--text-faint);
    background: transparent;
    padding: 0;
  }

  .badge {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    border-radius: var(--radius);
    padding: 0 var(--space-1);
    border: 1px solid var(--border-strong);
    color: var(--text-faint);
  }
  .badge.source-db {
    color: var(--accent);
    border-color: var(--accent);
  }
  .badge.source-env {
    color: var(--text-dim);
  }
  .badge.sensitive {
    color: var(--warn);
    border-color: var(--warn);
  }
  .badge.locked {
    border-style: dashed;
  }

  .row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .row input[type='text'],
  .row input[type='number'] {
    flex: 1;
    min-width: 0;
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-2);
  }
  .row select {
    font: inherit;
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    color: var(--text);
    background: var(--surface-0);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    padding: var(--space-1) var(--space-2);
  }
  .row input[type='checkbox'] {
    width: 1.1rem;
    height: 1.1rem;
    accent-color: var(--accent);
    padding: 0;
  }
  .row input:disabled,
  .row select:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }

  .readonly-value {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    color: var(--text-dim);
  }

  .ghost {
    font-size: var(--fs-xs);
    padding: 0 var(--space-2);
    background: transparent;
  }

  .hint {
    margin: 0;
    font-size: var(--fs-xs);
    color: var(--text-faint);
  }

  .desc {
    margin: 0;
    font-size: var(--fs-xs);
    color: var(--text-dim);
    max-width: 72ch;
  }

  .problem {
    margin: 0;
    font-size: var(--fs-xs);
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius);
    border: 1px solid;
  }
  .problem.error {
    color: var(--danger);
    border-color: var(--danger);
    background: var(--danger-dim);
  }
  .problem.warn {
    color: var(--warn);
    border-color: var(--warn);
    background: transparent;
  }
</style>
