<script lang="ts">
  // Usage panel of one consumer (RC-D C6, wave W05-10; design/05 §4.6):
  // window switch 24h · 7d · 30d · 90d, SeriesBars chart and — rendered from
  // the SAME rows (usage-format.ts usageTableRows) — a table alternative for
  // screen readers. null / complete:false render "not recorded" (never 0);
  // hist:false shows the marked mean (Ø); auth refusals are never a number
  // per consumer. State: UsageModel (usage.svelte.ts), 60 s visible-only poll.
  import { untrack } from 'svelte'
  import type { UsageWindow } from '../../lib/api/types'
  import Table from '../../lib/ui/Table.svelte'
  import { UsageModel } from './usage.svelte'
  import { MEAN_MARK, totalText, usageTableRows, windowTotal } from './usage-format'
  import { fmtMinute } from './format'
  import SeriesBars from './SeriesBars.svelte'

  let { consumerId }: { consumerId: string } = $props()

  const WINDOWS: readonly UsageWindow[] = ['24h', '7d', '30d', '90d']
  const model = $derived(new UsageModel(consumerId))

  $effect(() => {
    const m = model
    untrack(() => m.start())
    return () => m.dispose()
  })

  // group=consumer&consumer=<id>: at most one series, keyed by the consumer
  const series = $derived(model.series.find((s) => s.key === consumerId) ?? model.series[0] ?? null)
  const rows = $derived(series === null ? [] : usageTableRows(series, model.hist === true, true))
  const points = $derived(series?.points ?? [])
</script>

<section class="card" aria-label="usage">
  <header class="card-head">
    <h2>usage</h2>
    <!-- one tab stop, arrows move inside (the focus walk counts a radiogroup once) -->
    <fieldset class="windows" role="radiogroup" aria-label="usage window">
      <legend class="sr-only">window</legend>
      {#each WINDOWS as w (w)}
        <label class="win">
          <input
            type="radio"
            name="usage-window-{consumerId}"
            value={w}
            checked={model.window === w}
            onchange={() => void model.setWindow(w)}
          />
          {w}
        </label>
      {/each}
    </fieldset>
  </header>
  {#if model.status === 'idle' || model.status === 'loading'}
    <p class="state" aria-busy="true">loading usage…</p>
  {:else if model.status === 'error' && series === null}
    <div class="error" role="alert">
      <p>{model.loadError?.message ?? 'usage unavailable'}</p>
      <button type="button" onclick={() => void model.load()}>Retry</button>
    </div>
  {:else if series === null || points.length === 0}
    <p class="state" role="status">no usage recorded for this window</p>
  {:else}
    <dl class="totals">
      <dt>calls</dt>
      <dd data-total="calls">{totalText(windowTotal(points, 'calls'))}</dd>
      <dt>errors</dt>
      <dd data-total="errors">{totalText(windowTotal(points, 'errors'))}</dd>
      <dt>prompt tokens</dt>
      <dd data-total="prompt_tokens">{totalText(windowTotal(points, 'prompt_tokens'))}</dd>
      <dt>completion tokens</dt>
      <dd data-total="completion_tokens">{totalText(windowTotal(points, 'completion_tokens'))}</dd>
    </dl>
    {#if model.stale}
      <p class="note" role="status">Refresh failed ({model.loadError?.message ?? 'error'}) — showing the last loaded window.</p>
    {/if}
    {#if model.complete === false}
      <p class="note" role="status">Some buckets are not recorded — they read "not recorded", never 0.</p>
    {/if}
    {#if model.hist === false}
      <p class="note" role="status">No latency histograms for this window: {MEAN_MARK} marks the mean instead of p50/p95.</p>
    {/if}
    <SeriesBars {series} field="calls" />
    <Table label="usage table" focusableScroll>
      {#snippet head()}
        <tr>
          <th>bucket</th>
          <th>calls</th>
          <th>errors</th>
          <th>tokens</th>
          <th>latency{model.hist === true ? ' p50 / p95' : ''}</th>
          <th>queue</th>
          <th>ttft</th>
          <th>refusals</th>
        </tr>
      {/snippet}
      {#each rows as r (r.bucketStart)}
        <tr>
          <td data-col="bucket">{fmtMinute(r.bucketStart)}</td>
          <td data-col="calls" class:nr={!r.calls.recorded}>{r.calls.text}</td>
          <td data-col="errors" class:nr={!r.errors.recorded}>{r.errors.text}</td>
          <td data-col="tokens" class:nr={!r.tokens.recorded}>{r.tokens.text}</td>
          <td data-col="latency" class:nr={!r.latency.total.recorded}>{r.latency.total.text}</td>
          <td data-col="queue" class:nr={!r.latency.queue.recorded}>{r.latency.queue.text}</td>
          <td data-col="ttft" class:nr={!r.latency.ttft.recorded}>{r.latency.ttft.text}</td>
          <td data-col="rejections">
            {#if r.rejections.length === 0}
              {r.rejectionsText}
            {:else}
              {#each r.rejections as x (x.reason)}
                <span class="rej" data-reject-reason={x.reason}>{x.reason} {x.text}</span>
              {/each}
            {/if}
          </td>
        </tr>
      {/each}
    </Table>
  {/if}
</section>

<style>
  .card {
    background: var(--surface-1);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
  .card-head {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border-bottom: 1px solid var(--border);
  }
  h2 {
    margin: 0;
    font-family: var(--font-mono);
    font-size: var(--fs-md);
    font-weight: var(--fw-semibold);
  }
  .windows {
    display: flex;
    gap: var(--space-2);
    margin: 0 0 0 auto;
    padding: 0;
    border: 0;
    font-size: var(--fs-sm);
  }
  .win {
    display: inline-flex;
    gap: var(--space-1);
    align-items: center;
  }
  .sr-only {
    position: absolute;
    width: 1px;
    height: 1px;
    overflow: hidden;
    clip-path: inset(50%);
    white-space: nowrap;
  }
  .state,
  .note {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    color: var(--text-dim);
    font-size: var(--fs-sm);
  }
  .error {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    align-items: flex-start;
    margin: var(--space-2) var(--space-3);
    border: 1px solid var(--danger);
    border-radius: var(--radius);
    background: var(--danger-dim);
    padding: var(--space-2) var(--space-3);
    font-size: var(--fs-sm);
  }
  .error p {
    margin: 0;
    color: var(--danger);
  }
  .totals {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-1) var(--space-3);
    margin: 0;
    padding: var(--space-2) var(--space-3);
    font-size: var(--fs-sm);
  }
  .totals dt {
    color: var(--text-dim);
  }
  .totals dd {
    margin: 0 var(--space-2) 0 0;
    font-family: var(--font-mono);
  }
  .nr {
    color: var(--text-faint);
  }
  .rej {
    margin-right: var(--space-1);
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
  }
</style>
