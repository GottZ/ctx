<script lang="ts">
  // The ONE sanitized markdown sink of the app (design 04 §4.4 / §9.6, wave
  // U06). Every surface that renders untrusted markdown source — the chat
  // assistant turn, the issue body, every issue comment — renders THROUGH this
  // component, so there is exactly one place a raw string becomes DOM and it is
  // always the renderMarkdown pipeline (markdown-it html:false + DOMPurify +
  // ctx: rewrite + foreign-origin hardening + remote-image placeholder + 256 KB
  // cap). §9.6: the issue-detail renderer and the chat renderer are NOT two
  // markdown paths — they are this one component.
  //
  // The {@html} in this file is THE frozen sink (e2e/contract/html-sinks.test.ts).
  // It is safe by construction: its only input is renderMarkdown's output, which
  // is DOMPurify-sanitized. Adding a second raw {@html} anywhere turns that
  // freeze red before review.
  import { renderMarkdown } from './markdown'

  let { source }: { source: string } = $props()
  const html = $derived(renderMarkdown(source))
</script>

<!-- eslint-disable-next-line svelte/no-at-html-tags — sanitized by renderMarkdown (html:false + DOMPurify) -->
<div class="md">{@html html}</div>

<style>
  /* Shared markdown body styling — the readable defaults + the pipeline-emitted
     markers (blocked remote image, 256 KB truncation notice). Kept in sync with
     the chat MessageBubble .md block so both surfaces render identically. */
  .md :global(p) {
    margin: 0 0 var(--space-2);
  }
  .md :global(p:last-child) {
    margin-bottom: 0;
  }
  .md :global(pre) {
    background: var(--surface-0);
    padding: var(--space-2);
    border-radius: var(--radius);
    overflow: auto;
    font-size: var(--fs-sm);
  }
  .md :global(code) {
    font-family: var(--font-mono);
    font-size: var(--fs-code-rel);
  }
  .md :global(a) {
    color: var(--accent);
  }
  .md :global(ul),
  .md :global(ol) {
    margin: 0 0 var(--space-2);
    padding-left: var(--space-4);
  }
  .md :global(h1),
  .md :global(h2),
  .md :global(h3) {
    margin: var(--space-3) 0 var(--space-2);
    font-weight: var(--fw-semibold);
    line-height: var(--lh-heading);
  }
  .md :global(h1) {
    font-size: var(--fs-lg);
  }
  .md :global(h2) {
    font-size: var(--fs-base);
  }
  .md :global(h3) {
    font-size: var(--fs-sm);
  }
  .md :global(blockquote) {
    margin: 0 0 var(--space-2);
    padding-left: var(--space-3);
    border-left: 2px solid var(--border-strong);
    color: var(--text-dim);
  }
  .md :global(.md-img-blocked) {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    color: var(--text-dim);
    border: 1px dashed var(--border-strong);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
    word-break: break-all;
  }
  .md :global(.md-truncated) {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    color: var(--warn);
    border-top: 1px dashed var(--border-strong);
    padding-top: var(--space-1);
  }
</style>
