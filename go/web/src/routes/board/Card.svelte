<script lang="ts">
  // One board card (design 04 §4.1/§4.5/§5.5, waves U07 + U08). A fixed-height
  // link tile — title + a status/updated meta line — that deep-links to the issue
  // detail, carrying the active scope so /issues/:id can resolve the project. The
  // height is fixed (mirrors CARD_H in Column.svelte) so the per-column windowing
  // is pure math and the screenshot baseline is deterministic (§4.3).
  //
  // U08 write affordances render ONLY when the board is writable (§5.3 fail-
  // closed): the card becomes a DnD source (use:cardDrag) AND grows a keyboard
  // Move button that opens the board Move dialog (the mouse-free transition
  // path). A read-only board (adapter=null, writable=false) renders exactly the
  // U07 card — no grip, no Move button — so the read-only baselines never move.
  // The move control is absolutely positioned so it never changes the 56px card
  // geometry the windowing maths depend on.
  //
  // The title is attacker-controlled (any forge user opens issues); it renders
  // as Svelte text interpolation (auto-escaped), NEVER {@html} — the same rule
  // the issue list follows (§5.1).
  //
  // NOTE: comments here avoid quote pairs / backticks — svelte2tsx scans the
  // script block for string literals without honouring line comments, so a
  // quoted word can swallow the closing script tag (false positive; the real
  // compiler is unaffected).
  import type { IssueRow } from '../../lib/api/types'
  import { cardDrag, type BoardDndAdapter } from '../../lib/board/dnd'

  let {
    issue,
    scope,
    adapter = null,
    writable = false,
    pending = false,
    onmove,
  }: {
    issue: IssueRow
    scope: string | null
    adapter?: BoardDndAdapter | null
    writable?: boolean
    pending?: boolean
    onmove?: (issueId: string, from: string) => void
  } = $props()

  const href = $derived(`/issues/${issue.id}${scope ? `?scope=${encodeURIComponent(scope)}` : ''}`)
</script>

<div class="card-wrap" class:pending>
  <a
    class="card"
    {href}
    data-board-card
    aria-disabled={pending ? 'true' : undefined}
    use:cardDrag={{ adapter: writable ? adapter : null, issueId: issue.id, from: issue.workflow_status }}
  >
    <span class="title">{issue.title}</span>
    <span class="meta">
      <span class="badge">{issue.workflow_status}</span>
      <time datetime={issue.updated_at}>{issue.updated_at.slice(0, 10)}</time>
    </span>
  </a>

  {#if writable}
    <button
      type="button"
      class="move"
      data-move-trigger
      disabled={pending}
      aria-label="Move {issue.title}"
      title="Move to another column"
      onclick={() => onmove?.(issue.id, issue.workflow_status)}
    >
      <span aria-hidden="true">Move</span>
    </button>
  {/if}
</div>

<style>
  /* The wrap is the fixed 56px box the windowing maths use (Column CARD_H = 56 +
     8 gap); the move button floats over it so it never adds flow height. */
  .card-wrap {
    position: relative;
    height: 56px;
  }
  .card {
    box-sizing: border-box;
    height: 100%;
    display: flex;
    flex-direction: column;
    justify-content: center;
    gap: var(--space-1);
    padding: var(--space-2) var(--space-3);
    background: var(--surface-1);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    text-decoration: none;
    color: var(--text);
  }
  .card:hover {
    border-color: var(--accent);
  }
  .card:global([data-dragging]),
  .card-wrap.pending .card {
    opacity: 0.5;
  }
  .title {
    font-size: var(--fs-sm);
    font-weight: var(--fw-medium);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    /* leave room for the floating move button so a long title does not run
       under it */
    padding-right: var(--space-5);
  }
  .meta {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
  }
  .badge {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    padding: 0 var(--space-2);
    background: var(--surface-2);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    color: var(--text-dim);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 60%;
  }
  time {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    color: var(--text-faint);
    white-space: nowrap;
  }
  .move {
    position: absolute;
    top: var(--space-1);
    right: var(--space-1);
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    padding: var(--space-0) var(--space-1);
    background: var(--surface-2);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text-dim);
    cursor: pointer;
    line-height: var(--lh-body);
  }
  .move:hover:not(:disabled) {
    border-color: var(--accent);
    color: var(--text);
  }
  .move:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
</style>
