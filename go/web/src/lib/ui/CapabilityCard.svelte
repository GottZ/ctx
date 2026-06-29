<script lang="ts" module>
  // Generic capability card (design 06-role-nav.md §3, Welle N6). A titled panel
  // that explains one facet of the caller's session as prose — a labelled value
  // (scope / role / tenant) plus copy — with an optional CTA slot for a corpus
  // entry-point link. Reading-column furniture: token-only, no DOM logic.
  //
  // a11y (§58): each card is a region named by its own heading via
  // aria-labelledby, so a screen-reader can land on "Write scope" directly. The
  // heading id is uniqued by a module-scoped counter (CSR mount order is stable)
  // — no framework id helper needed, no collision when several cards co-exist.
  let seq = 0
</script>

<script lang="ts">
  import type { Snippet } from 'svelte'

  let {
    title,
    value = null,
    copy = null,
    cta,
  }: {
    /** Capability facet name — becomes the region's accessible name. */
    title: string
    /** Headline value (scope name, role, tenant). Mono. Omitted when empty. */
    value?: string | null
    /** Explanatory prose under the value. */
    copy?: string | null
    /** Optional call-to-action slot (e.g. a corpus-entry <a>). */
    cta?: Snippet
  } = $props()

  const titleId = `cap-card-${++seq}`
</script>

<article class="card" aria-labelledby={titleId}>
  <h2 id={titleId} class="title">{title}</h2>
  {#if value}
    <p class="value">{value}</p>
  {/if}
  {#if copy}
    <p class="copy">{copy}</p>
  {/if}
  {#if cta}
    <div class="cta">{@render cta()}</div>
  {/if}
</article>

<style>
  .card {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-3);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    background: var(--surface-1);
  }

  .title {
    margin: 0;
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    font-weight: 600;
    color: var(--text-dim);
  }

  .value {
    margin: 0;
    font-family: var(--font-mono);
    font-size: 1rem;
    color: var(--text);
    word-break: break-word;
  }

  .copy {
    margin: 0;
    font-size: 0.85rem;
    line-height: 1.5;
    color: var(--text-dim);
  }

  .cta {
    margin-top: var(--space-1);
  }
</style>
