<script lang="ts">
  // FE-10 / FE-SS2 — structural tenant usage (design 05-frontend-a3-selfservice §4
  // SS2). Reads the ScopesSelfModel's usage (tenant-usage-get): "scopes N/M" and
  // "keys X/Y", where a null cap renders "unlimited". This is the STRUCTURE axis
  // (scope/key COUNTS against the per-tenant caps), distinct from QuotaCard's
  // per-scope cost/call SPEND. Read-only summary — the server caps hard (429); the
  // key-create CTA disable (atKeyLimit) lives on TenantPage, which owns the button.
  //
  // Consumes the same single ScopesSelfModel instance that drives ScopeSelfCard, so
  // there is exactly one load (scope-list + tenant-usage-get) per /tenant visit.
  import type { ScopesSelfModel } from './scopes.svelte'

  let { model }: { model: ScopesSelfModel } = $props()

  // null cap = unlimited for that dimension (design §1; TenantUsageView.max_* null).
  function fmtCap(n: number | null): string {
    return n === null ? 'unlimited' : String(n)
  }
</script>

<section class="card" aria-label="tenant usage">
  <header class="card-head">
    <h2>usage</h2>
    <span class="count">structural</span>
  </header>
  <div class="card-body">
    {#if model.status === 'loading' || model.status === 'idle'}
      <p class="state" aria-busy="true">loading usage…</p>
    {:else if model.usage}
      <dl class="usage">
        <div class="row">
          <dt>scopes</dt>
          <dd aria-label="scopes usage">{model.usage.scope_count} / {fmtCap(model.usage.max_scopes)}</dd>
        </div>
        <div class="row">
          <dt>keys</dt>
          <dd aria-label="keys usage">{model.usage.key_count} / {fmtCap(model.usage.max_keys)}</dd>
        </div>
      </dl>
    {:else}
      <p class="state">usage unavailable</p>
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
    font-size: var(--fs-md);
    font-weight: var(--fw-semibold);
  }
  .count {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-dim);
  }
  .card-body {
    padding: var(--space-3);
  }
  .state {
    margin: 0;
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
  }
  .usage {
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
    font-size: var(--fs-sm);
    color: var(--text);
    font-variant-numeric: tabular-nums;
  }
</style>
