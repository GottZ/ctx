<script lang="ts">
  // Scope-landscape panel (design 04 §3 A0, Wave A0-FE) — read-only. One row per
  // scope across the WHOLE store: block count, key count, owning tenant. Counts
  // only (no block content ever leaves the server, store/scope_overview.go), so a
  // server-admin sees the cross-tenant landscape without a content leak. This is
  // the source the QuotaForm's tenant→scope mapping and the tenant-delete
  // blast-radius count later read from; here it is purely the landscape view.
  //
  // Mounted inside AdminPage's server-admin branch, so the tier is already
  // server-admin when this renders — the idle-guard $effect still fires the load
  // exactly once (status='idle' → load), surviving the boot-restore race the same
  // way AdminPage's tenant load does (reading session.caps.tier + model.status as
  // $state so the effect re-runs, Svelte-5).
  import { session } from '../../lib/auth.svelte'
  import { ScopesModel } from './scopes.svelte'

  const model = new ScopesModel()

  $effect(() => {
    if (session.caps.tier === 'server-admin' && model.status === 'idle') {
      void model.load()
    }
  })
</script>

<section class="card" aria-label="scope map">
  <header class="card-head">
    <h2>scope map</h2>
    <span class="count">{model.status === 'ready' ? model.scopes.length : '·'}</span>
  </header>

  <p class="note">
    Every scope across the store with its block and key counts and owning tenant. Counts only — no block
    content. A scope with no tenant (a system scope or an unassigned scope) shows as unmapped.
  </p>

  {#if model.status === 'loading' || model.status === 'idle'}
    <p class="state" aria-busy="true">loading scopes…</p>
  {:else if model.status === 'error'}
    <div class="error" role="alert">
      <p>{model.loadError?.message}</p>
      {#if model.loadError?.requestId}<p class="request-id">request {model.loadError.requestId}</p>{/if}
      <button type="button" onclick={() => void model.reload()}>Retry</button>
    </div>
  {:else if model.scopes.length === 0}
    <p class="empty" role="status">no scopes — the store is empty</p>
  {:else}
    <div class="scroll">
      <table>
        <thead>
          <tr>
            <th>scope</th>
            <th class="num">blocks</th>
            <th class="num">keys</th>
            <th>tenant</th>
          </tr>
        </thead>
        <tbody>
          {#each model.scopes as s (s.scope)}
            <tr>
              <td class="scope">{s.scope}</td>
              <td class="num">{s.block_count}</td>
              <td class="num">{s.key_count}</td>
              <td>
                {#if s.tenant_id}
                  <span class="tenant">{s.tenant_id}</span>
                {:else}
                  <span class="unmapped" title="system or unassigned scope — no owning tenant">unmapped</span>
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
    color: var(--text-dim);
  }
  .note {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    color: var(--text-dim);
    font-size: var(--fs-sm);
    border-bottom: 1px solid var(--border);
  }
  .state {
    margin: 0;
    padding: var(--space-3);
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
  }
  .error {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    align-items: flex-start;
    border: 1px solid var(--danger);
    border-radius: var(--radius);
    background: var(--danger-dim);
    margin: var(--space-2) var(--space-3);
    padding: var(--space-2) var(--space-3);
    font-size: var(--fs-sm);
  }
  .error p {
    margin: 0;
    color: var(--danger);
  }
  .request-id {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    color: var(--text-dim) !important;
  }
  .empty {
    margin: 0;
    padding: var(--space-3);
    color: var(--text-faint);
    font-size: var(--fs-sm);
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
    vertical-align: top;
  }
  .num {
    text-align: right;
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }
  td.num {
    font-family: var(--font-mono);
    color: var(--text-dim);
  }
  .scope {
    font-family: var(--font-mono);
    color: var(--text);
  }
  .tenant {
    font-family: var(--font-mono);
    color: var(--text-dim);
  }
  .unmapped {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    text-transform: uppercase;
    letter-spacing: var(--label-tracking);
    color: var(--text-faint);
  }
</style>
