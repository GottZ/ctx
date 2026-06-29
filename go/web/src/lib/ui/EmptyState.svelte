<script lang="ts" module>
  // Generic empty-state (design 06-role-nav.md §3/§59, Welle N8). A centred,
  // dezent panel shown area-wide when a surface has nothing to show — an empty
  // corpus, a search with no match, a graph with no clusters yet. It carries a
  // title, optional explanatory copy, and an optional CTA slot the consumer
  // fills with a corpus entry-point (store / ask / browse), caps-gated by the
  // caller wherever a write affordance is involved (a read-only key gets no
  // store-CTA — BlocksPage gates the slot on home_scope).
  //
  // The component owns no layout context: it self-centres within whatever box the
  // consumer gives it — a content-flow column (BlocksPage list), a flex pane
  // (ChatPage conversation), or a positioned overlay over a canvas (OverviewMap).
  // Token-only, no DOM logic — mirrors CapabilityCard / IdentityBadge.
  //
  // a11y: the wrapper is a `role="status"` live region so a screen reader
  // announces the empty result when it replaces a list/thread, the title leading
  // the announcement (same contract as the prior inline `<p role="status">`
  // empties it supersedes).
</script>

<script lang="ts">
  import type { Snippet } from 'svelte'

  let {
    title,
    copy = null,
    cta,
  }: {
    /** Short headline — what is empty (e.g. "No blocks yet"). */
    title: string
    /** Optional explanatory prose under the title. */
    copy?: string | null
    /** Optional call-to-action slot — a corpus-entry <a>/<button>. The consumer
     *  supplies (and caps-gates) it; omitted entirely for a read-only key or a
     *  surface with no user action (e.g. the periodically-rebuilt cluster map). */
    cta?: Snippet
  } = $props()
</script>

<div class="empty-state" role="status">
  <p class="title">{title}</p>
  {#if copy}<p class="copy">{copy}</p>{/if}
  {#if cta}<div class="cta">{@render cta()}</div>{/if}
</div>

<style>
  /* Self-centring within the consumer's box: margin:auto consumes free space in a
     flex parent (ChatPage's conversation column) → vertical+horizontal centre; in
     a plain block flow (BlocksPage list) it centres horizontally and sits at the
     top. max-inline-size keeps the copy at a readable measure. Dezent: dim text,
     no heavy chrome of its own — a consumer that needs a frame over busy
     surroundings (OverviewMap's canvas overlay) supplies the card backing. */
  .empty-state {
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    gap: var(--space-2);
    margin: auto;
    max-inline-size: 28rem;
    padding: var(--space-4) var(--space-3);
    text-align: center;
  }
  .title {
    margin: 0;
    font-size: 0.95rem;
    font-weight: 600;
    color: var(--text);
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
