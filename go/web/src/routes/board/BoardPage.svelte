<script lang="ts">
  // Board surface (design 04 §4.1/§4.2/§5.5, wave U07). The U04 dark-launch
  // scaffold becomes the real read-only kanban: status columns straight from the
  // board wire (order == wire order == the type-config status order, NEVER
  // hardcoded), the wire count per column, per-column keyset windowing, and the
  // closed-collapse driven by the registry workflow.terminal set (board-columns.ts
  // joins the board wire with GET /api/types — the board wire carries no
  // category). Runs in the `board` layout mode (full-bleed + horizontal scroll,
  // AppShell §4.1.2). API only through the U03 client.
  //
  // Read-only in U07 (§5.3): no drop targets, no card grip — the drag-and-drop
  // status transition is U08. Scope is URL-carried (?scope=), the 0/1/N picker is
  // shared with /issues.
  //
  // NOTE: comments here avoid quote pairs / backticks — svelte2tsx scans the
  // script block for string literals without honouring line comments, so a
  // quoted word can swallow the closing script tag (false positive; the real
  // compiler is unaffected).
  import { onMount } from 'svelte'
  import EmptyState from '../../lib/ui/EmptyState.svelte'
  import ProjectPicker from '../issues/ProjectPicker.svelte'
  import Column from './Column.svelte'
  import { listProjects } from '../../lib/api/issues'
  import type { ProjectRow } from '../../lib/api/types'
  import type { ResourceStatus } from '../../lib/resource.svelte'
  import { BoardModel } from './board.svelte'
  import { parseScopeParam } from '../issues/scope-param'
  import { projectForScope, resolveScope } from '../issues/picker'

  let scope = $state<string | null>(parseScopeParam(location.search))
  let projects = $state<ProjectRow[]>([])
  let projectsStatus = $state<ResourceStatus>('idle')
  let model = $state<BoardModel | null>(null)

  const activeProject = $derived(projectForScope(projects, scope))

  /** Scope → URL (replaceState, sv-router-safe: no navigation event, §4.2). */
  function writeUrl(): void {
    const q = scope ? `?scope=${encodeURIComponent(scope)}` : ''
    history.replaceState(history.state, '', `${location.pathname}${q}`)
  }

  /** Resolve the active scope (URL is the single truth), (re)build the model for
   * its project, load the board + registry. */
  async function activate(): Promise<void> {
    const resolved = resolveScope(projects, scope)
    if (resolved !== scope) {
      scope = resolved
      writeUrl()
    }
    const proj = projectForScope(projects, resolved)
    if (proj === null) {
      model = null
      return
    }
    model = new BoardModel(proj.id)
    await model.load()
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

  function onPick(next: string): void {
    scope = next
    writeUrl()
    void activate() // new project ⇒ new model
  }
</script>

<section class="area" aria-label="Board">
  <header>
    <h1>Board</h1>
    <ProjectPicker {projects} selected={scope} onselect={onPick} />
  </header>

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
      <EmptyState title="Select a project" copy="Choose a project above to view its board." />
    {:else if model === null || model.status === 'loading'}
      <p class="state" aria-busy="true">loading…</p>
    {:else if model.status === 'error'}
      <div class="error" role="alert">
        <p>{model.loadError?.message}</p>
        {#if model.loadError?.requestId}<p class="request-id">request {model.loadError.requestId}</p>{/if}
      </div>
    {:else if model.columns.length === 0}
      <EmptyState
        title="No board to show"
        copy="This project's type has no workflow statuses, so there is nothing to lay out."
      />
    {:else}
      <div class="board" role="group" aria-label="Board columns">
        {#each model.columns as column (column.status)}
          <Column
            {column}
            {scope}
            collapsed={model.isCollapsed(column.status)}
            loadingMore={model.loadingMore[column.status] === true}
            canLoadMore={model.canLoadMore(column.status)}
            ontoggle={() => model?.toggle(column.status)}
            onloadmore={() => model?.loadMore(column.status)}
          />
        {/each}
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
    flex: 0 0 auto;
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
  }
  /* Full-bleed board: columns lay out in a row, the board scrolls horizontally
     (the `board` layout mode gives it the full viewport width, AppShell §4.1.2);
     each column scrolls its own cards vertically. */
  .board {
    flex: 1 1 auto;
    min-height: 0;
    display: flex;
    gap: var(--space-3);
    overflow-x: auto;
    align-items: flex-start;
    padding-bottom: var(--space-2);
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
