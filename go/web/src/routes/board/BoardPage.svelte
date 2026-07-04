<script lang="ts">
  // Board surface (design 04 §4.1/§4.2/§4.5/§5.5, waves U07 + U08). Status columns
  // straight from the board wire (order == wire order == the type-config status
  // order, NEVER hardcoded), the wire count per column, per-column keyset
  // windowing, and the closed-collapse driven by the registry workflow.terminal
  // set (board-columns.ts joins the board wire with GET /api/types). Runs in the
  // `board` layout mode (full-bleed + horizontal scroll). API only through the
  // U03 client.
  //
  // U08 adds the WRITE path, gated fail-closed on the caller write-scope politik
  // (§5.3, N3 — canWriteScope over the active project scope): a writable board
  // makes cards draggable (pragmatic-drag-and-drop via the §4.5 adapter) and a
  // drop is an OPTIMISTIC status transition (card moves at once → PATCH → server
  // confirms, or a 409/422 rolls back + re-reads the wire). The mouse-free path
  // is the Move dialog (focus a card Move button → pick a target column). A
  // read-only board (writable:false) is exactly the U07 board: no drop targets,
  // no grips, no Move affordance.
  //
  // NOTE: comments here avoid quote pairs / backticks — svelte2tsx scans the
  // script block for string literals without honouring line comments, so a
  // quoted word can swallow the closing script tag (false positive; the real
  // compiler is unaffected).
  import { onMount } from 'svelte'
  import EmptyState from '../../lib/ui/EmptyState.svelte'
  import Modal from '../../lib/ui/Modal.svelte'
  import ProjectPicker from '../issues/ProjectPicker.svelte'
  import Column from './Column.svelte'
  import { listProjects } from '../../lib/api/issues'
  import type { ProjectRow } from '../../lib/api/types'
  import type { ResourceStatus } from '../../lib/resource.svelte'
  import { BoardModel } from './board.svelte'
  import { parseScopeParam } from '../issues/scope-param'
  import { projectForScope, resolveScope } from '../issues/picker'
  import { session } from '../../lib/auth.svelte'
  import { canWriteScope } from '../../lib/workflow/writable'
  import { createBoardDnd, type BoardDndAdapter } from '../../lib/board/dnd'

  let scope = $state<string | null>(parseScopeParam(location.search))
  let projects = $state<ProjectRow[]>([])
  let projectsStatus = $state<ResourceStatus>('idle')
  let model = $state<BoardModel | null>(null)
  let dnd = $state<BoardDndAdapter | null>(null)
  let announcement = $state('')

  // Keyboard Move dialog (§4.5 keyboard path): a card raises onmove, the page
  // opens this dialog with the droppable target columns for that card.
  let moveDialog = $state<{ issueId: string; from: string; title: string; targets: string[] } | null>(null)

  const activeProject = $derived(projectForScope(projects, scope))

  // Write gate (§5.3, fail-closed): derived from the caller write-scope politik
  // over the active project scope (N3). No project ⇒ not writable.
  const writable = $derived(
    activeProject !== null &&
      canWriteScope(activeProject.scope, { homeScope: session.homeScope, readScopes: session.readScopes }),
  )

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

  // One DnD adapter per mounted board; the drop callback drives the optimistic
  // transition. The monitor is inert while no card is a drag source (read-only),
  // so it is created unconditionally and torn down on unmount.
  onMount(() => {
    const adapter = createBoardDnd()
    adapter.ondragstart((_id, from) => {
      announcement = `Picked up issue from ${from}. Drop on a column to change its status.`
    })
    adapter.ondrop((issueId, from, to) => {
      void doTransition(issueId, from, to)
    })
    dnd = adapter
    return () => {
      adapter.destroy()
      dnd = null
    }
  })

  function onPick(next: string): void {
    scope = next
    writeUrl()
    void activate() // new project ⇒ new model
  }

  /** Run one optimistic transition + announce the outcome for the live region
   * (the model swallows the ApiError into transitionError, so this never
   * throws). Both the DnD drop and the Move dialog funnel through here. */
  async function doTransition(issueId: string, from: string, to: string): Promise<void> {
    if (model === null) return
    announcement = `Moving issue to ${to}.`
    await model.transition(issueId, from, to)
    if (model.transitionError !== null) {
      announcement = `Could not move issue to ${to}: ${model.transitionError.message}`
    } else {
      announcement = `Issue moved to ${to}.`
    }
  }

  function openMove(issueId: string, from: string): void {
    if (model === null) return
    const targets = model.moveTargets(from)
    if (targets.length === 0) return
    const title = findTitle(issueId, from)
    moveDialog = { issueId, from, title, targets }
  }

  function findTitle(issueId: string, from: string): string {
    const col = model?.columns.find((c) => c.status === from)
    return col?.issues.find((i) => i.id === issueId)?.title ?? 'issue'
  }

  function selectMove(to: string): void {
    const d = moveDialog
    moveDialog = null
    if (d !== null) void doTransition(d.issueId, d.from, to)
  }
</script>

<section class="area" aria-label="Board">
  <header>
    <h1>Board</h1>
    <ProjectPicker {projects} selected={scope} onselect={onPick} />
  </header>

  {#if writable}
    <p class="sr-only" role="status" aria-live="polite" data-board-live>{announcement}</p>
    {#if model?.transitionError}
      <div class="error banner" role="alert" data-transition-error>
        <p>Could not move issue: {model.transitionError.message}</p>
        {#if model.transitionError.requestId}
          <p class="request-id">request {model.transitionError.requestId}</p>
        {/if}
      </div>
    {/if}
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
            adapter={dnd}
            {writable}
            transitioning={model.transitioning}
            collapsed={model.isCollapsed(column.status)}
            loadingMore={model.loadingMore[column.status] === true}
            canLoadMore={model.canLoadMore(column.status)}
            ontoggle={() => model?.toggle(column.status)}
            onloadmore={() => model?.loadMore(column.status)}
            onmove={openMove}
          />
        {/each}
      </div>
    {/if}
  </div>
</section>

{#if moveDialog}
  <Modal width="20rem" ariaLabelledby="move-dialog-title" backdropClose onclose={() => (moveDialog = null)}>
    <div class="move-dialog" data-move-dialog>
      <h2 id="move-dialog-title">Move issue</h2>
      <p class="move-sub">{moveDialog.title}</p>
      <ul class="targets">
        {#each moveDialog.targets as t, i (t)}
          <li>
            <!-- svelte-ignore a11y_autofocus -->
            <button
              type="button"
              class="target"
              data-move-target={t}
              autofocus={i === 0}
              onclick={() => selectMove(t)}
            >
              {t}
            </button>
          </li>
        {/each}
      </ul>
      <div class="move-actions">
        <button type="button" class="ghost" onclick={() => (moveDialog = null)}>Cancel</button>
      </div>
    </div>
  </Modal>
{/if}

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
  .banner {
    flex: 0 0 auto;
    border: 1px solid var(--danger);
    border-radius: var(--radius);
    padding: var(--space-1) var(--space-2);
  }
  .banner p {
    margin: 0;
  }
  .request-id {
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
  }
  /* Screen-reader-only live region (visually hidden, still announced). */
  .sr-only {
    position: absolute;
    width: 1px;
    height: 1px;
    margin: -1px;
    padding: 0;
    overflow: hidden;
    clip: rect(0 0 0 0);
    white-space: nowrap;
    border: 0;
  }
  .move-dialog {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .move-dialog h2 {
    margin: 0;
    font-size: var(--fs-base);
    font-weight: var(--fw-semibold);
  }
  .move-sub {
    margin: 0;
    color: var(--text-dim);
    font-size: var(--fs-sm);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .targets {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .target {
    width: 100%;
    text-align: left;
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-2);
    background: var(--surface-1);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
    cursor: pointer;
  }
  .target:hover,
  .target:focus-visible {
    border-color: var(--accent);
  }
  .move-actions {
    display: flex;
    justify-content: flex-end;
  }
  .move-actions .ghost {
    font-family: var(--font-ui);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-3);
    background: transparent;
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
    cursor: pointer;
  }
</style>
