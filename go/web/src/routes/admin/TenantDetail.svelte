<script lang="ts">
  // Tenant-detail page (design 04 §3/§7.1, Wave A4) at /admin/tenants/:id —
  // server-admin only. Shows the tenant's register row (tenant-get: slug /
  // display_name / status / created — it carries NO scopes and NO counts,
  // store/tenant.go:104) plus one QuotaForm per scope the tenant owns. The scope
  // list is NOT in tenant-get (a tenant owns 0..N scopes); it comes from the A0
  // scope-overview filtered to this tenant_id — the only tenant→scope source.
  // This page mounts NO BlockGrantPanel: block-grants live in the Grants tab/A6
  // (design §7.1), and the block-grant list is owner-side anyway (§6.2).
  import { route, navigate } from '../../router'
  import { session } from '../../lib/auth.svelte'
  import { getTenant, scopeOverview, DEFAULT_TENANT_ID } from '../../lib/api/tenants'
  import { toApiError, type ApiError } from '../../lib/api'
  import type { ResourceStatus } from '../../lib/resource.svelte'
  import type { ScopeOverview, Tenant, TenantStatus } from '../../lib/api/types'
  import QuotaForm from './QuotaForm.svelte'
  import TenantProvision from './TenantProvision.svelte'
  import ConfirmDialog from '../../lib/components/ConfirmDialog.svelte'
  import { TenantsModel } from './tenants.svelte'

  // sv-router AllParams<T> is Partial<Record<param,string>> → id is string|undefined.
  // `route.params` is reactive; read it through a $derived so the load effect
  // re-runs on a param change (deep-linking to a different tenant).
  const tenantId = $derived(route.params.id ?? '')

  let status = $state<ResourceStatus>('idle')
  let loadError = $state<ApiError | null>(null)
  let tenant = $state<Tenant | null>(null)
  let scopes = $state<ScopeOverview[]>([])
  // The id whose data is loaded — the idle-guard key (load once per id, re-fire on
  // a real id change), surviving the boot-restore race like the sibling panels.
  let loadedId = $state<string | null>(null)

  $effect(() => {
    if (
      session.tier === 'server-admin' &&
      tenantId &&
      tenantId !== loadedId &&
      status !== 'loading'
    ) {
      void load(tenantId)
    }
  })

  async function load(id: string): Promise<void> {
    status = 'loading'
    loadError = null
    try {
      // tenant-get for the register row; scope-overview for this tenant's scopes
      // (the QuotaForm is scope-keyed and tenant-get carries no scopes).
      const [tRes, ovRes] = await Promise.all([getTenant(id), scopeOverview()])
      tenant = tRes.tenant
      scopes = ovRes.scopes.filter((s) => s.tenant_id === id)
      loadedId = id
      status = 'ready'
    } catch (err) {
      loadError = toApiError(err)
      loadedId = id // pin so the guard does not re-fire the failing load
      status = 'error'
    }
  }

  // Re-load ONLY the scope list after a provision write (A3c), without flipping
  // `status` to 'loading' — that would unmount the provision card mid-flow and
  // blow away its just-revealed scope/key. The store-wide scope-overview is the
  // same tenant→scope source `load` uses; on failure we keep the current list
  // (the provision card surfaces its own create error).
  async function refreshScopes(): Promise<void> {
    try {
      const ovRes = await scopeOverview()
      scopes = ovRes.scopes.filter((s) => s.tenant_id === tenantId)
    } catch {
      // keep the existing list
    }
  }

  function statusColor(s: TenantStatus): string {
    if (s === 'active') return 'var(--ok)'
    if (s === 'suspended') return 'var(--warn)'
    return 'var(--danger)' // offboarding
  }

  function fmtDate(iso: string): string {
    const t = Date.parse(iso)
    return Number.isNaN(t) ? iso : new Date(t).toISOString().slice(0, 10)
  }

  // A not-found tenant-get surfaces as an ApiError; show it 404-style (404 or a
  // 200 {success:false} "tenant not found" both land here).
  const notFound = $derived(loadError?.status === 404 || /not found/i.test(loadError?.message ?? ''))

  // --- A3b lifecycle: suspend / activate / delete (design 05 §4 A3b) ---
  // The brief drives the mutations through TenantsModel: busyId guards the
  // in-flight control, actionError surfaces a failure. The model reloads the
  // tenant LIST internally; this page renders ONE tenant, so after a
  // suspend/activate we lift the fresh row off the model's reloaded list (no
  // extra tenant-get), falling back to a local re-load if the list was truncated
  // at scale. Delete navigates back to /admin. Authorization is server-side
  // (tenant-update/-delete are tierServerAdmin) — these controls are UX only.
  const lifecycle = new TenantsModel()
  let showDelete = $state(false)

  // The default tenant is server-guarded (tenant-delete → 400, tenant_manage.go
  // :159-161); disable the control on the id we would actually delete — the route
  // param IS the delete target, so it is the authoritative guard key.
  const isDefault = $derived(tenantId === DEFAULT_TENANT_ID)

  // Blast-radius for the delete confirm. The counts come from the server-wide
  // scope-overview filtered to this tenant; at 1M+ scale that list can be capped,
  // so the copy reads "at least" (W17 — never understate the blast radius).
  const blastBlocks = $derived(scopes.reduce((n, s) => n + (s.block_count ?? 0), 0))
  const blastKeys = $derived(scopes.reduce((n, s) => n + (s.key_count ?? 0), 0))

  async function setStatus(next: 'active' | 'suspended'): Promise<void> {
    try {
      if (next === 'suspended') await lifecycle.suspend(tenantId)
      else await lifecycle.activate(tenantId)
      const updated = lifecycle.tenants.find((t) => t.id === tenantId)
      if (updated) tenant = updated
      else await load(tenantId) // list truncated → authoritative single re-load
    } catch {
      // actionError is surfaced from the model; keep the page in place.
    }
  }

  // ConfirmDialog awaits this — a throw keeps the dialog open and shows the error;
  // success navigates back to the (now shorter) register.
  async function confirmDelete(): Promise<void> {
    await lifecycle.remove(tenantId)
    navigate('/admin', { replace: true })
  }
</script>

<section class="area">
  <header>
    <a class="back" href="/admin">← back to admin</a>
    <h1>tenant detail</h1>
    <p class="sub">register row and per-scope quota — server-admin only</p>
  </header>

  {#if session.tier === 'loading'}
    <p class="state" aria-busy="true">restoring session…</p>
  {:else if session.caps.tier !== 'server-admin'}
    <p class="banner" role="alert">
      server-admin only — tenant administration is gated server-side (the server answers 403). Sign in with a
      server-admin key.
    </p>
  {:else if status === 'loading' || status === 'idle'}
    <p class="state" aria-busy="true">loading tenant…</p>
  {:else if status === 'error'}
    <div class="error" role="alert">
      {#if notFound}
        <p>tenant not found — no register row for id <code>{tenantId}</code>.</p>
      {:else}
        <p>{loadError?.message}</p>
      {/if}
      {#if loadError?.requestId}<p class="request-id">request {loadError.requestId}</p>{/if}
      <button type="button" onclick={() => void load(tenantId)}>Retry</button>
    </div>
  {:else if tenant}
    <section class="card" aria-label="tenant register row">
      <header class="card-head">
        <h2>{tenant.slug}</h2>
        <span class="badge">
          <span class="dot" style="background:{statusColor(tenant.status)}"></span>{tenant.status}
        </span>
      </header>
      <dl class="meta">
        <dt>display name</dt>
        <dd>{tenant.display_name || '—'}</dd>
        <dt>id</dt>
        <dd class="mono">{tenant.id}</dd>
        <dt>created</dt>
        <dd class="mono">{fmtDate(tenant.created_at)}</dd>
        <dt>updated</dt>
        <dd class="mono">{fmtDate(tenant.updated_at)}</dd>
      </dl>

      {#if lifecycle.actionError}
        <p class="problem" role="alert">{lifecycle.actionError}</p>
      {/if}
      <div class="actions">
        {#if tenant.status === 'active'}
          <button
            type="button"
            disabled={lifecycle.busyId === tenantId}
            onclick={() => void setStatus('suspended')}
          >
            Suspend
          </button>
        {:else}
          <button
            type="button"
            disabled={lifecycle.busyId === tenantId}
            onclick={() => void setStatus('active')}
          >
            Activate
          </button>
        {/if}
        <button
          type="button"
          class="danger"
          disabled={isDefault || lifecycle.busyId === tenantId}
          title={isDefault ? 'the default tenant cannot be deleted' : undefined}
          onclick={() => (showDelete = true)}
        >
          Delete tenant
        </button>
      </div>
    </section>

    <TenantProvision {tenantId} {scopes} onprovisioned={refreshScopes} />

    <section class="card" aria-label="per-scope quota">
      <header class="card-head">
        <h2>quota per scope</h2>
        <span class="count">{scopes.length}</span>
      </header>
      <p class="note">
        Each scope this tenant owns has its own quota row. A scope with no quota row is unlimited until you set
        one. The scope list comes from the store-wide scope overview filtered to this tenant.
      </p>
      {#if scopes.length === 0}
        <p class="empty" role="status">
          this tenant owns no scopes yet — quota is scope-keyed, so there is nothing to limit. Provision a scope
          in the section above to give it something to limit.
        </p>
      {:else}
        <div class="forms">
          {#each scopes as s (s.scope)}
            <QuotaForm scope={s.scope} />
          {/each}
        </div>
      {/if}
    </section>
  {/if}
</section>

{#if showDelete && tenant}
  <ConfirmDialog
    title="Delete tenant"
    danger
    requireTyped={tenant.slug}
    confirmLabel="Delete"
    onconfirm={confirmDelete}
    oncancel={() => (showDelete = false)}
  >
    <p class="warn-copy">
      Permanently prunes tenant <code>{tenant.slug}</code> — at least {blastBlocks} block{blastBlocks === 1
        ? ''
        : 's'} across {scopes.length} scope{scopes.length === 1 ? '' : 's'} and {blastKeys} key{blastKeys === 1
        ? ''
        : 's'}, plus the register row. This cannot be undone.
    </p>
  </ConfirmDialog>
{/if}

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
  .back {
    display: inline-block;
    margin-bottom: var(--space-1);
    color: var(--text-dim);
    font-size: 0.82rem;
    text-decoration: none;
  }
  .back:hover {
    color: var(--accent);
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
  .error code {
    font-family: var(--font-mono);
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
  .meta {
    display: grid;
    grid-template-columns: max-content 1fr;
    gap: var(--space-1) var(--space-3);
    margin: 0;
    padding: var(--space-3);
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
    color: var(--text);
    font-size: 0.85rem;
  }
  .mono {
    font-family: var(--font-mono);
    color: var(--text-dim);
  }
  .note {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    color: var(--text-dim);
    font-size: 0.8rem;
    border-bottom: 1px solid var(--border);
  }
  .empty {
    margin: 0;
    padding: var(--space-3);
    color: var(--text-faint);
    font-size: 0.85rem;
  }
  .forms {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    padding: var(--space-3);
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
  .actions {
    display: flex;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3) var(--space-3);
  }
  .actions button {
    font-family: var(--font-ui);
    font-size: 0.82rem;
    padding: var(--space-1) var(--space-3);
    background: var(--surface-2);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
    cursor: pointer;
  }
  .actions button:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  .actions button.danger {
    border-color: var(--danger);
    color: var(--danger);
  }
  .problem {
    margin: var(--space-2) var(--space-3) 0;
    font-size: 0.78rem;
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius);
    border: 1px solid var(--danger);
    color: var(--danger);
    background: var(--danger-dim);
  }
  .warn-copy {
    margin: 0;
    font-size: 0.85rem;
    line-height: 1.45;
    color: var(--warn);
  }
  .warn-copy code {
    font-family: var(--font-mono);
    color: var(--accent);
  }
</style>
