<script lang="ts">
  // Issue-detail CONTENT (design 04 §4.6.2, wave U09) — the self-loading detail
  // body extracted from IssueDetailPage so ONE renderer serves both hosts: the
  // /issues/:id route (desktop layout + mobile G6-sheet) AND the desktop board
  // floating window (card to a lib/windows content-snippet). There are NOT two
  // markdown paths and NOT two detail renderers — both hosts mount THIS (§9.6).
  //
  // Given (projectId, issueId, scope) it owns its own IssueDetailModel + status
  // vocabulary load, exactly as the page did before. The optional titleId lets a
  // floating-window host wire aria-labelledby to the issue heading (an h2 in a
  // window; the route renders an h1) — the BlockDetailContent mechanism (G5).
  //
  // Render safety (§5.1): the body AND every comment render through the shared
  // Markdown component (lib/markdown — html:false + DOMPurify + remote-image
  // placeholder). No raw HTML sink is added here; the ONE html-sink lives in that
  // component (html-sinks freeze).
  //
  // Write gate (§5.3, fail-closed): the shipped wire carries no writable field
  // (Ist deviation, documented in lib/workflow/writable.ts), so writable is
  // derived from the caller write-scope politik (N3) — read-only unless the
  // issue scope is a scope the caller may write. A deep link on an issue in a
  // foreign / read-only scope shows NO composer and NO mutation control; the
  // server stays the real gate (a 422/403 is surfaced with the selection kept).
  //
  // NOTE: comments in this script avoid apostrophes, backticks and quote pairs —
  // svelte2tsx (svelte-check) scans the block for string/template literals
  // without honouring line comments, so a quoted token swallows the closing
  // script tag (a script-left-open false positive); the real compiler is fine.
  import { onMount } from 'svelte'
  import { session } from '../../lib/auth.svelte'
  import EmptyState from '../../lib/ui/EmptyState.svelte'
  import Markdown from '../../lib/markdown/Markdown.svelte'
  import ConfirmDialog from '../../lib/components/ConfirmDialog.svelte'
  import { getBoard } from '../../lib/api/issues'
  import { IssueDetailModel } from './detail.svelte'
  import { LiveSource } from '../../lib/workflow/live'
  import { syncBadgeForIssue } from '../../lib/workflow/sync-badge'
  import { canWriteScope } from '../../lib/workflow/writable'
  import { issueFiltersToQuery } from './issue-filters'

  let {
    projectId,
    issueId,
    scope,
    statusOptions: statusOptionsProp,
    titleId,
  }: {
    projectId: string
    issueId: string
    scope: string | null
    /** Pre-loaded status vocabulary (board host passes it); the route omits it
     * and this component loads the board itself. */
    statusOptions?: string[]
    /** Host id for the heading so a floating window can wire aria-labelledby. */
    titleId?: string
  } = $props()

  /** A related-issue row as the detail wire may carry it (metadata.related). The
   * shipped wire does not emit it yet (Ist deviation) — rendered only when
   * present, so the section is dormant until the backend supplies it. */
  interface RelatedRef {
    id: string
    title: string
    status?: string
    via?: string
  }

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

  const issue = $derived(model?.issue ?? null)
  const currentStatus = $derived(issue?.workflow_status ?? '')
  const sync = $derived(issue ? syncBadgeForIssue(issue.metadata) : null)

  // Write gate (§5.3): fail-closed derivation from the caller write-scope politik.
  const writable = $derived(
    issue !== null &&
      canWriteScope(issue.scope, { homeScope: session.homeScope, readScopes: session.readScopes }),
  )

  // Drag-Region host-awareness (U04-W7, AM-4, design 04-§4.5). THIS renderer has
  // two live hosts: the board FloatingWindow (BoardPage passes titleId to wire the
  // window h2) AND the /issues/:id route (IssueDetailPage mounts it WITHOUT
  // titleId, an h1 in a scrollable reading column). Only the window host may turn
  // the head into a drag handle; on the route host user-select:none / cursor:move
  // would be a real regression (a reader could not select the title, the cursor
  // would lie). titleId IS that host signal, so it gates both the data-window-drag
  // markers and the drag CSS (via .in-window). Unlike BlockDetailContent (W5, one
  // window-only host) the markers here are NOT unconditional.
  const inWindow = $derived(titleId != null)

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

  function relatedHref(id: string): string {
    return `/issues/${id}${issueFiltersToQuery({ scope, status: null, q: null, type: null, labels: [] })}`
  }

  onMount(() => {
    const m = new IssueDetailModel(projectId, issueId)
    model = m
    void m.load()
    if (statusOptionsProp !== undefined) {
      statusOptions = statusOptionsProp
    } else {
      // Best-effort status vocabulary; a board failure just hides the transition.
      void getBoard(projectId)
        .then((board) => (statusOptions = board.columns.map((c) => c.status)))
        .catch(() => (statusOptions = []))
    }
    // Live (U13, moved here by the U09 extraction — the content owns the model,
    // so it owns the refetch): reload this issue only when a frame names its id
    // (targeted) or a bulk/resync/poll signal arrives (full).
    const live = new LiveSource({
      projectId,
      getInit: () => (session.key ? { headers: { Authorization: `Bearer ${session.key}` } } : {}),
      onBatch: (batch) => {
        if (batch.full || batch.ids.includes(issueId)) void m.load()
      },
    })
    live.start()
    return () => live.stop()
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

{#if model === null || model.status === 'loading'}
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
  <!-- U04-W7 (AM-4, design 04-§4.5): in a board FloatingWindow host (inWindow),
       the head free area (titlebar + meta) is the drag handle of the surrounding
       window via the DOM contract (data-window-drag / data-window-drag-exempt +
       the generic DRAG_EXEMPT interactive list in FloatingWindow). The Titel-Edit
       button/input, the Status-Wechsel select/button and the composer are all
       generically exempt (button/input/select/textarea) so they keep working; the
       copy-relevant labels opt out explicitly. On the /issues/:id route host
       (inWindow=false) NO marker is emitted and .in-window is absent → the reader
       keeps full selection. -->
  <article class="issue" class:in-window={inWindow}>
    <div class="titlebar" data-window-drag={inWindow ? '' : undefined}>
      {#if editingTitle}
        <div class="title-edit">
          <input type="text" aria-label="Issue title" bind:value={titleDraft} disabled={model.mutating} />
          <button type="button" disabled={model.mutating} onclick={saveTitle}>Save title</button>
          <button type="button" class="ghost" disabled={model.mutating} onclick={() => (editingTitle = false)}>
            Cancel
          </button>
        </div>
        {#if titleError}<p class="error inline" role="alert">{titleError}</p>{/if}
      {:else}
        <svelte:element this={titleId ? 'h2' : 'h1'} id={titleId} class="issue-title">{issue.title}</svelte:element>
        {#if writable}
          <button type="button" class="ghost small" onclick={startTitleEdit}>Edit title</button>
        {/if}
      {/if}
    </div>

    <div class="meta" data-window-drag={inWindow ? '' : undefined}>
      <span class="type">{issue.type ?? issue.category}</span>
      {#if sync}
        <span class="sync tone-{sync.tone}" title={sync.hint} data-sync-state={sync.state}>
          {sync.label}
        </span>
      {/if}
      {#if issue.tags.length > 0}
        <span class="labels" data-window-drag-exempt>
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
          <label class="lbl" for="comment-composer-{issueId}">Add a comment</label>
          <textarea
            id="comment-composer-{issueId}"
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
  /* U04-W7 (AM-4, design 04-§4.5): host-aware drag regions. ONLY when this
     renderer sits in a board FloatingWindow (host passes titleId → .in-window) do
     the titlebar + meta become the drag handle of the surrounding window. The
     /issues/:id route mounts the SAME component WITHOUT titleId, so .in-window is
     absent and NONE of these rules apply — the reading route keeps full text
     selection and the default cursor. touch-action:none → the browser drags
     instead of scrolling; user-select:none → drags instead of selecting;
     cursor:move is the affordance. The full-text body, related and comments stay
     unmarked (scroll/select as Ist). */
  .in-window .titlebar,
  .in-window .meta {
    touch-action: none;
    user-select: none;
    cursor: move;
  }
  /* The copy-relevant labels opt back out (selectable/scrollable). */
  .in-window .labels[data-window-drag-exempt] {
    touch-action: auto;
    user-select: text;
    cursor: auto;
  }
  /* The title input (edit mode) sits INSIDE the titlebar drag region; without this
     restore, user-select:none would cascade in and block selecting/typing. The
     drag itself never starts on it (input is DRAG_EXEMPT). */
  .in-window .title-edit input {
    touch-action: auto;
    user-select: text;
    cursor: text;
  }
  .issue-title {
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
