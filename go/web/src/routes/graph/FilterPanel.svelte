<script lang="ts">
  import MultiSelect from '../../lib/components/MultiSelect.svelte'
  import {
    ALL_LINK_CLASSES,
    categoryChecked,
    defaultFilters,
    isDefault,
    toggleCategory,
    type GraphFilters,
  } from '../../lib/graph/filters'

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

  // Option source = loaded classes ∪ currently filtered — a hidden class must
  // keep its (unchecked) row even after its last edge left the client, or the
  // user could never re-enable it. Same union for categories: a selected
  // category whose last node was evicted must stay unselectable-off.
  const structClassOptions = $derived(
    [...new Set([...structClasses, ...filters.structClassesHidden])].sort(),
  )
  const categoryOptions = $derived([...new Set([...categories, ...filters.categories])].sort())

  // ONE dropdown over both edge vocabularies (GC3: the list IS the edge
  // legend): dream classes first (fixed five), then the registry-driven
  // structural classes. The two halves keep their different filter models —
  // dream = allowlist, structural = blocklist (GC2) — behind one checked().
  const edgeOptions = $derived([...ALL_LINK_CLASSES, ...structClassOptions])

  const isDream = (opt: string): boolean => (ALL_LINK_CLASSES as readonly string[]).includes(opt)

  function edgeChecked(opt: string): boolean {
    return isDream(opt) ? filters.linkClasses.includes(opt) : !filters.structClassesHidden.includes(opt)
  }

  function toggleEdge(opt: string): void {
    if (isDream(opt)) {
      const has = filters.linkClasses.includes(opt)
      const next = has ? filters.linkClasses.filter((r) => r !== opt) : [...filters.linkClasses, opt]
      onchange({ ...filters, linkClasses: next })
    } else {
      // Blocklist toggle (GC2): unchecking ADDS to structClassesHidden,
      // checking removes — no materialization step, no special state.
      const hidden = filters.structClassesHidden.includes(opt)
      const next = hidden
        ? filters.structClassesHidden.filter((c) => c !== opt)
        : [...filters.structClassesHidden, opt]
      onchange({ ...filters, structClassesHidden: next })
    }
  }

  // "all" restores default edge visibility; "none"/"only" blocklist every
  // KNOWN structural class — unknown registry growth stays visible in every
  // filter state (the GC2 blocklist invariant holds by construction).
  function edgesAll(): void {
    onchange({ ...filters, linkClasses: [...ALL_LINK_CLASSES], structClassesHidden: [] })
  }
  function edgesNone(): void {
    onchange({ ...filters, linkClasses: [], structClassesHidden: structClassOptions })
  }
  function edgeOnly(opt: string): void {
    if (isDream(opt)) {
      onchange({ ...filters, linkClasses: [opt], structClassesHidden: structClassOptions })
    } else {
      onchange({ ...filters, linkClasses: [], structClassesHidden: structClassOptions.filter((c) => c !== opt) })
    }
  }

  // Categories: allowlist model (empty = all pass) behind the all-checked
  // presentation — toggleCategory (filters.ts, unit-pinned) materializes and
  // normalizes. No "none": an empty allowlist MEANS "all", so none is
  // unrepresentable; "only" covers the isolation use case.
  function onCategoryToggle(cat: string): void {
    onchange({ ...filters, categories: toggleCategory(filters.categories, categoryOptions, cat) })
  }
  function categoriesAll(): void {
    onchange({ ...filters, categories: [] })
  }
  function categoryOnly(cat: string): void {
    onchange({ ...filters, categories: [cat] })
  }
</script>

<div class="panel">
  <!-- GC3: the dropdown IS the edge legend (design 03-§4.4) — every row
       carries an aria-hidden swatch mirroring the canvas form language:
       straight line (dream), strong line (supersedes), curved arrow
       (structural). Colors come from the SAME --graph-* tokens the canvas
       bakes, so the legend can never drift from the render. -->
  <MultiSelect
    label="link class"
    options={edgeOptions}
    checked={edgeChecked}
    ontoggle={toggleEdge}
    onall={edgesAll}
    onnone={edgesNone}
    ononly={edgeOnly}
  >
    {#snippet option(opt)}
      {#if isDream(opt)}
        <svg class="sw {opt === 'supersedes' ? 'sw-strong' : 'sw-edge'}" viewBox="0 0 20 10" aria-hidden="true">
          <line x1="1" y1="5" x2="19" y2="5" />
        </svg>
        {opt}
      {:else}
        <svg class="sw sw-structural" viewBox="0 0 20 10" aria-hidden="true">
          <path d="M1 8 Q 9 0 16 4" />
          <path d="M12 2 L 16 4 L 12 7" />
        </svg>
        {opt}<span class="sr-only"> structural — deterministic reference</span>
      {/if}
    {/snippet}
  </MultiSelect>

  {#if categoryOptions.length > 0}
    <MultiSelect
      label="category"
      options={categoryOptions}
      checked={(cat) => categoryChecked(filters.categories, cat)}
      ontoggle={onCategoryToggle}
      onall={categoriesAll}
      ononly={categoryOnly}
    />
  {/if}

  <fieldset>
    <legend>min confidence (dream)</legend>
    <!-- GC3: fieldset legends give inputs NO accessible name — the three
         controls below carry explicit aria-labels (first axe pass over the
         focus stage surfaced them as pre-existing label violations). -->
    <input
      class="conf"
      type="number"
      min="0"
      max="1"
      step="0.05"
      inputmode="decimal"
      aria-label="min confidence (dream)"
      value={filters.minConfidence}
      oninput={(e) => onchange({ ...filters, minConfidence: Number(e.currentTarget.value) || 0 })}
    />
  </fieldset>

  <fieldset>
    <legend>created</legend>
    <!-- Cross-bounds: each picker excludes dates the other side already rules
         out — an empty-by-construction range is not selectable. -->
    <input
      type="date"
      aria-label="created after"
      max={filters.createdBefore || undefined}
      value={filters.createdAfter}
      oninput={(e) => onchange({ ...filters, createdAfter: e.currentTarget.value })}
    />
    <span class="sep">–</span>
    <input
      type="date"
      aria-label="created before"
      min={filters.createdAfter || undefined}
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
    align-items: center;
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

  /* Legend swatches (GC3): token-bound stroke = the exact canvas colors.
     Rendered inside the MultiSelect popup rows via the option snippet —
     snippet content keeps THIS component's style scope. */
  .sw {
    width: 20px;
    height: 10px;
    flex: none;
  }
  .sw line,
  .sw path {
    fill: none;
    stroke-width: 1.5;
  }
  .sw-edge {
    stroke: var(--graph-edge);
  }
  .sw-strong {
    stroke: var(--graph-edge-strong);
  }
  .sw-structural {
    stroke: var(--graph-edge-structural);
  }

  /* Screen-reader-only semantics suffix (visually hidden, still in the
     accessible name) — same pattern as BoardPage .sr-only. */
  .sr-only {
    position: absolute;
    width: 1px;
    height: 1px;
    margin: -1px;
    padding: 0;
    overflow: hidden;
    clip: rect(0 0 0 0);
    white-space: nowrap;
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
