<script lang="ts">
  // One board card (design 04 §4.1/§5.5, wave U07). A fixed-height link tile —
  // title + a status/updated meta line — that deep-links to the issue detail,
  // carrying the active scope so /issues/:id can resolve the project. The height
  // is fixed (mirrors CARD_H in Column.svelte) so the per-column windowing is
  // pure math and the screenshot baseline is deterministic (§4.3).
  //
  // The title is attacker-controlled (any forge user opens issues); it renders
  // as Svelte text interpolation (auto-escaped), NEVER {@html} — the same rule
  // the issue list follows (§5.1).
  import type { IssueRow } from '../../lib/api/types'

  let { issue, scope }: { issue: IssueRow; scope: string | null } = $props()

  const href = $derived(`/issues/${issue.id}${scope ? `?scope=${encodeURIComponent(scope)}` : ''}`)
</script>

<a class="card" {href} data-board-card>
  <span class="title">{issue.title}</span>
  <span class="meta">
    <span class="badge">{issue.workflow_status}</span>
    <time datetime={issue.updated_at}>{issue.updated_at.slice(0, 10)}</time>
  </span>
</a>

<style>
  .card {
    box-sizing: border-box;
    height: 56px;
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
  .title {
    font-size: var(--fs-sm);
    font-weight: var(--fw-medium);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
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
</style>
