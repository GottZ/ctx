<script lang="ts">
  import { onMount } from 'svelte'
  import FilterPanel from './FilterPanel.svelte'
  import BlockDetail from './BlockDetail.svelte'
  import BlockDialog from './BlockDialog.svelte'
  import { scopeVisible, type BlockFilters } from '../../lib/blocks/filters'
  import { BlocksModel } from './blocks.svelte'
  import { BlockDetailModel } from './detail.svelte'
  import { BlockEditModel, type EditMode } from './edit.svelte'
  import { BlockDeleteModel } from './delete.svelte'
  import { canEdit, type BlockDraft } from '../../lib/blocks/edit'
  import { session } from '../../lib/auth.svelte'

  // All list/search/filter state lives in the injectable model (block-workbench
  // W1/W2). This component stays thin: a search box, the facet panel, the hit
  // list, the three load states. The detail viewer (W3) is a sidebar panel; W4
  // adds create + edit through BlockDialog (delete/sensitivity-badge are W5/W6).
  const model = new BlocksModel()
  const detail = new BlockDetailModel()

  const homeScope = $derived(session.whoami?.home_scope ?? '')
  // The open block is editable only when it lives in the caller's home_scope —
  // a foreign-scope hit reachable via read_scopes is read-only (write is
  // scope-gated server-side; the dialog/edit affordance simply isn't offered).
  const editable = $derived(detail.block !== null && canEdit(detail.block.scope, homeScope))

  // After a successful save: reload the list (current filters) and, if a detail
  // is open, re-fetch it so edits show immediately.
  const editModel = new BlockEditModel(undefined, async () => {
    await model.setFilters(model.filters)
    if (detail.openId !== null) await detail.load(detail.openId)
  })

  // Delete (W5): on success the panel closes (the block is gone) and the list
  // reloads — no detail.load, the block no longer exists. close = detail.close
  // (panel away), reload = re-run the list with the current filters.
  const deleteModel = new BlockDeleteModel(
    undefined,
    () => model.setFilters(model.filters),
    () => detail.close(),
  )

  // The dialog is mounted only while editing; its mode + seed snapshot are held
  // here. `editId` is the FULL block UUID (the server resolves ids in HomeScope
  // only — a prefix would be ambiguous on write).
  let dialogOpen = $state(false)
  let dialogMode = $state<EditMode>('create')
  let editId = $state('')
  let editInitial = $state<Partial<BlockDraft> | undefined>(undefined)

  // Local input mirror — submitting commits the query into the one filter
  // state; clearing it collapses to the empty-query default (newest-first).
  let input = $state('')

  onMount(() => {
    void model.load()
    void model.loadCategories()
  })

  function openCreate(): void {
    dialogMode = 'create'
    editId = ''
    editInitial = { scope: homeScope }
    dialogOpen = true
  }

  function openEdit(): void {
    const b = detail.block
    if (b === null || !canEdit(b.scope, homeScope)) return
    dialogMode = 'edit'
    editId = b.id
    editInitial = {
      category: b.category,
      title: b.title,
      content: b.content,
      tags: b.tags,
      scope: b.scope,
      // The detail block carries no sensitivity field (W6) — leave it unset so
      // an unrelated edit never re-classifies; the user picks one to change it.
      sensitivity: '',
    }
    dialogOpen = true
  }

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

  /** Per-hit pick — lazy-loads the full block into the W3 detail sidebar. */
  function select(id: string): void {
    void detail.load(id)
  }
</script>

<section class="area">
  <header>
    <div class="title-row">
      <h1>Blocks</h1>
      {#if homeScope !== ''}
        <button type="button" class="new" onclick={openCreate}>New block</button>
      {/if}
    </div>
    <p class="sub">
      browse the corpus — empty search shows the newest blocks; a query runs full-text search
      (stemmed by keyword, substrings/typos don't match).
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

  <div class="workbench">
    <div class="list">
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
              <button type="button" class:active={detail.openId === r.id} onclick={() => select(r.id)}>
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
    </div>

    {#if detail.openId !== null}
      <BlockDetail
        model={detail}
        onedit={editable ? openEdit : undefined}
        deleteModel={editable ? deleteModel : undefined}
      />
    {/if}
  </div>
</section>

{#if dialogOpen}
  <BlockDialog
    mode={dialogMode}
    model={editModel}
    id={editId}
    initial={editInitial}
    onclose={() => (dialogOpen = false)}
  />
{/if}

<style>
  .area {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }

  .workbench {
    display: flex;
    align-items: flex-start;
    gap: var(--space-3);
  }
  .list {
    flex: 1;
    min-width: 0;
  }

  header {
    border-bottom: 1px solid var(--border);
    padding-bottom: var(--space-2);
  }
  .title-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
  }
  h1 {
    margin: 0;
    font-size: 1.35rem;
    font-weight: 600;
    letter-spacing: 0.01em;
  }
  .new {
    font-size: 0.8rem;
    padding: var(--space-1) var(--space-3);
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
  .results button.active {
    background: var(--accent-dim);
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
