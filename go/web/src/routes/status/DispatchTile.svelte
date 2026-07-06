<script lang="ts">
  // Dispatch / admission-registry tile (inference-scheduler MW12b, design/05
  // §4.5). Renders whichever of the two wire shapes the server sent: a server-
  // admin gets the FULL `dispatch` (targets, wait aggregates, preempt/aged
  // counters, embed-token rollup, last-run stamps); a tenant-admin gets the
  // COARSENED `dispatch_tenant` (busy + a leer/niedrig/hoch depth bucket + its
  // OWN queue bucket — never a foreign count, F-B3). Exactly one is non-null;
  // when both are null (no dispatch source wired / pre-MW12 server) the tile
  // hides. All colour is via closed-set classes (Q3), never a style attribute.
  import type { DispatchStatus, DispatchTenantStatus } from '../../lib/api/types'
  import Table from '../../lib/ui/Table.svelte'
  import { depthClass, depthLabel, fmtMs, fmtTokens } from './dispatch'

  let {
    dispatch = null,
    dispatchTenant = null,
  }: {
    dispatch?: DispatchStatus | null
    dispatchTenant?: DispatchTenantStatus | null
  } = $props()

  function fmtStamp(ts: string | null): string {
    return ts === null ? 'never' : new Date(ts).toLocaleTimeString()
  }
</script>

{#if dispatch}
  {@const d = dispatch}
  <section class="card" aria-label="dispatch registry">
    <header>
      <h2>dispatch · admission registry</h2>
      <div class="flags">
        <span class="pill" class:on={d.enabled}>{d.enabled ? 'enabled' : 'disabled'}</span>
        <span class="pill" class:on={d.enforcing}>{d.enforcing ? 'enforcing' : 'observe'}</span>
      </div>
    </header>

    <div class="metrics">
      <div class="metric"><span class="k">demand</span><span class="v">{d.demand}</span></div>
      <div class="metric"><span class="k">reaps</span><span class="v">{d.reaps_total}</span></div>
      <div class="metric"><span class="k">downgrades</span><span class="v">{d.class_downgrades}</span></div>
      <div class="metric"><span class="k">uncharged</span><span class="v">{d.uncharged_calls}</span></div>
      <div class="metric"><span class="k">ops</span><span class="v">{d.ops_total}</span></div>
      <div class="metric"><span class="k">max op</span><span class="v">{fmtMs(d.max_op_ms)}</span></div>
    </div>

    <div class="stamps">
      <span class="stamp"><span class="k">guard</span> {fmtStamp(d.last_guard_at)}</span>
      <span class="stamp"><span class="k">digest</span> {fmtStamp(d.last_digest_at)}</span>
      <span class="stamp"><span class="k">overview</span> {fmtStamp(d.last_overview_at)}</span>
    </div>

    <h3>embed tokens · 24h</h3>
    {#if d.embed_tokens.length === 0}
      <p class="state">no embed calls in the window.</p>
    {:else}
      <Table label="embed token rollup" valign="baseline">
        {#snippet head()}
          <tr><th>target</th><th class="num">prompt tokens</th></tr>
        {/snippet}
        {#each d.embed_tokens as et (et.target)}
          <tr>
            <td class="mono">{et.target}</td>
            <td class="num">{fmtTokens(et.prompt_tokens)}</td>
          </tr>
        {/each}
      </Table>
    {/if}

    <h3>targets</h3>
    {#if d.targets.length === 0}
      <p class="state">no dispatch targets.</p>
    {:else}
      <Table label="dispatch targets" valign="baseline">
        {#snippet head()}
          <tr>
            <th>origin</th><th class="num">slots</th><th class="num">held/inflight</th>
            <th class="num">int wait (wq · p95 · max)</th>
            <th class="num">bg wait (wq · p95 · max)</th>
            <th class="num">preempt/aged</th>
          </tr>
        {/snippet}
        {#each d.targets as t (t.origin)}
          <tr>
            <td class="mono">{t.origin}{#if t.preempt_background}<span class="tag">preempt-bg</span>{/if}</td>
            <td class="num">{t.slots}</td>
            <td class="num dim">{t.held}/{t.inflight}</td>
            <td class="num">{t.interactive.waiting} · {fmtMs(t.interactive.p95_wait_ms)} · {fmtMs(t.interactive.max_wait_ms)}</td>
            <td class="num">{t.background.waiting} · {fmtMs(t.background.p95_wait_ms)} · {fmtMs(t.background.max_wait_ms)}</td>
            <td class="num dim">{t.preempt.preempts_total}/{t.preempt.aged_preempts_total}</td>
          </tr>
        {/each}
      </Table>
    {/if}
  </section>
{:else if dispatchTenant}
  {@const dt = dispatchTenant}
  <section class="card" aria-label="dispatch occupancy">
    <header>
      <h2>dispatch · occupancy</h2>
    </header>
    {#if dt.targets.length === 0}
      <p class="state">no visible targets.</p>
    {:else}
      <Table label="dispatch occupancy" valign="baseline">
        {#snippet head()}
          <tr>
            <th>target</th><th>state</th><th>load</th>
            <th class="num">your queue</th><th class="num">running</th><th class="num">oldest wait</th>
          </tr>
        {/snippet}
        {#each dt.targets as t (t.origin)}
          <tr>
            <td class="mono">{t.origin}</td>
            <td><span class="dot {t.busy ? 'busy' : 'idle'}"></span>{t.busy ? 'busy' : 'idle'}</td>
            <td><span class="depth {depthClass(t.depth)}">{depthLabel(t.depth)}</span></td>
            <td class="num">{t.own_waiting}</td>
            <td class="num dim">{t.own_inflight}</td>
            <td class="num dim">{fmtMs(t.own_oldest_wait_ms)}</td>
          </tr>
        {/each}
      </Table>
    {/if}
  </section>
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
  h3 {
    margin: 0;
    padding: var(--space-2) var(--space-3) 0;
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-faint);
  }
  .flags {
    margin-left: auto;
    display: flex;
    gap: var(--space-2);
  }
  .pill {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    padding: var(--space-1) var(--space-2);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    background: var(--surface-2);
    color: var(--text-dim);
  }
  .pill.on {
    background: var(--accent-dim);
    color: var(--accent);
    border-color: var(--accent);
  }
  .metrics {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(7rem, 1fr));
    gap: var(--space-2);
    padding: var(--space-3);
  }
  .metric {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .metric .k,
  .stamp .k {
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
  }
  .metric .v {
    font-family: var(--font-mono);
    font-size: var(--fs-md);
    font-variant-numeric: tabular-nums;
  }
  .stamps {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-3);
    padding: 0 var(--space-3) var(--space-2);
  }
  .stamp {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    color: var(--text-dim);
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
  }
  .state {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
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
  .tag {
    margin-left: var(--space-2);
    font-size: var(--fs-xs);
    color: var(--accent);
    text-transform: uppercase;
  }
  .dot {
    display: inline-block;
    width: 0.55rem;
    height: 0.55rem;
    border-radius: 50%;
    margin-right: var(--space-1);
    vertical-align: middle;
  }
  .dot.busy {
    background: var(--warn);
  }
  .dot.idle {
    background: var(--ok);
  }
  .depth {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
  }
  .depth.idle {
    color: var(--text-dim);
  }
  .depth.low {
    color: var(--ok);
  }
  .depth.high {
    color: var(--warn);
  }
  .depth.unknown {
    color: var(--text-faint);
  }
</style>
