<script lang="ts">
  // Issue-list surface (design 04 §4.1/§5.5, wave U04) — DARK-LAUNCH scaffold.
  // U04 registers the route and renders the EmptyState only; the virtualized
  // list, server filters, keyset paging, search-mode and the ProjectPicker land
  // in U05. The one live wire here is the ?scope= parse (§4.1.5 picker vorarbeit):
  // the value is read and surfaced in the picker-skeleton slot, but NO fetch
  // happens yet (U04 makes zero API calls — the EmptyState is static).
  //
  // Reached only by deep link while dark-launched (viewWorkflow hides the nav
  // section + home tile); the page renders the EmptyState, never NotFound and
  // never a redirect (the route is an ungated member surface, auth server-side).
  import { route } from '../../router'
  import EmptyState from '../../lib/ui/EmptyState.svelte'
  import { parseScopeParam } from './scope-param'

  // Re-read on navigation (route.pathname is the reactive dep); location.search
  // carries the deep-linked ?scope=. Picker write-back is U05.
  const scope = $derived.by(() => {
    void route.pathname
    return parseScopeParam(location.search)
  })
</script>

<section class="area" aria-label="Issues">
  <header>
    <h1>Issues</h1>
    <!-- Picker skeleton (§4.1.5): shows the deep-linked scope; the select + 0/1/N
         project logic is U05. data-scope keeps it e2e-addressable. -->
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
      title="No issues to show yet"
      copy="The issue list for this workspace will appear here once the workflow surface is wired up."
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
