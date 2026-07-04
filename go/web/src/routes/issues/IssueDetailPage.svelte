<script lang="ts">
  // Issue-detail surface (design 04 §4.1/§5.5, wave U04) — DARK-LAUNCH scaffold.
  // The markdown body, comments thread, composer, related issues and sync-badge
  // land in U06; U04 renders the EmptyState only and makes zero API calls (the
  // uniform-404 handling for a foreign UUID is also U06). The :id param is read
  // so the scaffold is deep-link-shaped and e2e-addressable.
  //
  // Inherits the 'split' layout mode via the /issues prefix (modes.ts). Reached
  // by deep link only while dark-launched; renders the EmptyState, never crashes.
  import { route } from '../../router'
  import EmptyState from '../../lib/ui/EmptyState.svelte'

  const issueId = $derived(route.params.id ?? '')
</script>

<section class="area" aria-label="Issue detail" data-issue-id={issueId}>
  <header>
    <h1>Issue</h1>
  </header>

  <div class="body">
    <EmptyState
      title="Issue detail is not wired up yet"
      copy="The selected issue and its comments will render here once the detail view is connected."
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
    display: flex;
  }
</style>
