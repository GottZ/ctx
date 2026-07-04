<script lang="ts">
  // Issue-detail surface (design 04 §4.1/§4.5/§5.5, wave U06). The U04 dark-
  // launch scaffold becomes the real detail: markdown body + comments thread +
  // composer + status/title mutations + sync badge + related, all read-only until
  // the caller may write the scope. API only through the U03 client (issues.ts).
  //
  // Render safety (§5.1): the body AND every comment render through the shared
  // Markdown component (lib/markdown — html:false + DOMPurify + remote-image
  // placeholder). No raw HTML sink is added here; the ONE {@html} lives in that
  // component (html-sinks freeze).
  //
  // Write gate (§5.3, fail-closed): the shipped wire carries no writable field
  // (Ist deviation, documented in lib/workflow/writable.ts), so writable is
  // derived from the caller write-scope politik (N3) — read-only unless the
  // issue scope is a scope the caller may write. A deep link on an issue in a
  // foreign / read-only scope shows NO composer and NO mutation control; the
  // server stays the real gate (a 422/403 is surfaced with the selection kept).
  //
  // Status vocabulary comes from the board endpoint (columns are the type-config
  // status set, server-derived) — the detail page never hardcodes statuses.
  //
  // NOTE: comments in this script avoid apostrophes, backticks and quote pairs —
  // svelte2tsx (svelte-check) scans the block for string/template literals
  // without honouring line comments, so a quoted token swallows the closing
  // script tag (a script-left-open false positive); the real compiler is fine.
  import { onMount } from 'svelte'
  import { route } from '../../router'
  import { session } from '../../lib/auth.svelte'
  import EmptyState from '../../lib/ui/EmptyState.svelte'
  import Markdown from '../../lib/markdown/Markdown.svelte'
  import ConfirmDialog from '../../lib/components/ConfirmDialog.svelte'
  import { listProjects, getBoard } from '../../lib/api/issues'
  import type { ProjectRow } from '../../lib/api/types'
  import type { ResourceStatus } from '../../lib/resource.svelte'
  import { IssueDetailModel } from './detail.svelte'
  import { parseScopeParam } from './scope-param'
  import { resolveScope, projectForScope } from './picker'
  import { syncBadgeForIssue } from '../../lib/workflow/sync-badge'
  import { canWriteScope } from '../../lib/workflow/writable'
  import { issueFiltersToQuery } from './issue-filters'

  /** A related-issue row as the detail wire may carry it (metadata.related). The
   * shipped wire does not emit it yet (Ist deviation) — rendered only when
   * present, so the section is dormant until the backend supplies it. */
  interface RelatedRef {
    id: string
    title: string
    status?: string
    via?: string
  }

  const blockId = $derived(route.params.id ?? '')

  let projects = $state<ProjectRow[]>([])
  let projectsStatus = $state<ResourceStatus>('idle')
  let model = $state<IssueDetailModel | null>(null)
  let statusOptions = $state<string[]>([])

  // Mutation UI state.
  let pendingStatus = $state('')
  let confirmingStatus = $state(false)
  let editingTitle = $state(false)
  let titleDraft = $state('')
  let titleError = $state<string | null>(null)
  let composerText = $state('')
  let composerError = $state<string | null>(null)

  const urlScope = $derived(parseScopeParam(location.search))
  const activeScope = $derived(resolveScope(projects, urlScope))
  const activeProject = $derived(projectForScope(projects, activeScope))
  const issue = $derived(model?.issue ?? null)
  const currentStatus = $derived(issue?.workflow_status ?? '')
  const sync = $derived(issue ? syncBadgeForIssue(issue.metadata) : null)

  // Write gate (§5.3): fail-closed derivation from the caller write-scope politik.
  const writable = $derived(
    issue !== null &&
      canWriteScope(issue.scope, { homeScope: session.homeScope, readScopes: session.readScopes }),
  )

  // Related refs, defensively read from metadata (dormant until the wire emits it).
  const related = $derived<RelatedRef[]>(readRelated(issue?.metadata))
  // Status targets = board columns plus the current one, minus duplicates.
  const statusTargets = $derived(
    [...new Set([...statusOptions, currentStatus])].filter((s) => s !== ''),
  )

  function readRelated(metadata: Record<string, unknown> | undefined | null): RelatedRef[] {
    const raw = metadata?.['related'] ?? metadata?.['structural_links']
    if (!Array.isArray(raw)) return []
    return raw
      .filter((r): r is Record<string, unknown> => typeof r === 'object' && r !== null)
      .map((r) => ({
        id: String(r.id ?? ''),
        title: String(r.title ?? r.id ?? ''),
        status: typeof r.status === 'string' ? r.status : undefined,
        via: typeof r.via === 'string' ? r.via : undefined,
      }))
      .filter((r) => r.id !== '')
  }

  function listHref(): string {
    return `/issues${issueFiltersToQuery({ scope: activeScope, status: null, q: null, type: null, labels: [] })}`
  }

  function relatedHref(id: string): string {
    return `/issues/${id}${issueFiltersToQuery({ scope: activeScope, status: null, q: null, type: null, labels: [] })}`
  }

  onMount(async () => {
    projectsStatus = 'loading'
    try {
      const res = await listProjects()
      projects = res.projects
      projectsStatus = 'ready'
    } catch {
      projectsStatus = 'error'
      return
    }
    const proj = activeProject
    if (proj === null) return
    const m = new IssueDetailModel(proj.id, blockId)
    model = m
    void m.load()
    // Best-effort status vocabulary; a board failure just hides the transition.
    try {
      const board = await getBoard(proj.id)
      statusOptions = board.columns.map((c) => c.status)
    } catch {
      statusOptions = []
    }
  })

  // Keep the pending status seeded to the current one whenever it changes.
  $effect(() => {
    pendingStatus = currentStatus
  })

  function openStatusConfirm(): void {
    if (pendingStatus === '' || pendingStatus === currentStatus) return
    confirmingStatus = true
  }

  async function doChangeStatus(): Promise<void> {
    // A throw here keeps the ConfirmDialog open, shows the message and keeps the
    // selection (§4.5) — the 422 policy-violation path.
    await model?.changeStatus(pendingStatus)
    confirmingStatus = false
  }

  function startTitleEdit(): void {
    titleDraft = issue?.title ?? ''
    titleError = null
    editingTitle = true
  }

  async function saveTitle(): Promise<void> {
    titleError = null
    const next = titleDraft.trim()
    if (next === '' || next === issue?.title) {
      editingTitle = false
      return
    }
    try {
      await model?.changeTitle(next)
      editingTitle = false
    } catch (err) {
      titleError = err instanceof Error ? err.message : String(err)
    }
  }

  async function submitComment(): Promise<void> {
    composerError = null
    if (composerText.trim() === '') return
    try {
      await model?.addComment(composerText)
      composerText = ''
    } catch (err) {
      composerError = err instanceof Error ? err.message : String(err)
    }
  }

  /** Best-effort comment author label from metadata (the block wire has no
   * author_label field — Ist deviation); falls back to a generic label. */
  function commentAuthor(metadata: Record<string, unknown> | undefined | null): string {
    const a = metadata?.['author_label'] ?? metadata?.['author']
    return typeof a === 'string' && a.trim() !== '' ? a : 'comment'
  }
</script>

<section class="area" aria-label="Issue detail" data-issue-id={blockId}>
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
    {:else if model === null || model.status === 'loading'}
      <p class="state" aria-busy="true">loading&hellip;</p>
    {:else if model.notFound}
      <EmptyState
        title="Issue not found"
        copy="No issue with this id exists in the selected project, or it is outside your access."
      />
    {:else if model.status === 'error'}
      <div class="error" role="alert">
        <p>{model.loadError?.message}</p>
        {#if model.loadError?.requestId}<p class="request-id">request {model.loadError.requestId}</p>{/if}
      </div>
    {:else if issue !== null}
      <article class="issue">
        <div class="titlebar">
          {#if editingTitle}
            <div class="title-edit">
              <input
                type="text"
                aria-label="Issue title"
                bind:value={titleDraft}
                disabled={model.mutating}
              />
              <button type="button" disabled={model.mutating} onclick={saveTitle}>Save title</button>
              <button type="button" class="ghost" disabled={model.mutating} onclick={() => (editingTitle = false)}>
                Cancel
              </button>
            </div>
            {#if titleError}<p class="error inline" role="alert">{titleError}</p>{/if}
          {:else}
            <h1>{issue.title}</h1>
            {#if writable}
              <button type="button" class="ghost small" onclick={startTitleEdit}>Edit title</button>
            {/if}
          {/if}
        </div>

        <div class="meta">
          <span class="type">{issue.type ?? issue.category}</span>
          {#if sync}
            <span class="sync tone-{sync.tone}" title={sync.hint} data-sync-state={sync.state}>
              {sync.label}
            </span>
          {/if}
          {#if issue.tags.length > 0}
            <span class="labels">
              {#each issue.tags as tag (tag)}<span class="label">{tag}</span>{/each}
            </span>
          {/if}
        </div>

        <div class="statusbar">
          {#if writable && statusTargets.length > 1}
            <label class="status-picker">
              <span class="lbl">Status</span>
              <select aria-label="Workflow status" bind:value={pendingStatus} disabled={model.mutating}>
                {#each statusTargets as s (s)}<option value={s}>{s}</option>{/each}
              </select>
            </label>
            <button
              type="button"
              disabled={model.mutating || pendingStatus === currentStatus}
              onclick={openStatusConfirm}
            >
              Change status
            </button>
          {:else}
            <span class="status-badge" data-status={currentStatus}>{currentStatus}</span>
          {/if}
        </div>

        {#if !writable}
          <p class="readonly-banner" role="note">Read-only in this scope &mdash; you cannot edit this issue.</p>
        {/if}

        <div class="issue-body">
          <Markdown source={issue.content} />
        </div>

        {#if related.length > 0}
          <section class="related" aria-label="Related issues">
            <h2>Related</h2>
            <ul>
              {#each related as r (r.id)}
                <li>
                  <a href={relatedHref(r.id)}>{r.title}</a>
                  {#if r.status}<span class="rel-status">{r.status}</span>{/if}
                </li>
              {/each}
            </ul>
          </section>
        {/if}

        <section class="comments" aria-label="Comments">
          <h2>Comments <span class="count">({model.comments.length})</span></h2>

          {#if model.comments.length === 0}
            <p class="state">No comments yet.</p>
          {:else}
            <ul class="thread">
              {#each model.visibleComments as c (c.id)}
                <li class="comment" data-comment>
                  <div class="c-meta">
                    <span class="c-author">{commentAuthor(c.metadata)}</span>
                    <time datetime={c.created_at}>{c.created_at.slice(0, 10)}</time>
                  </div>
                  <div class="c-body"><Markdown source={c.content} /></div>
                </li>
              {/each}
            </ul>

            {#if model.hasHiddenComments}
              <button type="button" class="reveal" onclick={() => model?.revealMore()}>
                Show more comments ({model.comments.length - model.revealed} hidden)
              </button>
            {/if}
            {#if model.canLoadMoreComments}
              <button
                type="button"
                class="load-more"
                disabled={model.loadingMore}
                onclick={() => model?.loadMoreComments()}
              >
                {model.loadingMore ? 'loading…' : 'Load older comments'}
              </button>
            {/if}
          {/if}

          {#if writable}
            <form class="composer" onsubmit={(e) => { e.preventDefault(); void submitComment() }}>
              <label class="lbl" for="comment-composer">Add a comment</label>
              <textarea
                id="comment-composer"
                bind:value={composerText}
                rows="3"
                placeholder="Write a comment (markdown supported)…"
                disabled={model.posting}
              ></textarea>
              {#if composerError}<p class="error inline" role="alert">{composerError}</p>{/if}
              <div class="composer-actions">
                <button type="submit" disabled={model.posting || composerText.trim() === ''}>
                  {model.posting ? 'posting…' : 'Comment'}
                </button>
              </div>
            </form>
          {/if}
        </section>
      </article>
    {/if}
  </div>
</section>

{#if confirmingStatus}
  <ConfirmDialog
    title="Change status"
    message={`Move this issue to "${pendingStatus}"?`}
    confirmLabel="Move"
    onconfirm={doChangeStatus}
    oncancel={() => (confirmingStatus = false)}
  />
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
  .issue {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    max-width: 48rem;
  }
  .titlebar {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  h1 {
    margin: 0;
    font-size: var(--fs-xl);
    font-weight: var(--fw-semibold);
    letter-spacing: var(--track-1);
  }
  .title-edit {
    display: flex;
    gap: var(--space-2);
    align-items: center;
  }
  .title-edit input {
    flex: 1 1 auto;
    font-size: var(--fs-lg);
    padding: var(--space-1) var(--space-2);
    background: var(--surface-0);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
  }
  .meta {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
  }
  .type {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    text-transform: uppercase;
    letter-spacing: var(--label-tracking);
    color: var(--text-faint);
  }
  .sync {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    padding: var(--space-0) var(--space-2);
    border-radius: var(--radius);
    border: 1px solid var(--border-strong);
    color: var(--text-dim);
  }
  .sync.tone-ok {
    color: var(--ok);
    border-color: var(--ok);
  }
  .sync.tone-warn {
    color: var(--warn);
    border-color: var(--warn);
  }
  .sync.tone-danger {
    color: var(--danger);
    border-color: var(--danger);
  }
  .labels {
    display: inline-flex;
    gap: var(--space-1);
    flex-wrap: wrap;
  }
  .label {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    padding: var(--space-0) var(--space-1);
    background: var(--surface-2);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    color: var(--text-dim);
  }
  .statusbar {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .status-picker {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
  }
  .status-picker .lbl {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    text-transform: uppercase;
    letter-spacing: var(--label-tracking);
    color: var(--text-faint);
  }
  .status-picker select {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-2);
    background: var(--surface-0);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
  }
  .status-badge {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    padding: var(--space-0) var(--space-2);
    background: var(--surface-2);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    color: var(--text-dim);
  }
  .readonly-banner {
    margin: 0;
    font-size: var(--fs-sm);
    color: var(--text-dim);
    background: var(--surface-2);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    padding: var(--space-1) var(--space-2);
  }
  .issue-body {
    line-height: var(--lh-body);
  }
  .related h2,
  .comments h2 {
    margin: 0 0 var(--space-2);
    font-size: var(--fs-base);
    font-weight: var(--fw-semibold);
  }
  .related ul {
    margin: 0;
    padding-left: var(--space-4);
  }
  .rel-status {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    color: var(--text-faint);
    margin-left: var(--space-2);
  }
  .comments {
    border-top: 1px solid var(--border);
    padding-top: var(--space-3);
  }
  .count {
    color: var(--text-faint);
    font-weight: var(--fw-medium);
  }
  .thread {
    list-style: none;
    margin: 0 0 var(--space-3);
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  /* content-visibility bounds paint/layout of off-screen comments (the 500-
     comment render budget) while the reveal cap bounds the number rendered. */
  .comment {
    border: 1px solid var(--border);
    border-radius: var(--radius);
    padding: var(--space-2) var(--space-3);
    content-visibility: auto;
    contain-intrinsic-size: auto 4rem;
  }
  .c-meta {
    display: flex;
    justify-content: space-between;
    align-items: baseline;
    gap: var(--space-2);
    margin-bottom: var(--space-1);
  }
  .c-author {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    text-transform: uppercase;
    letter-spacing: var(--label-tracking);
    color: var(--text-faint);
  }
  .c-meta time {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    color: var(--text-faint);
  }
  .composer {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    margin-top: var(--space-2);
  }
  .composer .lbl {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    text-transform: uppercase;
    letter-spacing: var(--label-tracking);
    color: var(--text-faint);
  }
  .composer textarea {
    font-family: var(--font-ui);
    font-size: var(--fs-sm);
    padding: var(--space-2);
    background: var(--surface-0);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
    resize: vertical;
  }
  .composer-actions {
    display: flex;
    justify-content: flex-end;
  }
  .reveal,
  .load-more {
    width: 100%;
    margin-bottom: var(--space-2);
  }
  button {
    font-family: var(--font-ui);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-3);
    background: var(--surface-2);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
    cursor: pointer;
  }
  button:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  button.ghost {
    background: transparent;
  }
  button.small {
    font-size: var(--fs-xs);
    padding: var(--space-0) var(--space-2);
    align-self: flex-start;
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
  .error.inline {
    margin: 0;
  }
  .request-id {
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
  }
</style>
