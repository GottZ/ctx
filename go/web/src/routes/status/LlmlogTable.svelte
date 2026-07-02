<script lang="ts">
  // LLM telemetry table — its own Resource over GET /api/llmlog (separate
  // endpoint, own filters). The response NEVER carries prompt/response bodies;
  // error is the normalized {class, detail} (capped server-side).
  import { onMount } from 'svelte'
  import { fetchLLMLog } from '../../lib/api/status'
  import type { LLMLogEntry, LLMLogResponse } from '../../lib/api/types'
  import { Resource } from '../../lib/resource.svelte'

  // `live` carries SSE-pushed llmcall rows (G34); merged on top of the fetched
  // history below. Unfiltered upstream, so the current pipeline/errors filter
  // is reapplied client-side, and the fetched ids dedup the overlap.
  let { complete, live = [] }: { complete: boolean; live?: LLMLogEntry[] } = $props()

  let pipeline = $state('')
  let errorsOnly = $state(false)

  const log = new Resource<LLMLogResponse>(() =>
    fetchLLMLog({ limit: 50, pipeline: pipeline.trim() || undefined, errorsOnly }),
  )
  onMount(() => void log.load())

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
  {:else if entries.length === 0}
    <p class="state">no calls match.</p>
  {:else}
    <div class="scroll">
      <table>
        <thead>
          <tr>
            <th>time</th><th>pipeline</th><th>model</th><th>backend</th>
            <th class="num">ms</th><th class="num">tok in/out</th>
            {#if anyCost}<th class="num">cost</th>{/if}
            <th>error</th>
          </tr>
        </thead>
        <tbody>
          {#each entries as e (e.id)}
            <tr class:has-error={e.error !== null}>
              <td class="mono">{fmtTime(e.created_at)}</td>
              <td class="mono">{e.pipeline}</td>
              <td class="mono dim">{e.model}</td>
              <td class="mono">{e.backend}</td>
              <td class="num">{e.duration_ms ?? '—'}</td>
              <td class="num dim">{e.prompt_tokens ?? '—'}/{e.completion_tokens ?? '—'}</td>
              {#if anyCost}<td class="num">{fmtCost(e.cost_usd)}</td>{/if}
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
        </tbody>
      </table>
    </div>
  {/if}
</section>

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
  .scroll {
    overflow-x: auto;
  }
  table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--fs-sm);
  }
  th {
    text-align: left;
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
  td {
    padding: var(--space-1) var(--space-3);
    border-bottom: 1px solid var(--surface-2);
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
</style>
