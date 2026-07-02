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
  import { canEdit, editInitialFromDetail, type BlockDraft } from '../../lib/blocks/edit'
  import { sensitivityBadge } from '../../lib/blocks/sensitivity'
  import { session } from '../../lib/auth.svelte'
  import EmptyState from '../../lib/ui/EmptyState.svelte'

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
    // Seed the dialog from the loaded detail — crucially carrying the block's
    // CURRENT sensitivity (W6), so lowering it in the dialog is recognised as a
    // downgrade and the confirm gate becomes reachable from the page. (Before W6
    // the page hard-seeded '' and the downgrade flow was unreachable.)
    editInitial = { ...editInitialFromDetail(b) }
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
        {#if model.results.length > 0}
          <EmptyState
            title="No blocks in this scope"
            copy="Nothing matches the selected scope filter — clear it to see every block visible to this key."
          />
        {:else if model.query === ''}
          {#if homeScope !== ''}
            <EmptyState
              title="No blocks yet"
              copy="Your corpus is empty. Store your first block to start building the knowledge base."
            >
              {#snippet cta()}
                <button type="button" class="empty-cta" onclick={openCreate}>Store your first block</button>
              {/snippet}
            </EmptyState>
          {:else}
            <EmptyState title="No blocks yet" copy="No blocks are visible to this read-only key yet." />
          {/if}
        {:else}
          <EmptyState
            title="No matches"
            copy="No full-text match — words are stemmed, so substrings and typos don’t match. Try a different keyword."
          />
        {/if}
      {:else}
        <ul class="results">
          {#each visible as r (r.id)}
            {@const sb = sensitivityBadge(r.sensitivity ?? '')}
            <li>
              <button type="button" class:active={detail.openId === r.id} onclick={() => select(r.id)}>
                <span class="row">
                  <span class="title">{r.title}</span>
                  <time class="updated" datetime={r.updated_at}>{r.updated_at.slice(0, 10)}</time>
                </span>
                <span class="meta">
                  <span class="badge {sb.tone}" title="sensitivity">{sb.label}</span>
                  {r.category} · {r.scope}{r.tags.length > 0 ? ` · ${r.tags.join(', ')}` : ''}
                </span>
                <span class="preview">{r.content_preview}</span>
                <span class="len">{r.content_length} chars</span>
              </button>
            </li>
          {/each}
        </ul>

        {#if model.query === ''}
          <!-- Newest-first (browse) mode: real keyset pagination (W7). The
               button appears only while the server handed back a next cursor;
               a null cursor means the last page was reached. -->
          {#if model.nextCursor !== null}
            <button
              type="button"
              class="load-more"
              disabled={model.loadingMore}
              onclick={() => model.loadMore()}
            >
              {model.loadingMore ? 'loading…' : 'Load more'}
            </button>
          {/if}
        {:else}
          <!-- FTS mode is LIMIT-only "top matches" (ts_rank_cd ties make a
               keyset fragile/slow) — no Load more; refine the query instead. -->
          <p class="fts-hint" role="status">top matches — refine the query to narrow</p>
        {/if}
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
  /* split mode (design 01 §4 S6 + Content-Region-Contract). AppShell renders
     this .area inside .content[data-mode=split], a definite-height grid cell;
     height:100% pins .area to it so the WHOLE region never page-scrolls. The
     column flex hands the leftover height (after header/search/filter keep their
     intrinsic height) to .workbench. container-type makes .area the query
     container for the ≤sm stack (region-, not viewport-relative → holds with the
     rail open or closed; BlockDetail's @container resolves against this too). */
  .area {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    height: 100%;
    min-height: 0;
    container-type: inline-size;
  }

  /* Master-detail: hit list (master) + BlockDetail (.detail-pane) side by side,
     each with its OWN scroll chain. flex:1+min-height:0 lets the row shrink
     below its content; align-items:stretch sizes both panes to the row height so
     their overflow scrolls in place — 1M-corpus: list scrolls independently,
     detail scrolls independently, no page scroll, no clipping. */
  .workbench {
    display: flex;
    align-items: stretch;
    gap: var(--space-3);
    flex: 1;
    min-height: 0;
  }
  .list {
    flex: 1;
    min-width: 0;
    overflow-y: auto;
  }
  .workbench > :global(.detail-pane) {
    overflow-y: auto;
  }

  /* @375px (container ≤ sm / 40rem, the breakpoints.ts SM literal) → stack. The
     two panes collapse into one scrolling column (the workbench owns the single
     scroll; the panes flow) so the cramped side-by-side never reaches a phone. */
  @container (max-width: 40rem) {
    .workbench {
      flex-direction: column;
      overflow-y: auto;
    }
    .list {
      overflow-y: visible;
    }
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
    font-size: var(--fs-xl);
    font-weight: var(--fw-semibold);
    letter-spacing: var(--track-1);
  }
  .new {
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-3);
  }
  .sub {
    margin: var(--space-1) 0 0;
    color: var(--text-dim);
    font-size: var(--fs-sm);
  }

  .search {
    display: flex;
    gap: var(--space-2);
  }
  .search input {
    flex: 1;
    min-width: 0;
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
  }

  .state {
    margin: 0;
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
  }
  /* N8 empty-state CTA — "store your first block" on a fresh, writable corpus.
     Inherits the global button chrome (like .new), just sized for the panel. */
  .empty-cta {
    font-size: var(--fs-sm);
    padding: var(--space-2) var(--space-3);
  }

  /* W7 "Load more" — newest-first keyset pagination. */
  .load-more {
    width: 100%;
    margin-top: var(--space-2);
    font-size: var(--fs-sm);
    padding: var(--space-2) var(--space-3);
  }
  /* FTS "top matches" hint — no pagination on the ranked path. */
  .fts-hint {
    margin: var(--space-2) 0 0;
    text-align: center;
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
  }

  .error {
    border: 1px solid var(--danger);
    border-radius: var(--radius);
    background: var(--danger-dim);
    padding: var(--space-2) var(--space-3);
    font-size: var(--fs-sm);
  }
  .error p {
    margin: 0;
    color: var(--danger);
  }
  .request-id {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
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
    font-size: var(--fs-md);
    color: var(--text);
  }
  .updated {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    color: var(--text-faint);
    white-space: nowrap;
  }
  .meta {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-1);
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-faint);
  }

  /* Sensitivity badge (W6) — tone is a token-driven CSS-class suffix
     (BackendBadge pattern); colours come only from styles/tokens.css. */
  .badge {
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
    color: var(--text-dim);
  }
  .badge.danger {
    border-color: var(--danger);
    color: var(--danger);
  }
  .badge.warn {
    border-color: var(--warn);
    color: var(--warn);
  }
  .badge.accent {
    border-color: var(--accent);
    color: var(--accent);
  }
  .badge.ok {
    border-color: var(--ok);
    color: var(--ok);
  }
  .preview {
    font-size: var(--fs-sm);
    color: var(--text-dim);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 100%;
  }
  .len {
    font-family: var(--font-mono);
    font-size: var(--fs-2xs);
    color: var(--text-faint);
  }
</style>
