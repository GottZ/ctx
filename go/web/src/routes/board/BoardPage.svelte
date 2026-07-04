<script lang="ts">
  // Board surface (design 04 §4.1/§5.5, wave U04) — DARK-LAUNCH scaffold. The
  // status columns (from the type-config workflow, U07), per-column windows,
  // counts and DnD (U08) land later; U04 renders the EmptyState only + the
  // ?scope= parse (shared with /issues) and makes zero API calls.
  //
  // Runs in the new 'board' layout mode (modes.ts / AppShell §4.1.2): full-bleed
  // with horizontal scroll-containment. Reached by deep link only while
  // dark-launched; renders the EmptyState, never NotFound, never a redirect.
  import { route } from '../../router'
  import EmptyState from '../../lib/ui/EmptyState.svelte'
  import { parseScopeParam } from '../issues/scope-param'

  const scope = $derived.by(() => {
    void route.pathname
    return parseScopeParam(location.search)
  })
</script>

<section class="area" aria-label="Board">
  <header>
    <h1>Board</h1>
    <!-- Shared picker skeleton (§4.1.5): the board reads the same ?scope= as the
         issue list; the select logic is U05. -->
    <div class="picker-skeleton" data-scope={scope ?? ''}>
      {#if scope}
        <span class="scope">Scope: <code>{scope}</code></span>
      {:else}
        <span class="scope dim">No project selected</span>
      {/if}
    </div>
  </header>

  <div class="body">
    <EmptyState
      title="No board to show yet"
      copy="The status columns for this workspace will appear here once the board is wired up."
    />
  </div>
</section>

<style>
  .area {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    min-height: 100%;
    box-sizing: border-box;
    padding: var(--space-4) var(--shell-gutter);
  }
  header {
    display: flex;
    align-items: baseline;
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
  .picker-skeleton {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    color: var(--text-dim);
  }
  .scope.dim {
    color: var(--text-faint);
  }
  .body {
    flex: 1 1 auto;
    display: flex;
  }
</style>
