<script lang="ts">
  import { groupDomId, type SettingsGroup } from '../../lib/settings'
  import type { SettingsModel } from './model.svelte'
  import type { SettingsUi } from './ui.svelte'
  import SettingField from './SettingField.svelte'
  import BackoffCurve from './BackoffCurve.svelte'

  let { group, model, ui }: { group: SettingsGroup; model: SettingsModel; ui: SettingsUi } = $props()

  const domId = $derived(groupDomId(group.prefix))

  const dirtyCount = $derived(model.dirtyKeys(group.prefix).length)
  const visible = $derived(ui.visibleSettings(group.settings))
  // Search overrides the stored collapse choice: a group with hits is open
  // (a hit inside a closed card would be invisible), one without hides
  // entirely (handled by the page). Dirty edits keep the body open too — a
  // pending edit must never sit behind a closed header.
  const open = $derived(ui.searching || dirtyCount > 0 || !ui.isCollapsed(group.prefix))
  const showCurve = $derived(
    group.prefix === 'dream' && (!ui.searching || visible.some(({ setting }) => setting.key.startsWith('dream.backoff_'))),
  )
</script>

<section class="card" aria-label="{group.prefix} settings" id={domId}>
  <header class:open>
    <button
      class="disclosure"
      type="button"
      aria-expanded={open}
      aria-controls="{domId}-fields"
      disabled={ui.searching || dirtyCount > 0}
      onclick={() => ui.toggle(group.prefix)}
    >
      <span class="chevron" class:open aria-hidden="true">▸</span>
      <h2>{group.prefix}</h2>
      <span class="count">{ui.searching ? `${visible.length}/${group.settings.length}` : group.settings.length}</span>
    </button>
    {#if dirtyCount > 0}
      <span class="dirty-count">{dirtyCount} unsaved</span>
    {/if}
    <button
      class="save"
      type="button"
      disabled={dirtyCount === 0 || model.saving}
      onclick={() => void model.saveGroup(group.prefix)}
    >
      {model.saving ? 'Saving…' : 'Save'}
    </button>
  </header>
  {#if open}
    <div class="fields" id="{domId}-fields">
      {#if showCurve}
        <BackoffCurve {model} />
      {/if}
      {#each visible as { setting, result } (setting.key)}
        <SettingField {setting} {model} {result} />
      {/each}
    </div>
  {/if}
</section>

<style>
  .card {
    background: var(--surface-1);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    /* jump-nav target: keep the sticky page toolbar from covering the header */
    scroll-margin-top: calc(var(--space-8) + var(--space-4));
  }

  header {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-1) var(--space-3) var(--space-1) var(--space-2);
  }
  header.open {
    border-bottom: 1px solid var(--border);
    position: sticky;
    top: 0;
    background: var(--surface-1);
    /* stylelint-disable-next-line scale-unlimited/declaration-strict-value -- Lokaler Stacking-Context: der sticky Gruppen-Header hebt sich um 1 über die scrollenden Zeilen; das ist KEINE globale Layer-Ebene (--z-rail..--z-window), sondern der minimale lokale Wert. Q8 entzog '1' den globalen ignoreValues (erzwingt line-height:1→--lh-solid), was diesen Alt-Wert freilegte. */
    z-index: 1;
  }

  .disclosure {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex: 1;
    min-width: 0;
    background: transparent;
    border: none;
    padding: var(--space-1);
    cursor: pointer;
    text-align: left;
    color: inherit;
  }
  .disclosure:disabled {
    cursor: default;
  }
  .chevron {
    color: var(--text-faint);
    transition: transform var(--dur-1) var(--ease);
  }
  .chevron.open {
    transform: rotate(90deg);
  }
  h2 {
    margin: 0;
    font-family: var(--font-mono);
    font-size: var(--fs-md);
    font-weight: var(--fw-semibold);
  }
  .count {
    font-family: var(--font-mono);
    font-size: var(--fs-2xs);
    color: var(--text-faint);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
  }
  .dirty-count {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--accent);
  }
  .save {
    margin-left: auto;
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-3);
  }

  .fields {
    display: flex;
    flex-direction: column;
    padding: var(--space-1) 0;
  }

</style>
