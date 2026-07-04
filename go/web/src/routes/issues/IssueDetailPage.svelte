<script lang="ts">
  // Issue-detail ROUTE host (design 04 §4.1/§5.5, waves U06 + U09). Thin shell:
  // it resolves the project from ?scope= + the project list, carries the back
  // link, and mounts the shared IssueDetailContent (U09 extraction — the ONE
  // detail renderer, also used by the board floating window). The detail body,
  // comments, composer, mutations and sync badge all live in that component.
  //
  // Mobile (< SM, design 04 §7-U09): the whole surface renders as a G6-sheet
  // (position:fixed; inset:0; z-window) instead of the in-flow desktop column —
  // the same full-bleed treatment the graph floating windows take on mobile. The
  // switch is a pure @media rule on .area (no JS breakpoint), so the desktop
  // baselines never move and only the 390 shots re-freeze.
  //
  // NOTE: comments in this script avoid apostrophes, backticks and quote pairs —
  // svelte2tsx scans the block for string/template literals without honouring
  // line comments (a quoted token swallows the closing script tag); the real
  // compiler is unaffected.
  import { onMount } from 'svelte'
  import { route } from '../../router'
  import EmptyState from '../../lib/ui/EmptyState.svelte'
  import IssueDetailContent from './IssueDetailContent.svelte'
  import { listProjects } from '../../lib/api/issues'
  import type { ProjectRow } from '../../lib/api/types'
  import type { ResourceStatus } from '../../lib/resource.svelte'
  import { parseScopeParam } from './scope-param'
  import { resolveScope, projectForScope } from './picker'
  import { issueFiltersToQuery } from './issue-filters'

  const blockId = $derived(route.params.id ?? '')

  let projects = $state<ProjectRow[]>([])
  let projectsStatus = $state<ResourceStatus>('idle')

  const urlScope = $derived(parseScopeParam(location.search))
  const activeScope = $derived(resolveScope(projects, urlScope))
  const activeProject = $derived(projectForScope(projects, activeScope))

  function listHref(): string {
    return `/issues${issueFiltersToQuery({ scope: activeScope, status: null, q: null, type: null, labels: [] })}`
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
  })
</script>

<section class="area" aria-label="Issue detail" data-issue-id={blockId} data-detail-root>
  <header>
    <a class="back" href={listHref()}>&larr; Issues</a>
  </header>

  <div class="body">
    {#if projectsStatus === 'loading'}
      <p class="state" aria-busy="true">loading&hellip;</p>
    {:else if projectsStatus === 'error'}
      <div class="error" role="alert"><p>Could not load your projects.</p></div>
    {:else if activeProject === null}
      <EmptyState
        title="Project not found"
        copy="This issue link points at a project you cannot see. Open the issue list to pick a project."
      />
    {:else}
      <IssueDetailContent projectId={activeProject.id} issueId={blockId} scope={activeScope} />
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
    border-bottom: 1px solid var(--border);
    padding-bottom: var(--space-2);
  }
  .back {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    color: var(--text-dim);
    text-decoration: none;
  }
  .back:hover {
    color: var(--accent);
  }
  .body {
    flex: 1 1 auto;
    min-height: 0;
    overflow-y: auto;
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

  /* Mobile G6-sheet (design 04 §7-U09): below SM the detail is a full-bleed
     fixed sheet over the shell (z-window covers the mobile bar, like the graph
     sheet). inset:0 on every edge is the behavioural contract the U09 gate
     pins — a desktop in-flow render fails it (position:static, not fixed). */
  @media (max-width: 639px) {
    .area {
      position: fixed;
      inset: 0;
      z-index: var(--z-window);
      max-width: none;
      margin: 0;
      gap: var(--space-2);
      padding: var(--space-3) var(--shell-gutter);
      background: var(--surface-0);
    }
  }
</style>
