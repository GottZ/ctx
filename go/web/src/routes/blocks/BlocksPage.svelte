<script lang="ts">
  import { onMount } from 'svelte'
  import FilterPanel from './FilterPanel.svelte'
  import { scopeVisible, type BlockFilters } from '../../lib/blocks/filters'
  import { BlocksModel } from './blocks.svelte'

  // All list/search/filter state lives in the injectable model (block-workbench
  // W1/W2). This component stays thin: a search box, the facet panel, the hit
  // list, the three load states. Read-only — detail viewing/editing arrive in
  // W3/W4.
  const model = new BlocksModel()

  // Local input mirror — submitting commits the query into the one filter
  // state; clearing it collapses to the empty-query default (newest-first).
  let input = $state('')

  onMount(() => {
    void model.load()
    void model.loadCategories()
  })

  // Distinct scopes present in the loaded results — the client-side scope
  // chips. Derived from what is visible, never a request param.
  const scopeOptions = $derived([...new Set(model.results.map((r) => r.scope))].sort())

  // The rendered list, narrowed by the client-side scope facet (the server
  // never sees a scope param — it gates visibility through the auth key).
  const visible = $derived(model.results.filter((r) => scopeVisible(r, model.filters)))

  async function submit(event: SubmitEvent): Promise<void> {
    event.preventDefault()
    await model.setFilters({ ...model.filters, query: input.trim() })
  }

  async function applyFilters(next: BlockFilters): Promise<void> {
    // The query lives in the filter state too — keep the search box in sync
    // when the panel (e.g. its reset button) rewrites it.
    input = next.query
    await model.setFilters(next)
  }

  /** Per-hit pick — a light hook for the W3 detail viewer (no panel yet). */
  function select(id: string): void {
    void id
  }
</script>

<section class="area">
  <header>
    <h1>Blocks</h1>
    <p class="sub">
      browse the corpus — empty search shows the newest blocks; a query runs full-text search
      (stemmed by keyword, substrings/typos don't match). Read-only.
    </p>
  </header>

  <form class="search" onsubmit={submit}>
    <input
      type="search"
      placeholder="search blocks by keyword — leave empty for the newest"
      spellcheck="false"
      bind:value={input}
    />
    <button type="submit" disabled={model.status === 'loading'}>
      {model.status === 'loading' ? '…' : 'Search'}
    </button>
  </form>

  <FilterPanel
    filters={model.filters}
    categories={model.categories}
    scopes={scopeOptions}
    onchange={applyFilters}
  />

  {#if model.status === 'loading'}
    <p class="state" aria-busy="true">loading…</p>
  {:else if model.status === 'error'}
    <div class="error" role="alert">
      <p>{model.loadError?.message}</p>
      {#if model.loadError?.requestId}
        <p class="request-id">request {model.loadError.requestId}</p>
      {/if}
    </div>
  {:else if visible.length === 0}
    <p class="empty" role="status">
      {#if model.results.length > 0}
        no block in the selected scope — clear the scope filter to see all
      {:else if model.query === ''}
        no blocks visible to this key
      {:else}
        no full-text match — words are stemmed, substrings don’t match
      {/if}
    </p>
  {:else}
    <ul class="results">
      {#each visible as r (r.id)}
        <li>
          <button type="button" onclick={() => select(r.id)}>
            <span class="row">
              <span class="title">{r.title}</span>
              <time class="updated" datetime={r.updated_at}>{r.updated_at.slice(0, 10)}</time>
            </span>
            <span class="meta">
              {r.category} · {r.scope}{r.tags.length > 0 ? ` · ${r.tags.join(', ')}` : ''}
            </span>
            <span class="preview">{r.content_preview}</span>
            <span class="len">{r.content_length} chars</span>
          </button>
        </li>
      {/each}
    </ul>
  {/if}
</section>

<style>
  .area {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }

  header {
    border-bottom: 1px solid var(--border);
    padding-bottom: var(--space-2);
  }
  h1 {
    margin: 0;
    font-size: 1.35rem;
    font-weight: 600;
    letter-spacing: 0.01em;
  }
  .sub {
    margin: var(--space-1) 0 0;
    color: var(--text-dim);
    font-size: 0.85rem;
  }

  .search {
    display: flex;
    gap: var(--space-2);
  }
  .search input {
    flex: 1;
    min-width: 0;
    font-family: var(--font-mono);
    font-size: 0.85rem;
  }

  .state {
    margin: 0;
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: 0.85rem;
  }
  .empty {
    margin: 0;
    color: var(--text-faint);
    font-size: 0.85rem;
  }

  .error {
    border: 1px solid var(--danger);
    border-radius: var(--radius);
    background: var(--danger-dim);
    padding: var(--space-2) var(--space-3);
    font-size: 0.85rem;
  }
  .error p {
    margin: 0;
    color: var(--danger);
  }
  .request-id {
    font-family: var(--font-mono);
    font-size: 0.75rem;
    color: var(--text-dim) !important;
  }

  .results {
    list-style: none;
    margin: 0;
    padding: 0;
    border: 1px solid var(--border);
    border-radius: var(--radius);
    background: var(--surface-1);
  }
  .results li + li {
    border-top: 1px solid var(--border);
  }
  .results button {
    display: flex;
    flex-direction: column;
    gap: 0.2rem;
    width: 100%;
    text-align: left;
    background: transparent;
    border: none;
    border-radius: 0;
    padding: var(--space-2) var(--space-3);
  }
  .results button:hover {
    background: var(--surface-2);
  }

  .row {
    display: flex;
    align-items: baseline;
    gap: var(--space-3);
  }
  .title {
    flex: 1;
    font-size: 0.95rem;
    color: var(--text);
  }
  .updated {
    font-family: var(--font-mono);
    font-size: 0.75rem;
    color: var(--text-faint);
    white-space: nowrap;
  }
  .meta {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-faint);
  }
  .preview {
    font-size: 0.8rem;
    color: var(--text-dim);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 100%;
  }
  .len {
    font-family: var(--font-mono);
    font-size: 0.7rem;
    color: var(--text-faint);
  }
</style>
