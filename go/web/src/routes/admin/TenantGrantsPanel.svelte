<script lang="ts">
  // Cross-tenant read-grants panel (design 04 §7.1/§5.5, wave U11) — the Grants
  // surface the TenantDetail page owed since A4 (TenantDetail.svelte:8-9). Lists,
  // creates and revokes the SCOPE-level tenant grants where THIS tenant is the
  // grantee: which foreign scopes it may read (store.ListTenantGrants filters on
  // the grantee, tenant_manage.go:545 → the client narrows by grantee_tenant).
  // All three actions are tierServerAdmin (context_manage.go:164), matching the
  // server-admin-only TenantDetail mount — the panel consumes the pre-built
  // tenants.ts grant client verbatim, it invents NO wire path.
  //
  // Deny-fault contract (§5.5): a server 400/409 (reserved scope, unknown target,
  // grant already exists) surfaces as a visible error and NEVER a silent loss —
  // the create modal stays open with its message, a failed revoke keeps the
  // ConfirmDialog open. Grants carry only scope names + ids (no block content),
  // so the error echo is structurally content-free.
  //
  // NOTE (Grant-API-Ist): the block-level grant client (lib/api/grants.ts) is
  // deliberately NOT mounted here — block-grant-list is OWNER-side (lists the
  // CALLER's own-tenant grants, block_grant_manage.go:124/:29), so on a
  // server-admin session it would show the operator's default-tenant block
  // grants, not THIS tenant's. There is no by-owner-tenant block-grant list, so a
  // coherent per-tenant block-grant view has no wire path today (reported, not
  // faked).
  import { toApiError, type ApiError } from '../../lib/api'
  import { deleteTenantGrant, listTenantGrants } from '../../lib/api/tenants'
  import type { TenantGrant } from '../../lib/api/types'
  import type { ResourceStatus } from '../../lib/resource.svelte'
  import Table from '../../lib/ui/Table.svelte'
  import ConfirmDialog from '../../lib/components/ConfirmDialog.svelte'
  import GrantCreateModal from './GrantCreateModal.svelte'

  let { tenantId, tenantSlug }: { tenantId: string; tenantSlug: string } = $props()

  let status = $state<ResourceStatus>('idle')
  let grants = $state<TenantGrant[]>([])
  let loadError = $state<ApiError | null>(null)
  // Load-once guard keyed by the id whose grants are loaded (re-fire on a real
  // id change, survive the boot-restore race — the sibling-panel pattern).
  let loadedId = $state<string | null>(null)

  // Create modal + its draft/error, and the revoke target for the ConfirmDialog.
  let createOpen = $state(false)
  let revoking = $state<TenantGrant | null>(null)

  $effect(() => {
    if (tenantId && tenantId !== loadedId && status !== 'loading') void load(tenantId)
  })

  async function load(id: string): Promise<void> {
    status = 'loading'
    loadError = null
    try {
      const res = await listTenantGrants(id) // grants WHERE this tenant is grantee
      grants = res.grants
      loadedId = id
      status = 'ready'
    } catch (err) {
      loadError = toApiError(err)
      loadedId = id // pin so the guard does not re-fire the failing load
      status = 'error'
    }
  }

  function fmtDate(iso: string): string {
    const t = Date.parse(iso)
    return Number.isNaN(t) ? iso : new Date(t).toISOString().slice(0, 10)
  }

  // ConfirmDialog awaits this — a throw keeps the dialog open and shows the error
  // (deny-fault: a 404 grant-not-found stays visible, no silent drop).
  async function confirmRevoke(): Promise<void> {
    const g = revoking
    if (g === null) return
    await deleteTenantGrant(g.id)
    revoking = null
    await load(tenantId)
  }

  // Successful create → close the modal and re-read so the new grant shows.
  async function onCreated(): Promise<void> {
    createOpen = false
    await load(tenantId)
  }
</script>

<section class="card" aria-label="cross-tenant read grants">
  <header class="card-head">
    <h2>read grants</h2>
    <span class="count">{status === 'ready' ? grants.length : ''}</span>
    <button type="button" class="cta" onclick={() => (createOpen = true)}>+ Grant scope access</button>
  </header>
  <p class="note">
    Scopes owned by OTHER tenants that <code>{tenantSlug}</code> may read. A grant takes effect at the grantee's next
    auth. Revoking is the safe direction (no cross-tenant opt-in needed).
  </p>

  {#if status === 'loading' || status === 'idle'}
    <p class="state" aria-busy="true">loading grants…</p>
  {:else if status === 'error'}
    <div class="error" role="alert">
      <p>{loadError?.message}</p>
      {#if loadError?.requestId}<p class="request-id">request {loadError.requestId}</p>{/if}
      <button type="button" onclick={() => void load(tenantId)}>Retry</button>
    </div>
  {:else}
    <Table label="read grants" empty={grants.length === 0}>
      {#snippet emptyState()}
        <p class="empty" role="status">
          this tenant holds no cross-tenant read grants — it can read only its own scopes.
        </p>
      {/snippet}
      {#snippet head()}
        <tr>
          <th>granted scope</th>
          <th>granted</th>
          <th class="actions-col">actions</th>
        </tr>
      {/snippet}
      {#each grants as g (g.id)}
        <tr>
          <td class="scope">{g.granted_scope}</td>
          <td class="mono">{fmtDate(g.created_at)}</td>
          <td class="row-actions">
            <button type="button" class="link danger" onclick={() => (revoking = g)}>Revoke</button>
          </td>
        </tr>
      {/each}
    </Table>
  {/if}
</section>

{#if createOpen}
  <GrantCreateModal {tenantId} {tenantSlug} onclose={() => (createOpen = false)} onsaved={() => void onCreated()} />
{/if}

{#if revoking !== null}
  <ConfirmDialog
    title="Revoke read grant"
    message={`Revoke ${tenantSlug}'s read access to "${revoking.granted_scope}"? It loses the grant at its next auth.`}
    confirmLabel="Revoke grant"
    danger
    onconfirm={confirmRevoke}
    oncancel={() => (revoking = null)}
  />
{/if}

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
  .cta {
    margin-left: auto;
    font-family: var(--font-ui);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-3);
    background: var(--surface-2);
    border: 1px solid var(--accent);
    border-radius: var(--radius);
    color: var(--accent);
    cursor: pointer;
  }
  .note {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    color: var(--text-dim);
    font-size: var(--fs-sm);
    border-bottom: 1px solid var(--border);
  }
  .note code {
    font-family: var(--font-mono);
    color: var(--accent);
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
    margin: var(--space-3);
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
  .scope {
    font-family: var(--font-mono);
    color: var(--text);
  }
  .mono {
    font-family: var(--font-mono);
    color: var(--text-dim);
  }
  .actions-col {
    text-align: right;
  }
  .row-actions {
    display: flex;
    gap: var(--space-2);
    justify-content: flex-end;
    white-space: nowrap;
  }
  button.link {
    background: transparent;
    border: none;
    padding: 0;
    font-family: var(--font-ui);
    font-size: var(--fs-sm);
    color: var(--accent);
    cursor: pointer;
  }
  button.link.danger {
    color: var(--danger);
  }
</style>
