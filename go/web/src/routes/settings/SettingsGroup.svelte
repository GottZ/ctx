<script lang="ts">
  import type { SettingsGroup } from '../../lib/settings'
  import type { SettingsModel } from './model.svelte'
  import SettingField from './SettingField.svelte'

  let { group, model }: { group: SettingsGroup; model: SettingsModel } = $props()

  const dirtyCount = $derived(model.dirtyKeys(group.prefix).length)
</script>

<section class="card" aria-label="{group.prefix} settings">
  <header>
    <h2>{group.prefix}</h2>
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
  <div class="fields">
    {#each group.settings as setting (setting.key)}
      <SettingField {setting} {model} />
    {/each}
  </div>
</section>

<style>
  .card {
    background: var(--surface-1);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }

  header {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border-bottom: 1px solid var(--border);
    position: sticky;
    top: 0;
    background: var(--surface-1);
    /* stylelint-disable-next-line scale-unlimited/declaration-strict-value -- Lokaler Stacking-Context: der sticky Gruppen-Header hebt sich um 1 über die scrollenden Zeilen; das ist KEINE globale Layer-Ebene (--z-rail..--z-window), sondern der minimale lokale Wert. Q8 entzog '1' den globalen ignoreValues (erzwingt line-height:1→--lh-solid), was diesen Alt-Wert freilegte. */
    z-index: 1;
  }
  h2 {
    margin: 0;
    font-family: var(--font-mono);
    font-size: var(--fs-md);
    font-weight: var(--fw-semibold);
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
