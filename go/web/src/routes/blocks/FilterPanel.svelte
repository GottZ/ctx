<script lang="ts">
  // Block-workbench facet panel (W2): category single-select (options from
  // manage list-categories), a free-text tag entry (overlap match), and a
  // CLIENT-SIDE scope chip over the scopes present in the loaded results. One
  // reducer state in, a NEW BlockFilters out on every change — the page reacts
  // on the reference change. Mirrors routes/graph/FilterPanel.svelte.
  import { defaultFilters, isDefault, type BlockFilters } from '../../lib/blocks/filters'
  import type { CategoryCount } from '../../lib/api/blocks'

  let {
    filters,
    categories,
    scopes,
    onchange,
  }: {
    filters: BlockFilters
    /** Category facet options (manage list-categories, count-desc). */
    categories: CategoryCount[]
    /** Scopes present in the loaded results — the client-side scope chips. */
    scopes: string[]
    /** Always emits a NEW object — the page reacts on the reference change. */
    onchange: (next: BlockFilters) => void
  } = $props()

  // Local mirror for the comma-separated tag entry; commit on change/Enter so
  // a partial word doesn't trigger a server round-trip per keystroke. Seeded
  // and kept in sync by the $effect below so a reset (filters replaced
  // elsewhere) reflects in the box.
  let tagInput = $state('')

  $effect(() => {
    tagInput = filters.tags.join(', ')
  })

  function commitTags(raw: string): void {
    const tags = raw
      .split(',')
      .map((t) => t.trim())
      .filter((t) => t !== '')
    onchange({ ...filters, tags })
  }

  function pickScope(scope: string): void {
    // Toggle: clicking the active chip clears the scope facet (back to all).
    onchange({ ...filters, scope: filters.scope === scope ? '' : scope })
  }
</script>

<div class="panel">
  <fieldset>
    <legend>category</legend>
    <select
      value={filters.category}
      onchange={(e) => onchange({ ...filters, category: e.currentTarget.value })}
    >
      <option value="">all categories</option>
      {#each categories as c (c.category)}
        <option value={c.category}>{c.category} ({c.count})</option>
      {/each}
    </select>
  </fieldset>

  <fieldset>
    <legend>tags</legend>
    <input
      class="tags"
      type="text"
      placeholder="comma-separated, overlap match"
      spellcheck="false"
      value={tagInput}
      oninput={(e) => (tagInput = e.currentTarget.value)}
      onchange={(e) => commitTags(e.currentTarget.value)}
    />
  </fieldset>

  {#if scopes.length > 1}
    <fieldset>
      <legend>scope</legend>
      {#each scopes as s (s)}
        <button
          type="button"
          class="chip"
          class:active={filters.scope === s}
          aria-pressed={filters.scope === s}
          onclick={() => pickScope(s)}
        >
          {s}
        </button>
      {/each}
    </fieldset>
  {/if}

  {#if !isDefault(filters)}
    <button class="reset" type="button" onclick={() => onchange(defaultFilters())}>reset filters</button>
  {/if}
</div>

<style>
  .panel {
    display: flex;
    flex-wrap: wrap;
    align-items: flex-end;
    gap: var(--space-3);
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    background: var(--surface-1);
    font-size: 0.8rem;
  }

  fieldset {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    border: none;
    margin: 0;
    padding: 0;
  }
  legend {
    float: left;
    padding: 0 var(--space-2) 0 0;
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-faint);
  }

  select,
  .tags {
    font-family: var(--font-mono);
    font-size: 0.8rem;
    padding: var(--space-1) var(--space-2);
    color-scheme: dark;
  }
  .tags {
    width: 16rem;
    max-width: 100%;
  }

  .chip {
    font-family: var(--font-mono);
    font-size: 0.75rem;
    text-transform: lowercase;
    padding: var(--space-1) var(--space-2);
    background: var(--surface-2);
    border: 1px solid var(--border);
    color: var(--text-dim);
  }
  .chip.active {
    background: var(--accent-dim);
    border-color: var(--accent);
    color: var(--text);
  }

  .reset {
    font-size: 0.75rem;
    padding: var(--space-1) var(--space-2);
  }
</style>
