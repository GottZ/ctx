<script lang="ts">
  // LLM telemetry table — its own Resource over GET /api/llmlog (separate
  // endpoint, own filters). The LIST response NEVER carries prompt/response
  // bodies; error is the normalized {class, detail} (capped server-side). The
  // 091 dispatch-telemetry columns (queue_wait_ms/dispatch_class/dispatch_abort)
  // are pure Lease measurands and ride the body-free list. Bodies live ONLY
  // behind the gated per-id detail fetch (LlmlogDetailModel) reached by a row
  // click — and are never cached past the open card (D1b). The scroll+table
  // shell comes from the shared lib/ui/Table primitive (Q10).
  import { onMount, untrack } from 'svelte'
  import { fetchLLMLog } from '../../lib/api/status'
  import type { LLMLogEntry, LLMLogResponse } from '../../lib/api/types'
  import { Resource } from '../../lib/resource.svelte'
  import Modal from '../../lib/ui/Modal.svelte'
  import Table from '../../lib/ui/Table.svelte'
  import { fmtMs } from './dispatch'
  import { LlmlogDetailModel } from './llmlog-detail.svelte'

  // `live` carries SSE-pushed llmcall rows (G34); merged on top of the fetched
  // history below. Unfiltered upstream, so the current pipeline/errors filter
  // is reapplied client-side, and the fetched ids dedup the overlap. `refetch`
  // is the coalescing counterpart (S0): a tick above events.llmcall_coalesce_
  // threshold pushes NO rows, only a count — the table reloads its own page.
  let {
    complete,
    live = [],
    refetch = 0,
  }: { complete: boolean; live?: LLMLogEntry[]; refetch?: number } = $props()

  let pipeline = $state('')
  let errorsOnly = $state(false)

  const log = new Resource<LLMLogResponse>(() =>
    fetchLLMLog({ limit: 50, pipeline: pipeline.trim() || undefined, errorsOnly }),
  )
  onMount(() => void log.load())

  // One reload per raised token — untrack keeps the reload's own state reads out
  // of the dependency set, so the effect fires on the signal and nothing else.
  $effect(() => {
    if (refetch > 0) untrack(() => void log.reload())
  })

  // The gated body fetch behind a row click. Body-free-at-rest: the model holds
  // the fetched prompt/reply ONLY while its card is open and drops them on close.
  const detail = new LlmlogDetailModel()

  function onRowActivate(id: string): void {
    void detail.open(id)
  }
  function onRowKey(e: KeyboardEvent, id: string): void {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault()
      void detail.open(id)
    }
  }
  // The human-readable reason a body is absent (sealed = credentials-class row,
  // never stored; evicted = retention removed once-stored bodies; bodyless =
  // this pipeline never records prompt/reply — llmlog W1 split).
  function bodyStateReason(state: string): string {
    if (state === 'sealed') return 'sealed — credentials-class call, prompt/reply never stored'
    if (state === 'evicted') return 'evicted — bodies removed by retention'
    if (state === 'bodyless') return 'bodyless — this pipeline never records prompt/reply (embed, translate, rejection lines)'
    return ''
  }

  const entries = $derived.by(() => {
    const fetched = log.data?.entries ?? []
    const ids = new Set(fetched.map((e) => e.id))
    const p = pipeline.trim()
    const liveMatch = live.filter(
      (e) => !ids.has(e.id) && (p === '' || e.pipeline === p) && (!errorsOnly || e.error !== null),
    )
    if (liveMatch.length === 0) return fetched
    return [...liveMatch, ...fetched].sort((a, b) => (a.created_at < b.created_at ? 1 : -1)).slice(0, 50)
  })
  const anyCost = $derived(entries.some((e) => e.cost_usd !== null))

  function fmtTime(ts: string): string {
    return new Date(ts).toLocaleTimeString()
  }
  function fmtCost(c: number | null): string {
    return c === null ? '—' : `$${c.toFixed(5)}`
  }
</script>

<section class="card" aria-label="llm telemetry">
  <header>
    <h2>llm calls · 24h sample</h2>
    <div class="filters">
      <input
        type="text"
        placeholder="pipeline filter"
        bind:value={pipeline}
        oninput={() => void log.reload()}
      />
      <label>
        <input type="checkbox" bind:checked={errorsOnly} onchange={() => void log.reload()} />
        errors only
      </label>
    </div>
  </header>

  {#if !complete}
    <p class="disclaimer" role="status">
      telemetry incomplete: rerank/embed/translate are unlogged and fallback calls log as primary
      (F3-P2/P7)
    </p>
  {/if}

  {#if log.status === 'loading' || log.status === 'idle'}
    <p class="state" aria-busy="true">loading…</p>
  {:else if log.status === 'error'}
    <div class="state error" role="alert">
      <span>{log.error?.message}</span>
      <button type="button" onclick={() => void log.reload()}>Retry</button>
    </div>
  {:else}
    <Table empty={entries.length === 0} valign="baseline">
      {#snippet emptyState()}
        <p class="state">no calls match.</p>
      {/snippet}
      {#snippet head()}
        <tr>
          <th>time</th><th>pipeline</th><th>model</th><th>backend</th>
          <th class="num">ms</th><th class="num">tok in/out</th>
          {#if anyCost}<th class="num">cost</th>{/if}
          <th class="num">wait</th><th>class</th>
          <th>error</th>
        </tr>
      {/snippet}
      {#each entries as e (e.id)}
        <tr
          class:has-error={e.error !== null}
          class:open={detail.openId === e.id}
          class="clickable"
          role="button"
          tabindex="0"
          aria-expanded={detail.openId === e.id}
          title="show prompt / reply"
          onclick={() => onRowActivate(e.id)}
          onkeydown={(ev) => onRowKey(ev, e.id)}
        >
          <td class="mono">{fmtTime(e.created_at)}</td>
          <td class="mono">{e.pipeline}</td>
          <td class="mono dim">{e.model}</td>
          <td class="mono">{e.backend}</td>
          <td class="num">{e.duration_ms ?? '—'}</td>
          <td class="num dim">{e.prompt_tokens ?? '—'}/{e.completion_tokens ?? '—'}</td>
          {#if anyCost}<td class="num">{fmtCost(e.cost_usd)}</td>{/if}
          <td class="num dim">{e.queue_wait_ms === null ? '—' : fmtMs(e.queue_wait_ms)}</td>
          <td class="mono dim">
            {#if e.dispatch_class}<span class="dcls">{e.dispatch_class}</span>{:else}—{/if}
            {#if e.dispatch_abort}<span class="abort">{e.dispatch_abort}</span>{/if}
          </td>
          <td class="errcell">
            {#if e.error}
              <span class="errcls">{e.error.class}</span>
              <span class="errdetail" title={e.error.detail}>{e.error.detail}</span>
            {:else}
              —
            {/if}
          </td>
        </tr>
      {/each}
    </Table>
  {/if}
</section>

{#if detail.openId}
  <Modal width="44rem" onclose={() => detail.close()} ariaLabelledby="llmlog-detail-title">
    <div class="detail">
      <header class="detail-head">
        <h3 id="llmlog-detail-title">llm call · prompt / reply</h3>
        <button type="button" class="close" onclick={() => detail.close()}>close</button>
      </header>

      {#if detail.status === 'loading' || detail.status === 'idle'}
        <p class="state" aria-busy="true">loading…</p>
      {:else if detail.status === 'error'}
        {@const err = detail.error}
        <div class="state error" role="alert">
          {#if err?.status === 404}
            <span>not found — the row is gone or not yours to view.</span>
          {:else if err?.status === 403}
            <span>forbidden — this key may not read call bodies.</span>
          {:else}
            <span>{err?.message ?? 'failed to load detail'}</span>
          {/if}
          {#if err?.requestId}<span class="request-id">request {err.requestId}</span>{/if}
        </div>
      {:else if detail.detail}
        {@const d = detail.detail}
        <dl class="meta">
          <div><dt>pipeline</dt><dd class="mono">{d.pipeline}</dd></div>
          <div><dt>model</dt><dd class="mono">{d.model}</dd></div>
          <div><dt>backend</dt><dd class="mono">{d.backend}</dd></div>
          <div><dt>sensitivity</dt><dd class="mono">{d.required_sensitivity || '—'}</dd></div>
        </dl>

        {#if d.body_state !== 'present'}
          <p class="state note" role="status">{bodyStateReason(d.body_state)}</p>
        {:else}
          <section class="body">
            <h4>system prompt</h4>
            <pre>{d.request_system ?? '—'}</pre>
          </section>
          <section class="body">
            <h4>user prompt</h4>
            <pre>{d.request_user ?? '—'}</pre>
          </section>
          <section class="body">
            <h4>response</h4>
            <pre>{d.response_content ?? '—'}</pre>
          </section>
        {/if}
      {/if}
    </div>
  </Modal>
{/if}

<style>
  .card {
    background: var(--surface-1);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
  header {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    flex-wrap: wrap;
    padding: var(--space-2) var(--space-3);
    border-bottom: 1px solid var(--border);
  }
  h2 {
    margin: 0;
    font-family: var(--font-mono);
    font-size: var(--fs-md);
    font-weight: var(--fw-semibold);
  }
  .filters {
    margin-left: auto;
    display: flex;
    align-items: center;
    gap: var(--space-3);
    font-size: var(--fs-sm);
    color: var(--text-dim);
  }
  .filters input[type='text'] {
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-2);
    width: 12rem;
  }
  .filters label {
    display: flex;
    align-items: center;
    gap: var(--space-1);
    cursor: pointer;
  }
  .disclaimer {
    margin: 0;
    padding: var(--space-1) var(--space-3);
    color: var(--warn);
    font-size: var(--fs-xs);
    border-bottom: 1px solid var(--border);
  }
  .state {
    margin: 0;
    padding: var(--space-3);
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
  }
  .state.error {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    color: var(--danger);
  }
  .mono {
    font-family: var(--font-mono);
    white-space: nowrap;
  }
  .dim {
    color: var(--text-dim);
  }
  .num {
    text-align: right;
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }
  tr.has-error {
    background: var(--danger-dim);
  }
  .errcell {
    max-width: 22rem;
  }
  .errcls {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    color: var(--danger);
    text-transform: uppercase;
    margin-right: var(--space-2);
  }
  .errdetail {
    color: var(--text-dim);
    font-size: var(--fs-xs);
  }
  tr.clickable {
    cursor: pointer;
  }
  tr.clickable:hover,
  tr.clickable:focus-visible {
    background: var(--surface-2);
    outline: none;
  }
  tr.clickable.open {
    background: var(--accent-dim);
  }
  .dcls {
    text-transform: uppercase;
    font-size: var(--label-size);
  }
  .abort {
    margin-left: var(--space-1);
    color: var(--warn);
    font-size: var(--fs-xs);
    text-transform: uppercase;
  }
  .detail {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  .detail-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
  }
  .detail-head h3 {
    margin: 0;
    font-family: var(--font-mono);
    font-size: var(--fs-md);
  }
  .close {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    padding: var(--space-1) var(--space-2);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    background: var(--surface-2);
    color: var(--text-dim);
    cursor: pointer;
  }
  .request-id {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    color: var(--text-dim);
  }
  .note {
    color: var(--warn);
    padding: var(--space-2) 0;
  }
  .meta {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(10rem, 1fr));
    gap: var(--space-2);
    margin: 0;
  }
  .meta div {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .meta dt {
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
  }
  .meta dd {
    margin: 0;
  }
  .body h4 {
    margin: 0 0 var(--space-1);
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-faint);
  }
  .body pre {
    margin: 0;
    max-height: 18rem;
    overflow: auto;
    padding: var(--space-2);
    background: var(--surface-2);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    white-space: pre-wrap;
    word-break: break-word;
  }
</style>
