<script lang="ts">
  // Type-registry admin surface (design 04 §4.7/§5.5, wave U10) — the first web
  // surface that makes classification policy WRITABLE (retrieval/guard/dream/
  // parent), until now only reachable via SQL migrations. Server-admin only:
  // the /admin prefix-guard redirects a lower tier (guard.ts TIER_GATED, U04
  // pin); the page ALSO self-gates (banner-not-request) so a non-admin key that
  // somehow reaches here sees the 403 banner, never a doomed write. E04-5: v1 is
  // server-admin — tenant-admin self-service is a later Achse-01 wave.
  //
  // Read via the U03 client (listTypes); write via the U10 additions to the same
  // client (putType/deleteType) — NO hand-rolled fetch. The list + form reuse the
  // shared Table (Q10) and Modal (Q9) primitives; the delete confirm reuses the
  // ConfirmDialog primitive (two-click danger arm + open-on-error for the 409).
  import { session } from '../../../lib/auth.svelte'
  import { ApiError } from '../../../lib/api'
  import type { BlockTypeView } from '../../../lib/api/types'
  import { deleteType, listTypes } from '../../../lib/api/types-registry'
  import Table from '../../../lib/ui/Table.svelte'
  import ConfirmDialog from '../../../lib/components/ConfirmDialog.svelte'
  import TypeForm from './TypeForm.svelte'
  import { canDeleteType, isBuiltin, policySummary } from './types-admin.svelte'

  type Status = 'idle' | 'loading' | 'ready' | 'error'
  let status = $state<Status>('idle')
  let types = $state<BlockTypeView[]>([])
  let loadError = $state<ApiError | null>(null)

  // The form (create = null row) and the delete-confirm target, each a $state row.
  let editing = $state<BlockTypeView | null>(null)
  let formOpen = $state(false)
  let deleting = $state<BlockTypeView | null>(null)

  async function load(): Promise<void> {
    status = 'loading'
    loadError = null
    try {
      const res = await listTypes()
      types = res.types
      status = 'ready'
    } catch (e) {
      loadError = e instanceof ApiError ? e : new ApiError(0, 'internal', String(e))
      status = 'error'
    }
  }

  // Load once the session resolves to server-admin (survives the boot restore
  // race; idle-guard prevents a reload loop) — the AdminPage precedent.
  $effect(() => {
    if (session.tier === 'server-admin' && status === 'idle') void load()
  })

  function openCreate(): void {
    editing = null
    formOpen = true
  }
  function openEdit(t: BlockTypeView): void {
    editing = t
    formOpen = true
  }
  async function onSaved(): Promise<void> {
    formOpen = false
    await load() // re-read the effective registry so the row shows the new policy
  }
  async function confirmDelete(): Promise<void> {
    const t = deleting
    if (t === null) return
    await deleteType(t.name) // a 409 (builtin / in-use) rejects → ConfirmDialog stays open
    deleting = null
    await load()
  }
</script>

<section class="area">
  <header>
    <h1>Types</h1>
    <p class="sub">block-type registry — classification policy per type (server-admin only)</p>
  </header>

  {#if session.tier === 'loading'}
    <p class="state" aria-busy="true">restoring session…</p>
  {:else if session.caps.tier !== 'server-admin'}
    <p class="banner" role="alert">
      server-admin only — the type registry is gated server-side (the server answers 403). Sign in with a
      server-admin key to manage types.
    </p>
  {:else if status === 'loading' || status === 'idle'}
    <p class="state" aria-busy="true">loading types…</p>
  {:else if status === 'error'}
    <div class="error" role="alert">
      <p>{loadError?.message}</p>
      {#if loadError?.requestId}<p class="request-id">request {loadError.requestId}</p>{/if}
      <button type="button" onclick={() => void load()}>Retry</button>
    </div>
  {:else}
    <section class="card" aria-label="type registry">
      <header class="card-head">
        <h2>types</h2>
        <span class="count">{types.length}</span>
        <button type="button" class="cta" onclick={openCreate}>+ New type</button>
      </header>

      <Table label="type registry" empty={types.length === 0}>
        {#snippet emptyState()}
          <p class="empty">no types — the registry is empty</p>
        {/snippet}
        {#snippet head()}
          <tr>
            <th>key</th>
            <th>name</th>
            <th>source</th>
            <th>policy</th>
            <th class="actions-col">actions</th>
          </tr>
        {/snippet}
        {#each types as t (t.id)}
          <tr>
            <td class="key">{t.name}</td>
            <td class="name">{t.display_name || '—'}</td>
            <td>
              <span class="badge" class:builtin={isBuiltin(t)}>
                {isBuiltin(t) ? 'builtin' : 'tenant'}
              </span>
            </td>
            <td class="policy">{policySummary(t)}</td>
            <td class="row-actions">
              <button type="button" class="link" onclick={() => openEdit(t)}>Edit</button>
              <button
                type="button"
                class="link danger"
                disabled={!canDeleteType(t)}
                title={canDeleteType(t) ? 'delete this type' : 'builtin types cannot be deleted'}
                onclick={() => (deleting = t)}
              >
                Delete
              </button>
            </td>
          </tr>
        {/each}
      </Table>
    </section>
  {/if}
</section>

{#if formOpen}
  <TypeForm type={editing} onclose={() => (formOpen = false)} onsaved={() => void onSaved()} />
{/if}

{#if deleting !== null}
  <ConfirmDialog
    title="Delete type"
    message={`Delete the type "${deleting.name}"? This is refused if any block still references it.`}
    confirmLabel="Delete type"
    danger
    onconfirm={confirmDelete}
    oncancel={() => (deleting = null)}
  />
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
  h1 {
    margin: 0;
    font-size: var(--fs-xl);
    font-weight: var(--fw-semibold);
    letter-spacing: var(--track-1);
  }
  .sub {
    margin: var(--space-1) 0 0;
    color: var(--text-dim);
    font-size: var(--fs-sm);
  }
  .banner {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--warn);
    border-radius: var(--radius);
    color: var(--warn);
    font-size: var(--fs-sm);
  }
  .state {
    margin: 0;
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
  .empty {
    margin: 0;
    padding: var(--space-3);
    color: var(--text-faint);
    font-size: var(--fs-sm);
  }
  .key {
    font-family: var(--font-mono);
    color: var(--text);
  }
  .name {
    color: var(--text-dim);
  }
  .policy {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    color: var(--text-dim);
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
  .badge.builtin {
    border-color: var(--accent);
    color: var(--accent);
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
  button.link:disabled {
    color: var(--text-faint);
    cursor: not-allowed;
  }
</style>
