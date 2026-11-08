<script lang="ts">
  // "Model API" status tile (RC-D C6, wave W05-17; design/05 §4.6, placed
  // after DispatchTile). Rides the status answer — no request of its own
  // ("Hub-Last ≤ Rollup": the counts come from C2's 30-s generation).
  // server-admin: counters, 5-min refusals, instance envelope, the
  // internal/external partition per target (dispatch.targets, A03-W3), top
  // consumers, expiring keys. tenant-admin: its own counters (the same
  // `gateway` field, own consumers only) + the coarse depth class per target
  // from dispatch_tenant — never a number (V-03-b). No `gateway` section ⇒
  // the tile is not drawn (status/gateway.ts gatewayTileMode).
  import type { StatusResponse } from '../../lib/api/types'
  import { session } from '../../lib/auth.svelte'
  import Table from '../../lib/ui/Table.svelte'
  import { fmtMs } from './dispatch'
  import { countText, expiringText, gatewayTileMode, partitionRows, rejectionsText, tenantTargetCells } from './gateway'

  let { status }: { status: StatusResponse } = $props()

  const mode = $derived(gatewayTileMode(status, session.caps.tier))
  const g = $derived(status.gateway ?? null)
  const partition = $derived(mode === 'admin' ? partitionRows(status.dispatch) : [])
  const tenantCells = $derived(mode === 'tenant' ? tenantTargetCells(status.dispatch_tenant) : [])
  const envelope = $derived(Object.entries(g?.instance_envelope ?? {}).sort((a, b) => a[0].localeCompare(b[0])))
</script>

{#if mode !== 'off' && g !== null}
  <section class="card" aria-label="model api">
    <header>
      <h2>model api</h2>
      <span class="pill" class:on={g.enabled}>{g.enabled ? 'enabled' : 'disabled'}</span>
    </header>
    <div class="metrics">
      <div class="metric"><span class="k">consumers</span><span class="v">{g.consumers}</span></div>
      <div class="metric"><span class="k">used 1h</span><span class="v">{g.consumers_used_1h}</span></div>
      <div class="metric">
        <span class="k">open reservations</span><span class="v" data-testid="gateway-reservations">{countText(g.reservations_open)}</span>
      </div>
    </div>
    <p class="line" data-testid="gateway-rejections">refusals 5 min: {rejectionsText(g.rejections_5m)}</p>
    {#if expiringText(g.consumers_expiring) !== ''}
      <p class="line warn" data-testid="gateway-expiring">{expiringText(g.consumers_expiring)}</p>
    {/if}

    {#if mode === 'admin'}
      {#if envelope.length > 0}
        <p class="line">
          instance envelope today:
          {#each envelope as [op, u] (op)}<span class="env">{op} {u.used.toLocaleString('en-US')} / {u.limit.toLocaleString('en-US')}</span>{/each}
        </p>
      {/if}
      {#if partition.length > 0}
        <Table label="model api partition" focusableScroll>
          {#snippet head()}
            <tr>
              <th>target</th>
              <th>internal held / waiting</th>
              <th>reserved</th>
              <th>external held / XU / XB waiting</th>
              <th>external capacity</th>
              <th>p95 wait internal / external</th>
            </tr>
          {/snippet}
          {#each partition as r (r.origin)}
            <tr>
              <td><code>{r.origin}</code></td>
              <td>{r.internalHeld} / {r.internalWaiting}</td>
              <td>{r.internalReserved}</td>
              <td>{r.externalHeld} / {r.xuWaiting} / {r.xbWaiting}</td>
              <td>{r.externalCapacity}</td>
              <td>{fmtMs(r.p95InternalMs)} / {fmtMs(r.p95ExternalMs)}</td>
            </tr>
          {/each}
        </Table>
      {/if}
      {#if (g.top_consumers_1h ?? []).length > 0}
        <ol class="top" aria-label="top consumers last hour">
          {#each g.top_consumers_1h ?? [] as t (t.consumer_id)}
            <li>
              <a href="/gateway/consumers/{t.consumer_id}"><code>{t.consumer_id.slice(-8)}</code></a>
              <span>{t.requests} requests · {t.tokens.toLocaleString('en-US')} tokens</span>
            </li>
          {/each}
        </ol>
      {/if}
    {:else}
      <ul class="depths" aria-label="queue depth per target">
        {#each tenantCells as cell (cell.origin)}
          <li>
            <code>{cell.origin}</code>
            <!-- depthLabel's closed set (idle/low/high, '—' for anything else) doubles as the class -->
            <span class="depth {cell.depth === '—' ? 'unknown' : cell.depth}" data-tenant-depth>{cell.depth}</span>
          </li>
        {/each}
      </ul>
    {/if}
  </section>
{/if}

<style>
  .card {
    background: var(--surface-1);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    padding: var(--space-2) var(--space-3);
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  header {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  h2 {
    margin: 0;
    font-family: var(--font-mono);
    font-size: var(--fs-md);
    font-weight: var(--fw-semibold);
  }
  .pill {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
    color: var(--text-dim);
  }
  .pill.on {
    color: var(--ok);
  }
  .metrics {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-3);
  }
  .metric {
    display: flex;
    flex-direction: column;
  }
  .k {
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    color: var(--text-dim);
  }
  .v {
    font-family: var(--font-mono);
    font-size: var(--fs-md);
  }
  .line {
    margin: 0;
    font-size: var(--fs-sm);
    color: var(--text-dim);
  }
  .line.warn {
    color: var(--warn);
  }
  .env {
    margin-left: var(--space-2);
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
  }
  .top,
  .depths {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    font-size: var(--fs-sm);
  }
  .top li,
  .depths li {
    display: flex;
    gap: var(--space-2);
    align-items: baseline;
  }
  code {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
  }
  .depth {
    font-family: var(--font-mono);
    font-size: var(--label-size);
  }
  .depth.low {
    color: var(--warn);
  }
  .depth.high {
    color: var(--danger);
  }
  .depth.idle,
  .depth.unknown {
    color: var(--text-dim);
  }
</style>
