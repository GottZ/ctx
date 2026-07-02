<script lang="ts">
  // Tool-call collapsible (design 06 §3.9): collapsed by default, header line
  // `ctx_query · "query" · 5 blocks · 1.8s`; expanded shows the arguments (JSON,
  // text only — never innerHTML) and the result block list, each linking to
  // /graph?focus=<id>. The full tool output lives in the session API; the
  // stream/history carry the block previews.
  import type { ToolResultEvent } from '../../lib/chat/types'

  let {
    name,
    args = {},
    result = undefined,
  }: { name: string; args?: Record<string, unknown>; result?: ToolResultEvent } = $props()

  let open = $state(false)

  // A short header hint from the most telling argument.
  const hint = $derived.by(() => {
    const v = (args.query ?? args.id ?? args.category) as unknown
    return typeof v === 'string' && v !== '' ? `"${v}"` : ''
  })
  const argsText = $derived(JSON.stringify(args, null, 2))
</script>

<div class="tool" class:err={result && !result.ok}>
  <button class="head" type="button" aria-expanded={open} onclick={() => (open = !open)}>
    <span class="caret" class:open>▸</span>
    <span class="tname">{name}</span>
    {#if hint}<span class="hint">{hint}</span>{/if}
    {#if result}
      <span class="sep">·</span>
      <span class="summary">{result.summary}</span>
      <span class="sep">·</span>
      <span class="ms">{result.duration_ms}ms</span>
      {#if result.truncated}<span class="trunc" title="result truncated">⋯</span>{/if}
    {:else}
      <span class="running">running…</span>
    {/if}
  </button>

  {#if open}
    <div class="body">
      <div class="label">arguments</div>
      <pre class="args">{argsText}</pre>
      {#if result?.blocks && result.blocks.length > 0}
        <div class="label">{result.blocks.length} block{result.blocks.length === 1 ? '' : 's'}</div>
        <ul class="blocks">
          {#each result.blocks as b (b.id)}
            <li>
              <a href={`/graph?focus=${encodeURIComponent(b.id)}`} title={b.id}>{b.title}</a>
              <span class="cat">{b.category}</span>
              {#if b.score != null}<span class="score">{b.score.toFixed(2)}</span>{/if}
            </li>
          {/each}
        </ul>
      {/if}
    </div>
  {/if}
</div>

<style>
  .tool {
    border: 1px solid var(--border);
    border-radius: var(--radius);
    background: var(--surface-1);
    margin: var(--space-1) 0;
    font-size: var(--fs-sm);
  }
  .tool.err {
    border-color: var(--danger-dim);
  }
  .head {
    display: flex;
    align-items: center;
    gap: var(--space-1);
    width: 100%;
    text-align: left;
    background: none;
    border: none;
    padding: var(--space-1) var(--space-2);
    cursor: pointer;
    font-family: var(--font-mono);
    color: var(--text-dim);
  }
  .caret {
    transition: transform var(--dur-1) var(--ease);
    color: var(--text-faint);
  }
  .caret.open {
    transform: rotate(90deg);
  }
  .tname {
    color: var(--accent);
  }
  .hint {
    color: var(--text);
  }
  .sep {
    color: var(--text-faint);
  }
  .ms,
  .summary {
    color: var(--text-faint);
  }
  .running {
    color: var(--warn);
  }
  .trunc {
    color: var(--text-faint);
  }
  .body {
    padding: 0 var(--space-2) var(--space-2);
    border-top: 1px solid var(--surface-2);
  }
  .label {
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-faint);
    font-family: var(--font-mono);
    margin: var(--space-2) 0 var(--space-1);
  }
  .args {
    margin: 0;
    white-space: pre-wrap;
    word-break: break-word;
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    color: var(--text-dim);
    background: var(--surface-0);
    border-radius: var(--radius);
    padding: var(--space-2);
    max-height: 14rem;
    overflow: auto;
  }
  .blocks {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .blocks li {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
  }
  .blocks a {
    color: var(--accent);
    text-decoration: none;
  }
  .blocks a:hover {
    text-decoration: underline;
  }
  .cat {
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--label-size);
  }
  .score {
    color: var(--text-faint);
    font-variant-numeric: tabular-nums;
    margin-left: auto;
  }
</style>
