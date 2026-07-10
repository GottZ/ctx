<script lang="ts">
  // Issue-list surface (design 04 §4.1/§4.2/§5.5, wave U05). The dark-launch
  // scaffold (U04) becomes the real list: virtua-style fixed-height windowing
  // over the Q10 Table primitive (virtual-window.ts — no library, the U05 gate
  // forbids a new dep), server filters (status/search), URL-carried filter state
  // INCL. ?scope=, keyset append (cap 50k), the search-mode Top-N (no append),
  // and the 0/1/N ProjectPicker. API only through the U03 client (issues.ts).
  //
  // Read-only in U05 (fail-closed default, §5.3): the W6 list wire carries no
  // writable field, so no create/write affordance is offered here — the write
  // path (create/mutate, with a real writable signal) lands in U06. Writable-
  // unknown maps to read-only (the §5.3 default), which makes the §5.5 no-create-
  // button assert hold by construction.
  //
  // NOTE: comments in this script block avoid backticks and quote pairs —
  // svelte2tsx (svelte-check) scans the block for string/template literals
  // without honouring line comments, so a quoted word swallows the closing
  // script tag and reports script-left-open (the real compiler is unaffected).
  import { onMount, onDestroy } from 'svelte'
  import EmptyState from '../../lib/ui/EmptyState.svelte'
  import Table from '../../lib/ui/Table.svelte'
  import ProjectPicker from './ProjectPicker.svelte'
  import FilterBar from './FilterBar.svelte'
  import { listProjects } from '../../lib/api/issues'
  import type { ProjectRow } from '../../lib/api/types'
  import type { ResourceStatus } from '../../lib/resource.svelte'
  import { IssuesModel, type IssueQuery } from './issues.svelte'
  import { parseIssueFilters, issueFiltersToQuery, isSearchMode, type IssueFilters } from './issue-filters'
  import { projectForScope, resolveScope } from './picker'
  import { computeWindow, isNearBottom } from '../../lib/ui/virtual-window'
  import { LiveSource } from '../../lib/workflow/live'

  // Fixed row geometry (§4.3): the constant that makes the window pure math.
  // MUST match the .issue-row height in the style block (44px) — the spacer
  // heights are computed from it, a mismatch would drift the scroll geometry.
  const ROW_H = 44
  const OVERSCAN = 8
  const NEAR_BOTTOM_PX = ROW_H * 6

  let filters = $state<IssueFilters>(parseIssueFilters(location.search))
  let projects = $state<ProjectRow[]>([])
  let projectsStatus = $state<ResourceStatus>('idle')
  let model = $state<IssuesModel | null>(null)
  let live = $state<LiveSource | null>(null)

  let scroller = $state<HTMLElement | null>(null)
  let scrollTop = $state(0)
  let viewportH = $state(0)

  const rows = $derived(model?.rows ?? [])
  const win = $derived(
    computeWindow({ scrollTop, viewportHeight: viewportH, rowHeight: ROW_H, total: rows.length, overscan: OVERSCAN }),
  )
  const windowRows = $derived(rows.slice(win.start, win.end))
  const searchMode = $derived(isSearchMode(filters))
  // Status options: the statuses present in the loaded rows, plus the active
  // filter (so a deep-linked status stays selectable before its rows load).
  const statusOptions = $derived(
    [...new Set(rows.map((r) => r.workflow_status).concat(filters.status ? [filters.status] : []))]
      .filter((s) => s !== '')
      .sort(),
  )
  const activeProject = $derived(projectForScope(projects, filters.scope))

  function queryFrom(f: IssueFilters): IssueQuery {
    return { state: f.status ?? undefined, labels: f.labels, q: f.q ?? undefined }
  }

  /** Detail deep link — carries the scope so /issues/:id resolves the project
   * (the other filters are list-only, dropped on the detail URL). */
  function detailHref(id: string): string {
    return `/issues/${id}${issueFiltersToQuery({ scope: filters.scope, status: null, q: null, type: null, labels: [] })}`
  }

  /** Filters → URL (replaceState, sv-router-safe: no navigation event, §4.2). */
  function writeUrl(): void {
    history.replaceState(history.state, '', `${location.pathname}${issueFiltersToQuery(filters)}`)
  }

  function resetScroll(): void {
    scrollTop = 0
    if (scroller) scroller.scrollTop = 0
  }

  /** Resolve the active scope, (re)build the model for its project, load page 1. */
  async function activate(): Promise<void> {
    const scope = resolveScope(projects, filters.scope)
    if (scope !== filters.scope) {
      filters = { ...filters, scope }
      writeUrl()
    }
    const proj = projectForScope(projects, scope)
    if (proj === null) {
      model = null
      live?.stop()
      live = null
      return
    }
    model = new IssuesModel(proj.id)
    startLive(proj.id)
    resetScroll()
    await model.load(queryFrom(filters))
  }

  /** (Re)open the SSE live stream for the active project; a project frame drives a
   * head reload, a bulk/resync or a poll tick reloads the whole page. Poll stays
   * the permanent fallback (design 04 §7-U13). */
  function startLive(projectId: string): void {
    live?.stop()
    live = new LiveSource({
      projectId,
      // Auth fährt als httpOnly-Session-Cookie mit (OAuth R4); der GET-Stream
      // braucht weder Bearer noch CSRF.
      onBatch: () => {
        if (model !== null) void model.load(queryFrom(filters))
      },
    })
    live.start()
  }

  onDestroy(() => {
    live?.stop()
    live = null
  })

  /** Reload the CURRENT project list with the current filters (query change). */
  async function reloadCurrent(): Promise<void> {
    if (model === null) return
    resetScroll()
    await model.load(queryFrom(filters))
  }

  onMount(async () => {
    projectsStatus = 'loading'
    try {
      const res = await listProjects()
      projects = res.projects
      projectsStatus = 'ready'
    } catch {
      projectsStatus = 'error'
    }
    await activate()
  })

  function onSearch(q: string): void {
    filters = { ...filters, q: q === '' ? null : q }
    writeUrl()
    void reloadCurrent()
  }

  function onStatus(status: string): void {
    filters = { ...filters, status: status === '' ? null : status }
    writeUrl()
    void reloadCurrent()
  }

  function onPick(scope: string): void {
    filters = { ...filters, scope }
    writeUrl()
    void activate() // new project ⇒ new model
  }

  function onScroll(): void {
    if (!scroller) return
    scrollTop = scroller.scrollTop
    if (
      model?.canLoadMore &&
      !model.loadingMore &&
      isNearBottom(scroller.scrollTop, scroller.clientHeight, scroller.scrollHeight, NEAR_BOTTOM_PX)
    ) {
      void model.loadMore()
    }
  }
</script>

<section class="area" aria-label="Issues">
  <header>
    <h1>Issues</h1>
    <ProjectPicker {projects} selected={filters.scope} onselect={onPick} />
  </header>

  {#if activeProject !== null}
    <FilterBar q={filters.q ?? ''} status={filters.status ?? ''} {statusOptions} onsearch={onSearch} onstatus={onStatus} />
  {/if}

  <div class="body">
    {#if projectsStatus === 'loading'}
      <p class="state" aria-busy="true">loading…</p>
    {:else if projectsStatus === 'error'}
      <div class="error" role="alert"><p>Could not load your projects.</p></div>
    {:else if projects.length === 0}
      <EmptyState
        title="No projects yet"
        copy="Provision a project via the /admin wizard or `ctx project init` to start tracking issues."
      />
    {:else if activeProject === null}
      <EmptyState title="Select a project" copy="Choose a project above to view its issues." />
    {:else if model === null || model.status === 'loading'}
      <p class="state" aria-busy="true">loading…</p>
    {:else if model.status === 'error'}
      <div class="error" role="alert">
        <p>{model.loadError?.message}</p>
        {#if model.loadError?.requestId}<p class="request-id">request {model.loadError.requestId}</p>{/if}
      </div>
    {:else if rows.length === 0}
      {#if searchMode}
        <EmptyState title="No matches" copy="No issue matches this search. Try a different query or clear it to browse." />
      {:else}
        <EmptyState title="No issues yet" copy="This project has no issues in the selected filter." />
      {/if}
    {:else}
      <div class="list" bind:this={scroller} bind:clientHeight={viewportH} onscroll={onScroll}>
        <Table label="Issues" valign="baseline">
          {#snippet head()}
            <tr>
              <th>Title</th>
              <th>Status</th>
              <th class="updated-col">Updated</th>
            </tr>
          {/snippet}
          {#snippet children()}
            {#if win.padTop > 0}
              <tr class="spacer" aria-hidden="true"><td colspan="3" style="height: {win.padTop}px"></td></tr>
            {/if}
            {#each windowRows as r (r.id)}
              <tr class="issue-row" data-issue-row>
                <td class="title">
                  <a href={detailHref(r.id)}>{r.title}</a>
                </td>
                <td class="status"><span class="badge">{r.workflow_status}</span></td>
                <td class="updated-col"><time datetime={r.updated_at}>{r.updated_at.slice(0, 10)}</time></td>
              </tr>
            {/each}
            {#if win.padBottom > 0}
              <tr class="spacer" aria-hidden="true"><td colspan="3" style="height: {win.padBottom}px"></td></tr>
            {/if}
          {/snippet}
        </Table>

        <div class="footer">
          {#if searchMode}
            <p class="hint" role="status">Top matches — refine the query to narrow.</p>
          {:else if model.capped}
            <p class="hint" role="status">Showing the first 50,000 issues — refine the filter to see more.</p>
          {:else if model.canLoadMore}
            <button type="button" class="load-more" disabled={model.loadingMore} onclick={() => model?.loadMore()}>
              {model.loadingMore ? 'loading…' : 'Load more'}
            </button>
          {/if}
        </div>
      </div>
    {/if}
  </div>
</section>

<style>
  .area {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    height: 100%;
    min-height: 0;
    box-sizing: border-box;
    padding: var(--space-4) var(--shell-gutter);
  }
  header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    border-bottom: 1px solid var(--border);
    padding-bottom: var(--space-2);
  }
  h1 {
    margin: 0;
    font-size: var(--fs-xl);
    font-weight: var(--fw-semibold);
    letter-spacing: var(--track-1);
  }
  .body {
    flex: 1 1 auto;
    min-height: 0;
    display: flex;
    flex-direction: column;
  }
  /* The vertical scroll container the windowing measures — a definite-height
     flex child so 10k rows scroll IN PLACE, never the page (design 04 §6.1). */
  .list {
    flex: 1 1 auto;
    min-height: 0;
    overflow-y: auto;
  }
  /* Fixed row height — the constant ROW_H (44) mirrors here; the spacer <tr>s
     stand in for the off-window rows so the scrollbar geometry stays honest. */
  .issue-row {
    height: 44px;
  }
  .issue-row .title a {
    color: var(--text);
    text-decoration: none;
  }
  .issue-row .title a:hover {
    color: var(--accent);
    text-decoration: underline;
  }
  .badge {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    padding: var(--space-0) var(--space-2);
    background: var(--surface-2);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    color: var(--text-dim);
  }
  .updated-col {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    color: var(--text-faint);
    white-space: nowrap;
  }
  .footer {
    padding: var(--space-3) 0;
  }
  .load-more {
    width: 100%;
  }
  .hint {
    margin: 0;
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    text-align: center;
  }
  .state {
    margin: 0;
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
  }
  .error {
    color: var(--danger);
    font-size: var(--fs-sm);
  }
  .request-id {
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
  }
</style>
