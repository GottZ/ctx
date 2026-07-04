<script lang="ts">
  // One board column (design 04 §4.2/§4.3/§4.5/§6.2, waves U07 + U08). A
  // workflow-status column: a header (collapse toggle + status + wire count, plus
  // an `unmapped` badge when the status is not in the registry vocabulary) and —
  // when expanded — a VERTICALLY WINDOWED card list. Each column owns its own
  // scroll container and windowing (computeWindow over a fixed card height), so a
  // 10k-card column keeps the DOM at O(viewport): the board stays bounded even
  // with several columns open (§6.2 DOM ceiling). Near the bottom the column
  // keyset-appends its next page (per-column cursor); collapsed columns render
  // zero cards but keep their count visible.
  //
  // U08: when the board is writable (§5.3) AND this column is a real drop target
  // (not the synthetic unmapped one) the column registers as a DnD drop target
  // (use:columnDrop) and its cards become drag sources + grow a Move button. A
  // read-only board (adapter=null) renders exactly the U07 column — no drop
  // target, no grips. data-drop-over is set by the adapter during a hover only.
  //
  // NOTE: comments here avoid quote pairs / backticks — svelte2tsx scans the
  // script block for string literals without honouring line comments, so a
  // quoted word can swallow the closing script tag (script-left-open false
  // positive; the real compiler is unaffected).
  import Card from './Card.svelte'
  import { computeWindow, isNearBottom } from '../../lib/ui/virtual-window'
  import type { ClassifiedColumn } from './board-columns'
  import { columnDrop, type BoardDndAdapter } from '../../lib/board/dnd'

  let {
    column,
    scope,
    collapsed,
    loadingMore,
    canLoadMore,
    adapter = null,
    writable = false,
    transitioning = {},
    ontoggle,
    onloadmore,
    onmove,
  }: {
    column: ClassifiedColumn
    scope: string | null
    collapsed: boolean
    loadingMore: boolean
    canLoadMore: boolean
    adapter?: BoardDndAdapter | null
    writable?: boolean
    transitioning?: Record<string, boolean>
    ontoggle: () => void
    onloadmore: () => void
    onmove?: (issueId: string, from: string) => void
  } = $props()

  // Fixed card geometry (§4.3) — MUST match the .card height in Card.svelte (56)
  // plus the inter-card gap, so the spacer maths keep the scrollbar honest.
  const CARD_H = 56 + 8
  const OVERSCAN = 6
  const NEAR_BOTTOM_PX = CARD_H * 4

  // An unmapped column is never a drop target (§4.2), even on a writable board.
  const droppable = $derived(writable && column.category !== 'unmapped')

  let scroller = $state<HTMLElement | null>(null)
  let scrollTop = $state(0)
  let viewportH = $state(0)

  const win = $derived(
    computeWindow({
      scrollTop,
      viewportHeight: viewportH,
      rowHeight: CARD_H,
      total: column.issues.length,
      overscan: OVERSCAN,
    }),
  )
  const windowCards = $derived(column.issues.slice(win.start, win.end))
  const shown = $derived(column.issues.length)
  const remaining = $derived(Math.max(0, column.count - shown))

  function onScroll(): void {
    if (scroller === null) return
    scrollTop = scroller.scrollTop
    if (
      canLoadMore &&
      !loadingMore &&
      isNearBottom(scroller.scrollTop, scroller.clientHeight, scroller.scrollHeight, NEAR_BOTTOM_PX)
    ) {
      onloadmore()
    }
  }
</script>

<section
  class="column"
  data-board-column
  data-status={column.status}
  data-category={column.category}
  data-droppable={droppable ? '' : undefined}
  aria-label="{column.status} ({column.count})"
  use:columnDrop={{ adapter: droppable ? adapter : null, statusId: column.status }}
>
  <header>
    <button type="button" class="toggle" aria-expanded={!collapsed} onclick={ontoggle}>
      <span class="caret" aria-hidden="true">{collapsed ? '▸' : '▾'}</span>
      <span class="status">{column.status}</span>
      {#if column.category === 'unmapped'}
        <span class="badge unmapped" data-unmapped>unmapped</span>
      {/if}
    </button>
    <span class="count" data-count>{column.count}</span>
  </header>

  {#if !collapsed}
    <div
      class="cards"
      bind:this={scroller}
      bind:clientHeight={viewportH}
      onscroll={onScroll}
      role="list"
      aria-label="{column.status} issues"
    >
      {#if column.issues.length === 0}
        <p class="empty" role="status">No issues.</p>
      {:else}
        {#if win.padTop > 0}
          <div class="spacer" aria-hidden="true" style="height: {win.padTop}px"></div>
        {/if}
        {#each windowCards as issue (issue.id)}
          <div class="card-slot" role="listitem">
            <Card
              {issue}
              {scope}
              {adapter}
              writable={droppable}
              pending={transitioning[issue.id] === true}
              {onmove}
            />
          </div>
        {/each}
        {#if win.padBottom > 0}
          <div class="spacer" aria-hidden="true" style="height: {win.padBottom}px"></div>
        {/if}
      {/if}

      <div class="footer">
        {#if canLoadMore}
          <button type="button" class="load-more" disabled={loadingMore} onclick={onloadmore}>
            {loadingMore ? 'loading…' : `Load more (${shown} of ${column.count})`}
          </button>
        {:else if remaining > 0}
          <p class="hint" role="status">Showing {shown} of {column.count}.</p>
        {/if}
      </div>
    </div>
  {/if}
</section>

<style>
  .column {
    box-sizing: border-box;
    flex: 0 0 auto;
    width: 300px;
    max-width: 80vw;
    display: flex;
    flex-direction: column;
    min-height: 0;
    max-height: 100%;
    background: var(--surface-0);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
  /* Drop-target hover highlight — set by the DnD adapter during a drag only, so
     it never appears in a static (read-only) screenshot baseline. */
  .column:global([data-drop-over]) {
    border-color: var(--accent);
    outline: 1px solid var(--accent);
    outline-offset: -2px;
  }
  header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border-bottom: 1px solid var(--border);
  }
  .toggle {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    background: none;
    border: none;
    padding: 0;
    cursor: pointer;
    color: var(--text);
    font: inherit;
    min-width: 0;
  }
  .caret {
    color: var(--text-faint);
    font-size: var(--fs-xs);
  }
  .status {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    font-weight: var(--fw-medium);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .badge.unmapped {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    padding: 0 var(--space-2);
    border-radius: var(--radius);
    background: var(--warn-surface, var(--surface-2));
    border: 1px solid var(--warn, var(--border));
    color: var(--warn, var(--text-dim));
  }
  .count {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    color: var(--text-dim);
    flex: 0 0 auto;
  }
  /* Block flow (NOT flex) so the windowing spacer heights map 1:1 to layout:
     each card slot is exactly CARD_H tall (card 56 + 8 bottom gap), the constant
     the computeWindow maths use. A flex gap would add space around the spacers
     and drift the scroll geometry. */
  .cards {
    flex: 1 1 auto;
    min-height: 0;
    overflow-y: auto;
    padding: var(--space-2);
  }
  .card-slot {
    height: 64px;
    padding-bottom: 8px;
    box-sizing: border-box;
  }
  .empty {
    margin: 0;
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    text-align: center;
    padding: var(--space-2);
  }
  .footer {
    padding-top: var(--space-2);
  }
  .load-more {
    width: 100%;
  }
  .hint {
    margin: 0;
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    text-align: center;
  }
  .spacer {
    flex: 0 0 auto;
  }
</style>
