<script lang="ts">
  // Read-only tenant quota transparency (design 05-tenant-admin §3, Wave TK6).
  // Reads ONE scope's quota via tenant-quota-get (server-pins a tenant-admin to
  // its own scope; getTenantQuota is the A1 binding, re-exported by keys.ts).
  // Renders BOTH response shapes: a nil policy → {enabled:false, unlimited:true}
  // → the "unlimited" state; a real policy → the budget fields, where a null
  // cost/calls field is "unlimited for that dimension" (server stored a nil
  // pointer). No write — quota-set is server-admin only (design §1).
  import { onMount } from 'svelte'
  import { toApiError, type ApiError } from '../../lib/api'
  import { getTenantQuota } from '../../lib/api/keys'
  import { session } from '../../lib/auth.svelte'
  import type { TenantQuotaView } from '../../lib/api/types'

  let status = $state<'loading' | 'ready' | 'error'>('loading')
  let quota = $state<TenantQuotaView | null>(null)
  let error = $state<ApiError | null>(null)

  async function load(): Promise<void> {
    status = 'loading'
    error = null
    try {
      // A tenant-admin is server-pinned to its own scope (payload ignored); a
      // server-admin targets its own home scope here.
      const res = await getTenantQuota(session.homeScope ?? undefined)
      quota = res.quota
      status = 'ready'
    } catch (err) {
      error = toApiError(err)
      status = 'error'
    }
  }
  onMount(() => void load())

  // null/undefined budget field = unlimited for that dimension (design §3).
  function fmtUsd(v: number | null | undefined): string {
    return v === null || v === undefined ? 'unlimited' : `$${v.toFixed(2)}`
  }
  function fmtCalls(v: number | null | undefined): string {
    return v === null || v === undefined ? 'unlimited' : v.toLocaleString()
  }
  function fmtExceed(v: string | undefined): string {
    if (v === 'block') return 'block (hard error)'
    if (v === 'external_off') return 'external backends off (degrade to local)'
    return v ?? '—'
  }
</script>

<section class="card" aria-label="tenant quota">
  <header class="card-head">
    <h2>quota</h2>
    {#if quota}<span class="count">{quota.scope}</span>{/if}
  </header>

  <div class="card-body">
    {#if status === 'loading'}
      <p class="state" aria-busy="true">loading quota…</p>
    {:else if status === 'error'}
      <div class="error" role="alert">
        <p>{error?.message ?? 'failed to load quota'}</p>
        {#if error?.requestId}<p class="request-id">request {error.requestId}</p>{/if}
        <button type="button" onclick={() => void load()}>Retry</button>
      </div>
    {:else if quota}
      {#if quota.unlimited === true || quota.enabled === false}
        <p class="unlimited" role="status">
          <span class="badge">unlimited</span>
          No quota policy is set for this scope — usage is not budget-capped.
        </p>
      {:else}
        <dl class="limits">
          <div class="row">
            <dt>daily cost</dt>
            <dd>{fmtUsd(quota.daily_cost_usd)}</dd>
          </div>
          <div class="row">
            <dt>monthly cost</dt>
            <dd>{fmtUsd(quota.monthly_cost_usd)}</dd>
          </div>
          <div class="row">
            <dt>daily calls</dt>
            <dd>{fmtCalls(quota.daily_calls)}</dd>
          </div>
          <div class="row">
            <dt>on exceed</dt>
            <dd>{fmtExceed(quota.on_exceed)}</dd>
          </div>
        </dl>
      {/if}
    {/if}
  </div>
</section>

<style>
  .card {
    background: var(--surface-1);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
  .card-head {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border-bottom: 1px solid var(--border);
  }
  h2 {
    margin: 0;
    font-family: var(--font-mono);
    font-size: 0.95rem;
    font-weight: 600;
  }
  .count {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    color: var(--text-dim);
  }
  .card-body {
    padding: var(--space-3);
  }
  .state {
    margin: 0;
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: 0.85rem;
  }
  .error {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    align-items: flex-start;
    border: 1px solid var(--danger);
    border-radius: var(--radius);
    background: var(--danger-dim);
    padding: var(--space-2) var(--space-3);
    font-size: 0.85rem;
  }
  .error p {
    margin: 0;
    color: var(--danger);
  }
  .request-id {
    font-family: var(--font-mono);
    font-size: 0.75rem;
    color: var(--text-dim) !important;
  }
  .error button {
    font-family: var(--font-ui);
    font-size: 0.8rem;
    padding: var(--space-1) var(--space-3);
    background: var(--surface-2);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
    cursor: pointer;
  }
  .unlimited {
    margin: 0;
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-2);
    font-size: 0.85rem;
    color: var(--text-dim);
  }
  .badge {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    text-transform: uppercase;
    letter-spacing: var(--label-tracking);
    border: 1px solid var(--ok);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
    color: var(--ok);
  }
  .limits {
    margin: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .row {
    display: flex;
    justify-content: space-between;
    gap: var(--space-3);
    padding: var(--space-1) 0;
    border-bottom: 1px solid var(--surface-2);
  }
  dt {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-faint);
  }
  dd {
    margin: 0;
    font-family: var(--font-mono);
    font-size: 0.85rem;
    color: var(--text);
    font-variant-numeric: tabular-nums;
  }
</style>
