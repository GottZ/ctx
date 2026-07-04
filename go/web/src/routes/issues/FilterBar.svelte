<script lang="ts">
  // Issue-list filter bar (design 04 §4.2/§5.5, wave U05). The server filters
  // the W6 list endpoint actually supports (api/issues.ts IssueListParams): a
  // free-text query `q` (→ SEARCH mode) and a workflow `status` (→ wire `state`).
  // Submit-driven (Enter / button) like BlocksPage — deterministic for the
  // Playwright roundtrip, no debounce race. Status options are the union of the
  // statuses present in the loaded rows plus the active filter, so a deep-linked
  // status stays selectable even before its rows scroll into view.
  //
  // NOTE (Ist-deviation, §3.2 vs api/issues.ts): the design lists a `type`
  // filter, but the shipped W6 list handler has NO type param — a type narrowing
  // would fight the keyset (client-side gaps). `?type=`/`?label=` stay in the URL
  // round-trip (issue-filters.ts, deep-link fidelity + board reuse) and labels
  // reach the server when present, but the bar surfaces only the two filters the
  // list endpoint honours. Type-driven views are the board's (U07) territory.
  let {
    q,
    status,
    statusOptions,
    onsearch,
    onstatus,
  }: {
    /** Current query text (empty = browse mode). */
    q: string
    /** Current status filter (empty = all). */
    status: string
    /** Selectable statuses (union of loaded rows + active filter). */
    statusOptions: string[]
    /** Fires the (trimmed) query — '' clears search mode. */
    onsearch: (q: string) => void
    /** Fires the status filter — '' clears it. */
    onstatus: (status: string) => void
  } = $props()

  // Local input mirror; submit commits it into the URL/model. Kept in sync with
  // the prop when the URL rewrites q (deep link / clear) via the effect below.
  let input = $state('')
  $effect(() => {
    input = q
  })

  function submit(event: SubmitEvent): void {
    event.preventDefault()
    onsearch(input.trim())
  }

  function onStatusChange(event: Event): void {
    onstatus((event.currentTarget as HTMLSelectElement).value)
  }
</script>

<div class="filter-bar" role="search">
  <form class="search" onsubmit={submit}>
    <input
      type="search"
      aria-label="Search issues"
      placeholder="search issues — leave empty to browse newest"
      spellcheck="false"
      bind:value={input}
    />
    <button type="submit">Search</button>
  </form>

  <label class="status">
    <span class="lbl">Status</span>
    <select aria-label="Filter by status" value={status} onchange={onStatusChange}>
      <option value="">All statuses</option>
      {#each statusOptions as s (s)}
        <option value={s}>{s}</option>
      {/each}
    </select>
  </label>
</div>

<style>
  .filter-bar {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-3);
  }
  .search {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex: 1 1 16rem;
    min-width: 0;
  }
  .search input {
    flex: 1;
    min-width: 0;
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
  }
  .status {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--fs-sm);
  }
  .lbl {
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
  }
  select {
    min-height: 2rem;
    padding: var(--space-1) var(--space-2);
    background: var(--surface-0);
    color: var(--text);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    font-size: var(--fs-sm);
    font-family: var(--font-mono);
  }
</style>
