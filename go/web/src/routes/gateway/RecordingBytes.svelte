<script lang="ts">
  // The raw view of one address of a recording (issue #575): a block's bytes,
  // or a whole body (request / response), read in 64 KiB windows on demand
  // (WindowReader). Bytes that are valid UTF-8 show as text, decoded by one
  // streaming decoder; bytes that are not show as a hex dump with offsets —
  // lossless either way, and labelled which one it is. Text interpolation
  // only: nothing here is parsed as Markdown or HTML.
  import { tick } from 'svelte'
  import { fmtBytes } from './format'
  import type { WindowReader } from './recordings.svelte'

  let {
    reader,
    label,
    size,
  }: {
    reader: WindowReader
    /** What the bytes are (names the scroll region). */
    label: string
    /** The size the server stated before any window was read. */
    size: number
  } = $props()

  let pre: HTMLPreElement | undefined = $state()

  // Focus stays sane: the button sits below the bytes and stays while more
  // remain; when the last window arrived it disappears, so focus moves to the
  // bytes region instead of falling back to the page.
  async function more(): Promise<void> {
    await reader.more()
    if (reader.eof) {
      await tick()
      pre?.focus()
    }
  }

  const total = $derived(reader.total ?? size)
</script>

<div class="bytes" data-raw-view>
  <p class="kind">
    {reader.hexView ? 'raw bytes · hex (not valid UTF-8)' : 'raw bytes · UTF-8 text'} · {fmtBytes(reader.loaded)} of {fmtBytes(total)}
  </p>
  {#if reader.loaded > 0}
    <!-- A focusable scroll region is the WCAG-sanctioned exception to "no
         tabindex on a non-interactive element" (axe scrollable-region-focusable),
         as in lib/ui/Table.svelte. -->
    <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
    <pre
      bind:this={pre}
      class:hex={reader.hexView}
      tabindex="0"
      role="region"
      aria-label={`${label}, scrollable`}
      data-raw>{reader.hexView ? reader.hexText() : reader.text}</pre>
  {/if}
  {#if reader.error !== null}
    <p class="err" role="alert" data-error-code={reader.error.code}>{reader.error.text}</p>
  {/if}
  {#if !reader.eof && total > 0}
    <button type="button" onclick={() => void more()} disabled={reader.status === 'loading'}>
      {reader.status === 'loading' ? 'loading…' : reader.loaded === 0 ? 'Load bytes' : 'Load more'}
    </button>
  {/if}
</div>

<style>
  /* stretch: as a flex item of a block pane it takes the pane's width, so a
     hex line wider than a phone scrolls inside its <pre>, not the page */
  .bytes {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    align-items: flex-start;
    align-self: stretch;
    min-width: 0;
  }
  .kind {
    margin: 0;
    color: var(--text-dim);
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
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
  pre.hex {
    white-space: pre;
    overflow-wrap: normal;
  }
  .err {
    margin: 0;
    color: var(--text);
    font-size: var(--fs-sm);
    border-left: 2px solid var(--danger);
    padding-left: var(--space-2);
  }
</style>
