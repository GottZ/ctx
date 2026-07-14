<script lang="ts">
  import { ALL_LINK_CLASSES, defaultFilters, isDefault, type GraphFilters } from '../../lib/graph/filters'

  let {
    filters,
    categories,
    structClasses = [],
    onchange,
  }: {
    filters: GraphFilters
    /** Categories present in the loaded graph (panel options). */
    categories: string[]
    /** Structural link classes loaded in the client (GraphPage settle sweep) —
     *  registry-driven, no hardcoded vocabulary (GC2, design 03-§4.3). */
    structClasses?: string[]
    /** Always emits a NEW object — the page/view react on reference change. */
    onchange: (next: GraphFilters) => void
  } = $props()

  // Checkbox source = loaded classes ∪ currently hidden — a hidden class must
  // keep its (unchecked) checkbox even after its last edge left the client,
  // or the user could never re-enable it (same pattern as the category list).
  const structClassOptions = $derived(
    [...new Set([...structClasses, ...filters.structClassesHidden])].sort(),
  )

  function toggleLinkClass(rel: string): void {
    const has = filters.linkClasses.includes(rel)
    const next = has ? filters.linkClasses.filter((r) => r !== rel) : [...filters.linkClasses, rel]
    onchange({ ...filters, linkClasses: next })
  }

  // Blocklist toggle (GC2): unchecking ADDS to structClassesHidden, checking
  // removes — no materialization step, no special state.
  function toggleStructClass(cls: string): void {
    const hidden = filters.structClassesHidden.includes(cls)
    const next = hidden
      ? filters.structClassesHidden.filter((c) => c !== cls)
      : [...filters.structClassesHidden, cls]
    onchange({ ...filters, structClassesHidden: next })
  }

  function toggleCategory(cat: string): void {
    const has = filters.categories.includes(cat)
    const next = has ? filters.categories.filter((c) => c !== cat) : [...filters.categories, cat]
    onchange({ ...filters, categories: next })
  }
</script>

<div class="panel">
  <fieldset>
    <legend>link class</legend>
    {#each ALL_LINK_CLASSES as rel (rel)}
      <label class="check">
        <input type="checkbox" checked={filters.linkClasses.includes(rel)} onchange={() => toggleLinkClass(rel)} />
        {rel}
      </label>
    {/each}
    <!-- structural sub-block (GC2): same .check interaction pattern, checked =
         NOT in the blocklist. Options are registry-driven (loaded ∪ hidden) —
         swatches + a11y texts land with GC3. -->
    {#each structClassOptions as cls (cls)}
      <label class="check">
        <input
          type="checkbox"
          checked={!filters.structClassesHidden.includes(cls)}
          onchange={() => toggleStructClass(cls)}
        />
        {cls}
      </label>
    {/each}
  </fieldset>

  <fieldset>
    <legend>min confidence (dream)</legend>
    <input
      class="conf"
      type="number"
      min="0"
      max="1"
      step="0.05"
      value={filters.minConfidence}
      oninput={(e) => onchange({ ...filters, minConfidence: Number(e.currentTarget.value) || 0 })}
    />
  </fieldset>

  {#if categories.length > 0}
    <fieldset>
      <legend>category</legend>
      {#each categories as cat (cat)}
        <label class="check">
          <input type="checkbox" checked={filters.categories.includes(cat)} onchange={() => toggleCategory(cat)} />
          {cat}
        </label>
      {/each}
    </fieldset>
  {/if}

  <fieldset>
    <legend>created</legend>
    <input
      type="date"
      value={filters.createdAfter}
      oninput={(e) => onchange({ ...filters, createdAfter: e.currentTarget.value })}
    />
    <span class="sep">–</span>
    <input
      type="date"
      value={filters.createdBefore}
      oninput={(e) => onchange({ ...filters, createdBefore: e.currentTarget.value })}
    />
  </fieldset>

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
    font-size: var(--fs-sm);
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

  .check {
    display: inline-flex;
    align-items: center;
    gap: 0.3rem;
    font-family: var(--font-mono);
    color: var(--text-dim);
    cursor: pointer;
  }
  .check input {
    accent-color: var(--accent);
    margin: 0;
  }

  .conf {
    width: 5rem;
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-2);
  }

  input[type='date'] {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    padding: var(--space-1) var(--space-2);
  }
  .sep {
    color: var(--text-faint);
  }

  .reset {
    font-size: var(--fs-xs);
    padding: var(--space-1) var(--space-2);
  }
</style>
