<script lang="ts">
  // Project picker (design 04 §4.1.5, wave U05). Source is B11 GET /api/project
  // (ReadScopes-intersected register), NOT whoami.read_scopes. 0/1/N behaviour
  // (picker.ts pure policy): 0 → provisioning hint, 1 → auto-selected label,
  // N → a <select> that writes ?scope= (URL is the single source of truth). No
  // persisted "last project" — the URL carries it (§4.1.5). Shared with /board.
  import type { ProjectRow } from '../../lib/api/types'
  import { pickerMode } from './picker'

  let {
    projects,
    selected,
    onselect,
  }: {
    /** Projects visible to the caller (already loaded). */
    projects: ProjectRow[]
    /** The active scope (from the URL), or null. */
    selected: string | null
    /** Called with the chosen scope when the user picks in the N-case. */
    onselect: (scope: string) => void
  } = $props()

  const mode = $derived(pickerMode(projects))
  const current = $derived(projects.find((p) => p.scope === selected) ?? null)

  function onchange(event: Event): void {
    const value = (event.currentTarget as HTMLSelectElement).value
    if (value !== '') onselect(value)
  }
</script>

<div class="picker" data-picker-mode={mode} data-scope={selected ?? ''}>
  {#if mode === 'empty'}
    <p class="hint" role="status">
      No projects yet — provision one via the <code>/admin</code> wizard or
      <code>ctx project init</code>.
    </p>
  {:else if mode === 'single'}
    <!-- One project: auto-selected upstream; the picker is a static label. -->
    <span class="label">
      <span class="lbl">Project</span>
      <code class="scope">{projects[0].display_name}</code>
    </span>
  {:else}
    <label class="label">
      <span class="lbl">Project</span>
      <select aria-label="Select project" value={selected ?? ''} onchange={onchange}>
        {#if current === null}
          <option value="" disabled>Select a project…</option>
        {/if}
        {#each projects as p (p.id)}
          <option value={p.scope}>{p.display_name}</option>
        {/each}
      </select>
    </label>
  {/if}
</div>

<style>
  .picker {
    display: flex;
    align-items: center;
    min-height: 2rem;
  }
  .label {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--fs-sm);
  }
  .lbl {
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
  }
  .scope {
    color: var(--text);
    font-family: var(--font-mono);
  }
  select {
    min-height: 2rem;
    padding: var(--space-1) var(--space-2);
    background: var(--surface-0);
    color: var(--text);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    font-size: var(--fs-sm);
    font-family: var(--font-mono);
  }
  .hint {
    margin: 0;
    color: var(--text-dim);
    font-size: var(--fs-sm);
  }
  .hint code,
  .scope {
    font-family: var(--font-mono);
  }
</style>
