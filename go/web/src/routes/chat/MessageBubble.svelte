<script lang="ts">
  // One user or assistant message. User content is plain text; assistant
  // content is markdown rendered through the shared sanitizing pipeline
  // (lib/markdown — html:false + DOMPurify + ctx: rewrite + foreign-origin
  // hardening, design 04 §4.4). Tool messages are not rendered here (they
  // surface as ToolCallCard under the assistant turn that called them).
  import { renderMarkdown } from '../../lib/markdown/markdown'
  import { camoProxy } from '../../lib/markdown/camo'
  import type { Sensitivity } from '../../lib/chat/types'

  let {
    role,
    content,
    sensitivity = undefined,
    streaming = false,
  }: { role: 'user' | 'assistant'; content: string; sensitivity?: Sensitivity; streaming?: boolean } = $props()

  const html = $derived(role === 'assistant' ? renderMarkdown(content) : '')
</script>

<div class="msg {role}">
  <div class="meta">
    <span class="role">{role}</span>
    {#if sensitivity && sensitivity !== 'public'}
      <span class="sens {sensitivity}">{sensitivity}</span>
    {/if}
  </div>
  {#if role === 'assistant'}
    <!-- eslint-disable-next-line svelte/no-at-html-tags — sanitized by renderMarkdown -->
    <!-- use:camoProxy swaps remote-image placeholders for proxied same-origin <img> post-render (design 07 §4.3) -->
    <div class="body md" use:camoProxy={html}>{@html html}{#if streaming}<span class="cursor"></span>{/if}</div>
  {:else}
    <div class="body user-text">{content}</div>
  {/if}
</div>

<style>
  .msg {
    padding: var(--space-2) var(--space-3);
    border-radius: var(--radius);
    border: 1px solid var(--border);
  }
  .msg.user {
    background: var(--surface-2);
    align-self: flex-end;
    max-width: 85%;
  }
  .msg.assistant {
    background: var(--surface-1);
  }
  .meta {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    margin-bottom: var(--space-1);
  }
  .role {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-faint);
  }
  .sens {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    text-transform: uppercase;
    border-radius: var(--radius);
    padding: 0 var(--space-1);
    color: var(--text-dim);
    border: 1px solid var(--border-strong);
  }
  .sens.credentials {
    color: var(--danger);
    border-color: var(--danger-dim);
  }
  .sens.personal {
    color: var(--warn);
    border-color: var(--warn);
  }
  .user-text {
    white-space: pre-wrap;
    word-break: break-word;
  }
  .cursor {
    display: inline-block;
    width: 0.5rem;
    height: 1rem;
    margin-left: 2px;
    background: var(--accent);
    vertical-align: text-bottom;
    animation: blink 1s steps(2, start) infinite;
  }
  @keyframes blink {
    50% {
      opacity: 0;
    }
  }
  /* Markdown body: keep it readable, inherit the mono/ui tokens. */
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
    /* Parent-relative inline-code scale — em (not the rem scale) so it tracks
       the surrounding message text. Q8 em-Konsolidierung (05-§7 Q8 row): the
       former 0.85em folds onto var(--fs-code-rel) (0.875em, = app.css code),
       the +0.025em pixel-delta is the documented Doc decision. */
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
  /* Pipeline-emitted markers (lib/markdown): blocked remote image + 256 KB
     truncation notice (design 04 §4.4.2/§4.4.3). */
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
