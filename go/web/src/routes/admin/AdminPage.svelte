<script lang="ts">
  // Server-admin area landing (design 04 §1/§3, Wave A2) — read-only tenant
  // register. Page-self-gate (K8): the real authorization is server-side
  // (RequireAdmin → 403), the client gate only decides banner-vs-request so a
  // non-admin key sees the 403 banner instead of a doomed call. While the boot
  // restore probe is in flight the tier is 'loading' (capabilities loading-floor,
  // R6) — we hold a neutral state rather than flash the 403 banner. This wave is
  // strictly read-only: tenant create/edit/suspend/delete is A3, the
  // /admin/tenants/:id detail + quota is A4, grants/corpus are later tabs.
  import { session } from '../../lib/auth.svelte'
  import type { TenantStatus } from '../../lib/api/types'
  import { TenantsModel } from './tenants.svelte'
  import ScopeMap from './ScopeMap.svelte'
  import CorpusMaintenance from './CorpusMaintenance.svelte'

  const model = new TenantsModel()

  // Reactive load that survives the boot restore race: fires once the session
  // resolves to server-admin (reading session.tier + model.status as $state so
  // the effect actually re-runs, Svelte-5). A non-admin never triggers the
  // request — they get the banner. Idle-guard prevents a reload loop.
  $effect(() => {
    if (session.tier === 'server-admin' && model.status === 'idle') {
      void model.load()
    }
  })

  /** Status → token color (active healthy, suspended warned, offboarding danger). */
  function statusColor(s: TenantStatus): string {
    if (s === 'active') return 'var(--ok)'
    if (s === 'suspended') return 'var(--warn)'
    return 'var(--danger)' // offboarding
  }

  /** ISO timestamp → date portion; degrades to the raw string if unparseable. */
  function fmtDate(iso: string): string {
    const t = Date.parse(iso)
    return Number.isNaN(t) ? iso : new Date(t).toISOString().slice(0, 10)
  }
</script>

<section class="area">
  <header>
    <h1>Admin</h1>
    <p class="sub">tenant register and cross-tenant administration — server-admin only</p>
  </header>

  {#if session.tier === 'loading'}
    <p class="state" aria-busy="true">restoring session…</p>
  {:else if session.caps.tier !== 'server-admin'}
    <p class="banner" role="alert">
      server-admin only — the tenant register is gated server-side (the server answers 403). Sign in with a
      server-admin key to manage tenants.
    </p>
  {:else if model.status === 'loading' || model.status === 'idle'}
    <p class="state" aria-busy="true">loading tenants…</p>
  {:else if model.status === 'error'}
    <div class="error" role="alert">
      <p>{model.loadError?.message}</p>
      {#if model.loadError?.requestId}<p class="request-id">request {model.loadError.requestId}</p>{/if}
      <button type="button" onclick={() => void model.reload()}>Retry</button>
    </div>
  {:else}
    <section class="card" aria-label="tenant register">
      <header class="card-head">
        <h2>tenants</h2>
        <span class="count">{model.tenants.length}</span>
      </header>

      {#if model.tenants.length === 0}
        <p class="empty">no tenants — the register is empty</p>
      {:else}
        {#if model.tenants.length === 1}
          <p class="empty" role="status">
            only the default tenant exists — no additional tenants have been created yet
          </p>
        {/if}
        <div class="scroll">
          <table>
            <thead>
              <tr>
                <th>slug</th>
                <th>display name</th>
                <th>status</th>
                <th>created</th>
              </tr>
            </thead>
            <tbody>
              {#each model.tenants as t (t.id)}
                <tr>
                  <td class="slug">{t.slug}</td>
                  <td class="name">{t.display_name || '—'}</td>
                  <td>
                    <span class="badge">
                      <span class="dot" style="background:{statusColor(t.status)}"></span>{t.status}
                    </span>
                  </td>
                  <td class="created">{fmtDate(t.created_at)}</td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/if}
    </section>

    <ScopeMap />

    <CorpusMaintenance />
  {/if}
</section>

<style>
  .area {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  header {
    border-bottom: 1px solid var(--border);
    padding-bottom: var(--space-2);
  }
  h1 {
    margin: 0;
    font-size: 1.35rem;
    font-weight: 600;
    letter-spacing: 0.01em;
  }
  .sub {
    margin: var(--space-1) 0 0;
    color: var(--text-dim);
    font-size: 0.85rem;
  }
  .banner {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--warn);
    border-radius: var(--radius);
    color: var(--warn);
    font-size: 0.875rem;
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
  .empty {
    margin: 0;
    padding: var(--space-3);
    color: var(--text-faint);
    font-size: 0.85rem;
  }
  .scroll {
    overflow-x: auto;
  }
  table {
    width: 100%;
    border-collapse: collapse;
    font-size: 0.82rem;
  }
  th {
    text-align: left;
    padding: var(--space-1) var(--space-3);
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    font-weight: 500;
    border-bottom: 1px solid var(--border);
    white-space: nowrap;
  }
  td {
    padding: var(--space-1) var(--space-3);
    border-bottom: 1px solid var(--surface-2);
    vertical-align: top;
  }
  .slug {
    font-family: var(--font-mono);
    color: var(--text);
  }
  .name {
    color: var(--text-dim);
  }
  .created {
    font-family: var(--font-mono);
    color: var(--text-dim);
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }
  .badge {
    display: inline-flex;
    align-items: center;
    font-family: var(--font-mono);
    font-size: var(--label-size);
    text-transform: uppercase;
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
    color: var(--text-dim);
    white-space: nowrap;
  }
  .dot {
    display: inline-block;
    width: 0.55rem;
    height: 0.55rem;
    border-radius: 50%;
    margin-right: var(--space-1);
    vertical-align: middle;
  }
</style>
