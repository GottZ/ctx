<script lang="ts">
  // One block of a recording (issue #575): its kind, role, name, tool call id,
  // media type and size; the ORIGINAL — the reference — as plain text (a
  // <pre> with Svelte text interpolation: never {@html}, never Markdown), more
  // of it on demand in 64 KiB windows; a raw view of its bytes; and, for a
  // block with text, a translation by a model of this instance NEXT TO it.
  //
  // The translation is Markdown and renders ONLY through the app's one
  // sanitizing renderer, lib/markdown/Markdown.svelte (markdown-it html:false
  // + DOMPurify, remote images as placeholders that load through the image
  // proxy only on a click, foreign links with rel="noopener noreferrer") —
  // the chat's renderer, no second one (e2e/contract/html-sinks.test.ts).
  // It is marked as generated, lives in this component's state only and is
  // stored nowhere. An image is never an <img> here: a payload shows its media
  // type and size, a remote image its URL as text.
  import { tick } from 'svelte'
  import Markdown from '../../lib/markdown/Markdown.svelte'
  import { RECORDING_LANGUAGES } from '../../lib/api/recordings'
  import { fmtBytes } from './recording-data'
  import RecordingBytes from './RecordingBytes.svelte'
  import type { BlockModel } from './recordings.svelte'

  let {
    model,
    language,
  }: {
    model: BlockModel
    /**
     * The target language of a translation started here — the page holds ONE
     * select for all blocks (a chat of thousands of messages would otherwise
     * carry thousands of 28-option selects). Absent: the block's own default.
     */
    language?: string
  } = $props()

  const b = $derived(model.block)
  const r = $derived(model.reader)
  const t = $derived(model.translation)

  const title = $derived(b.role ? `${b.role} · ${b.kind}` : b.kind)
  /** Bytes the original shows: the preview's until a window was read. */
  const previewBytes = $derived(new TextEncoder().encode(b.preview).length)
  const shownBytes = $derived(r.loaded > 0 ? r.loaded : previewBytes)

  let pre: HTMLPreElement | undefined = $state()

  async function loadMore(): Promise<void> {
    await r.more()
    if (r.eof) {
      await tick()
      pre?.focus()
    }
  }

  function toggleRaw(): void {
    model.raw = !model.raw
    if (model.raw && r.loaded === 0 && b.bytes > 0) void r.more()
  }

  function translate(): void {
    if (t === null) return
    if (language !== undefined) t.language = language
    void t.start()
  }

  const progress = $derived.by(() => {
    if (t === null) return ''
    const n = t.chunks
    switch (t.status) {
      case 'running':
        return n === null ? 'translating part 1…' : `translating part ${t.done + 1} of ${n}…`
      case 'done':
        return n === 1 ? 'translated' : `translated in ${n ?? t.done} parts`
      case 'stopped':
        return `stopped after ${t.done} of ${n ?? '?'} parts`
      case 'error':
        return 'translation failed'
      default:
        return ''
    }
  })
</script>

<article class="block" data-block={b.address} aria-label={`block ${b.address}`}>
  <header class="head">
    <div class="ident">
      <span class="title">{title}</span>
      <code class="addr">{b.address}</code>
      {#if b.name}<span class="meta">name <code>{b.name}</code></span>{/if}
      {#if b.tool_call_id}<span class="meta">call <code>{b.tool_call_id}</code></span>{/if}
      {#if b.media_type}<span class="meta">{b.media_type}</span>{/if}
      {#if b.labels_truncated}<span class="chip" data-labels-cut title="A role, name, call id or media type was cut at 256 bytes; the raw body holds it whole">labels cut</span>{/if}
      <span class="meta">{fmtBytes(b.bytes)}</span>
      {#if !b.utf8}<span class="chip">not UTF-8</span>{/if}
    </div>
    <div class="actions">
      {#if b.utf8}
        <button type="button" aria-pressed={model.raw} onclick={toggleRaw}>Raw</button>
      {/if}
      {#if t !== null}
        {#if t.status === 'running'}
          <button type="button" onclick={() => t.stop()}>Stop</button>
        {:else}
          <button type="button" onclick={translate}>Translate</button>
        {/if}
        <span class="progress" role="status">{progress}</span>
      {/if}
    </div>
  </header>

  <div class="panes" class:two={t !== null && t.status !== 'idle'}>
    <section class="pane" aria-label={`original of ${b.address}`}>
      {#if !b.utf8 || model.raw}
        <RecordingBytes reader={r} label={`bytes of ${b.address}`} size={b.bytes} />
      {:else if !b.inline}
        <p class="note" data-payload>
          {b.media_type ?? b.kind} · {fmtBytes(b.bytes)} — a payload, not shown inline; its bytes are in the raw view.
        </p>
      {:else if b.preview_omitted && r.loaded === 0}
        <p class="note">No preview of this block in the answer ({fmtBytes(b.bytes)}).</p>
        <button type="button" onclick={() => void loadMore()} disabled={r.status === 'loading'}>
          {r.status === 'loading' ? 'loading…' : 'Load'}
        </button>
      {:else}
        <!-- the original as TEXT: Svelte escapes it, nothing in it is ever markup -->
        <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
        <pre bind:this={pre} tabindex="0" role="region" aria-label={`text of ${b.address}, scrollable`} data-original
          >{model.originalText}</pre
        >
        {#if shownBytes < b.bytes}
          <p class="more">
            <span>{fmtBytes(shownBytes)} of {fmtBytes(b.bytes)} shown</span>
            <button type="button" onclick={() => void loadMore()} disabled={r.status === 'loading'}>
              {r.status === 'loading' ? 'loading…' : 'Load more'}
            </button>
          </p>
        {/if}
      {/if}
      {#if r.error !== null && !model.raw && b.utf8}
        <p class="err" role="alert" data-error-code={r.error.code}>{r.error.text}</p>
      {/if}
    </section>

    {#if t !== null && t.status !== 'idle'}
      <section class="pane translation" aria-label={`translation of ${b.address}`} aria-busy={t.status === 'running'} data-translation>
        <p class="generated">
          <strong>Generated by a model — the original is the reference.</strong>
          {RECORDING_LANGUAGES[t.language] ?? t.language}{#if t.model !== null}, model <code>{t.model}</code>{/if}.
        </p>
        {#each t.parts as part, i (i)}
          <div class="part"><Markdown source={part} /></div>
        {/each}
        {#if t.error !== null}
          <p class="err" role="alert" data-error-code={t.error.code}>{t.error.text}</p>
        {/if}
      </section>
    {/if}
  </div>
</article>

<style>
  .block {
    container-type: inline-size;
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border-top: 1px solid var(--border);
  }
  .head {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
    align-items: center;
    justify-content: space-between;
  }
  .ident,
  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
    align-items: center;
    min-width: 0;
  }
  .title {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    font-weight: var(--fw-semibold);
  }
  .addr,
  code {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    overflow-wrap: anywhere;
  }
  .meta,
  .progress {
    color: var(--text-dim);
    font-size: var(--fs-xs);
  }
  .chip {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    border: 1px dashed var(--warn);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
  }
  .panes {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: var(--space-3);
  }
  .panes.two {
    grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
  }
  /* narrow: the translation stacks under the original */
  @container (max-width: 40rem) {
    .panes.two {
      grid-template-columns: minmax(0, 1fr);
    }
  }
  .pane {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    align-items: flex-start;
    min-width: 0;
  }
  pre {
    align-self: stretch;
    margin: 0;
    max-height: 28rem;
    overflow: auto;
    padding: var(--space-2);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    background: var(--surface-0);
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
  }
  .translation {
    align-items: stretch;
    padding: var(--space-2);
    border: 1px dashed var(--accent);
    border-radius: var(--radius);
    font-size: var(--fs-sm);
    overflow-wrap: anywhere;
  }
  .generated {
    margin: 0;
    color: var(--text-dim);
    font-size: var(--fs-xs);
  }
  .generated strong {
    color: var(--text);
    font-weight: var(--fw-semibold);
  }
  .part + .part {
    border-top: 1px dotted var(--border);
    padding-top: var(--space-1);
  }
  .note,
  .more {
    margin: 0;
    color: var(--text-dim);
    font-size: var(--fs-sm);
  }
  .more {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
    align-items: center;
  }
  .err {
    margin: 0;
    color: var(--text);
    font-size: var(--fs-sm);
    border-left: 2px solid var(--danger);
    padding-left: var(--space-2);
  }
</style>
