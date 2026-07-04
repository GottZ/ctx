<script lang="ts">
  // Shared table primitive (design 05 sec.7 Q10). ONE place owns the
  // scroll-shell + <table> chrome + the head/cell CSS that four hand-rolled
  // tables duplicated byte-for-byte (BackendTable, AdminPage, TenantPage,
  // LlmlogTable — FE-Inventur sec.12.6 S9): the .scroll overflow container, the
  // width/border-collapse table box, and the identical <th>/<td> typography,
  // padding and separators. Q3 unified the colour VALUES those cells use; Q10
  // unifies the STRUCTURE. This is the Modal.svelte (Q9) sibling: own the native
  // element, keep every consumer-specific column/row detail out here as a
  // snippet the caller renders.
  //
  // NO data logic. The rows are the `children` snippet — the consumer runs its
  // own {#each} (or a windowed slice), so the primitive never forces a whole-
  // list iteration: 04-U05 renders 10k+ issues VIRTUALISED and drops its
  // windowed rows straight into this same shell (design 05 sec.7 Ziel-Scale).
  // The <th> row is the `head` snippet; the empty message is `emptyState`,
  // rendered INSTEAD of the table when `empty` is true (each caller keeps its
  // exact inline empty markup — verhaltensneutral, no forced EmptyState.svelte
  // which is the area-wide panel, a different shape).
  //
  // The cell rules are :global(.ctx-table th/td): the <th>/<td> live in the
  // caller's head/children snippets, so they carry the CALLER's scope hash, not
  // this file's — a scoped `.ctx-table :global(th)` would take on THIS file's
  // hash and outrank a caller's single-class column modifier (.num, .slug …),
  // silently overriding e.g. a right-aligned numeric header. The fully-global
  // form reproduces the exact (0,1,1) specificity the inlined `th`/`td` rules
  // had, so every consumer column class relates to the base exactly as before
  // (verhaltensneutral); .ctx-table is unique to this primitive, so it scopes by
  // convention with no leak.
  //
  // Density lives here in ONE place — the --space-1/--space-3 cell padding all
  // four tables held inline is now a single pair to tune for the 1M-scale
  // surfaces (the space-scale tokens ARE the density tokens; kept at the current
  // values so the migration is diff-free). Cell vertical-align is the only
  // per-caller variable of the shared block (three aligned `top` for their
  // multi-line name/scopes cells, LlmlogTable kept the baseline default) — a
  // declarative prop, not a re-hand-rolled CSS rule.
  //
  // NO persistence: the primitive adds no localStorage/sessionStorage (design 05
  // UI-State-Invariante — only ctx.theme + ctx.nav-rail persist, neither here).
  import type { Snippet } from 'svelte'

  let {
    /** Cell vertical-align: 'top' (multi-line name/scopes cells) or 'baseline'. */
    valign = 'top',
    /** Optional aria-label on the <table> element (most callers name the card). */
    label,
    /** When true (and emptyState is given) the empty message replaces the table. */
    empty = false,
    /** The <tr><th>…</th></tr> header row. */
    head,
    /** The empty-state markup, rendered instead of the table when `empty`. */
    emptyState,
    /** The <tr>…</tr> body rows — the consumer owns the iteration (virtual-safe). */
    children,
  }: {
    valign?: 'top' | 'baseline'
    label?: string
    empty?: boolean
    head: Snippet
    emptyState?: Snippet
    children: Snippet
  } = $props()
</script>

{#if empty && emptyState}
  {@render emptyState()}
{:else}
  <div class="scroll">
    <table class="ctx-table" class:valign-baseline={valign === 'baseline'} aria-label={label}>
      <thead>{@render head()}</thead>
      <tbody>{@render children()}</tbody>
    </table>
  </div>
{/if}

<style>
  .scroll {
    overflow-x: auto;
  }
  /* Fully-global under the unique .ctx-table class — see the script header for why
     scoped selectors would outrank consumer column modifiers. */
  :global(.ctx-table) {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--fs-sm);
  }
  :global(.ctx-table th) {
    text-align: left;
    /* Density (Q10): the one place the table cell padding lives — the --space
       scale IS the density token, kept at the four tables' inline values. */
    padding: var(--space-1) var(--space-3);
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    font-weight: var(--fw-medium);
    border-bottom: 1px solid var(--border);
    white-space: nowrap;
  }
  :global(.ctx-table td) {
    padding: var(--space-1) var(--space-3);
    border-bottom: 1px solid var(--surface-2);
    vertical-align: top;
  }
  :global(.ctx-table.valign-baseline td) {
    vertical-align: baseline;
  }
</style>
