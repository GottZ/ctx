<script lang="ts">
  // Model API area (design/05 §4.6, waves W05-05 + W05-06) — the gateway
  // consumers: the keyset-paged list with status filter, label-prefix search
  // and (server-admin) tenant filter, polled every 30 s while the tab is
  // visible (ConsumersModel); since W05-06 "+ New consumer" (only with
  // caps.mintModelKeys — server-admin, or tenant-admin with gateway.enabled
  // AND gateway.tenant_self_service) opening ConsumerCreateDialog with the
  // reveal-once key, and per row Revoke (each live key) / Deactivate — both
  // behind a danger ConfirmDialog that requires the last 4 hint characters —
  // and Reactivate (plain confirm, only where the server grants it). After a
  // revoke the toast reports what the SERVER says about running streams
  // (cancelled_inflight, V-01-e/B-25), never a static "immediately". Every
  // control is a UI mirror; the server decides (V-01-c). Strings are English
  // inline like every key/admin surface (design/05 §4.6 "Strings").
  //
  // Gate: the route is registered flag-independently (dark-launch precedent,
  // routes/index.ts) and guardArea redirects below caps.viewModelApi
  // (server-admin always, tenant-admin only with model_api.enabled) before the
  // page mounts, so neither a member nor a tenant-admin before the switch
  // fires model-consumer-list (pinned by the e2e deny + gateway.spec). The page
  // additionally self-gates on the same cap (defense in depth). Visibility is
  // UX: the server stays the real gate (model-consumer-list is tierTenantAdmin).
  //
  // Every status shown is the server's verdict (B-11): the page renders
  // consumer.status / key.status and never derives one from expires_at.
  //
  // RC-D C5: per row "Edit policy" (W05-07, ConsumerEditDialog — lazy chunk,
  // field tiers mirrored, never on a ctx-peer row below server-admin), "Rotate
  // key" (W05-08, RotateDialog — lazy chunk, leak = grace 0 preselected, the
  // second reveal-once path) and on a rotating consumer's predecessor "End
  // grace period now" — a danger confirm, then model-key-revoke with THAT
  // key's id; the successor stays valid.
  import { untrack } from 'svelte'
  import { tenantFilterFromSearch } from './bridge'
  import { session } from '../../lib/auth.svelte'
  import { CONSUMER_STATUSES, DEFAULT_CONSUMER_STATUSES } from '../../lib/api/modelapi'
  import type { ConsumerView, KeyView } from '../../lib/api/types'
  import ConfirmDialog from '../../lib/components/ConfirmDialog.svelte'
  import Table from '../../lib/ui/Table.svelte'
  import ConsumerRow from './ConsumerRow.svelte'
  import { ConsumersModel } from './consumers.svelte'
  import { STATUS_LABEL, cancelledText } from './format'
  import { canRotate } from './rotate'
  import { canEditPolicy, canReactivate, deactivateTyped, hintTail } from './rows'

  const model = new ConsumersModel()

  // Load + poll only behind the cap; an $effect (not onMount) so the load
  // starts the moment the cap resolves true and stops (dispose) when it turns
  // false or the page unmounts. This self-gate is defense in depth and NOT
  // what the tests pin: the gate that keeps model-consumer-list from firing
  // below viewModelApi is the route guard (guardArea redirects in beforeLoad,
  // before this page mounts; e2e deny + gateway.spec assert the absent call),
  // and the shell mounts only after whoami resolved (App.svelte), so the
  // 'loading' branch below is a fallback. untrack: start() reads and writes
  // model state, which must not become a dependency of this effect.
  $effect(() => {
    if (!session.caps.viewModelApi) return
    untrack(() => {
      // W05-20 bridge: /gateway?tenant=<id> (from /admin/tenants/:id) opens
      // the list filtered to that tenant — server-admin only (bridge.ts).
      const bridged = tenantFilterFromSearch(location.search, session.caps.viewTenants)
      if (bridged !== null) model.filter = { ...model.filter, tenantId: bridged }
      model.start()
    })
    return () => model.dispose()
  })

  // Server-admin: the tenant filter options (tenant-list is server-admin only).
  $effect(() => {
    if (!session.caps.viewModelApi || !session.caps.viewTenants) return
    untrack(() => void model.loadTenants())
  })

  const gatewayOn = $derived(session.whoami?.capabilities?.model_api?.enabled === true)
  const crossTenant = $derived(session.caps.viewTenants)

  // The search field is committed on submit (Enter / Search), not per keystroke.
  let query = $state('')
  function submitSearch(e: SubmitEvent): void {
    e.preventDefault()
    void model.setFilter({ q: query })
  }

  // Filter beyond the server default? Drives the empty-state wording only.
  const filtered = $derived(
    model.filter.q !== '' ||
      model.filter.tenantId !== null ||
      model.filter.statuses.join() !== DEFAULT_CONSUMER_STATUSES.join(),
  )

  // The OpenAI-compatible base URL of THIS instance — the same origin that
  // serves the SPA (no secret; design/05 §4.6 empty state + beside the reveal).
  const baseUrl = typeof location === 'undefined' ? '/v1' : `${location.origin}/v1`

  // --- W05-06 mutations ---
  const serverAdmin = $derived(session.caps.tier === 'server-admin')
  let creating = $state(false)
  // The create dialog is its own lazy chunk: only a click on "+ New consumer"
  // loads it (the list's first load stays within the default chunk budget).
  const loadCreateDialog = () => import('./ConsumerCreateDialog.svelte')
  // RC-D C5: the edit and rotate dialogs are lazy chunks of their own too.
  const loadEditDialog = () => import('./ConsumerEditDialog.svelte')
  const loadRotateDialog = () => import('./RotateDialog.svelte')
  let editing = $state<ConsumerView | null>(null)
  let rotating = $state<ConsumerView | null>(null)
  const rotationGraceS = $derived(session.whoami?.capabilities?.model_api?.rotation_grace_s ?? null)
  const tierCaps = $derived({ serverAdmin, mintModelKeys: session.caps.mintModelKeys })
  type Pending =
    | { kind: 'revoke'; consumer: ConsumerView; key: KeyView }
    | { kind: 'endgrace'; consumer: ConsumerView; key: KeyView }
    | { kind: 'deactivate'; consumer: ConsumerView }
    | { kind: 'reactivate'; consumer: ConsumerView }
  // The row awaiting confirmation (null = no dialog); a fresh ConfirmDialog
  // mounts per target so showModal re-fires (precedent TenantPage revoke).
  let pending = $state<Pending | null>(null)
  /** Outcome line of the last row mutation — the server's word on running streams. */
  let toast = $state<string | null>(null)

  function request(p: Pending): void {
    toast = null
    pending = p
  }

  const PEER_WARN = ' This consumer is a ctx peer — its federation link breaks.'

  // ConfirmDialog awaits this: a throw keeps it open with the error (the
  // model rethrows after setting actionError); success sets the toast from
  // the server's answer and unmounts the dialog after its own close().
  async function confirmPending(): Promise<void> {
    const p = pending
    if (p === null) return
    if (p.kind === 'endgrace') {
      // the predecessor's OWN key id — never the consumer, never the successor (B-20)
      const res = await model.revokeKey(p.key.id)
      toast = `Grace period of key ${p.key.hint} of ${p.consumer.label} ended — the old key is revoked, the new key stays valid. ${cancelledText(res.cancelled_inflight)}`
    } else if (p.kind === 'revoke') {
      const res = await model.revokeKey(p.key.id)
      toast = `Key ${p.key.hint} of ${p.consumer.label} revoked — new requests are refused. ${cancelledText(res.cancelled_inflight)}`
    } else if (p.kind === 'deactivate') {
      const res = await model.deactivate(p.consumer.id)
      toast = `${p.consumer.label} deactivated and its keys revoked — new requests are refused. ${cancelledText(res.cancelled_inflight)}`
    } else {
      await model.reactivate(p.consumer.id)
      toast = `${p.consumer.label} reactivated — it has no live key until a new one is issued.`
    }
    setTimeout(() => (pending = null), 0)
  }
</script>

<section class="area">
  <header>
    <h1>Model API</h1>
    <p class="sub">
      Consumers of the OpenAI-compatible endpoint — external programs and ctx peers, each reaching the models with its
      own <code>ctxm_</code> keys. Status is computed by the server.
    </p>
  </header>

  {#if session.caps.tier === 'loading'}
    <p class="state" aria-busy="true">resolving session…</p>
  {:else if !session.caps.viewModelApi}
    <p class="banner" role="alert">
      The Model API area needs a tenant-admin key while the gateway is switched on — it is hidden for this key.
    </p>
  {:else}
    {#if crossTenant && !gatewayOn}
      <p class="note" role="status">
        gateway.enabled is off — /v1 is not mounted and tenant-admins do not see this area yet. Consumers prepared here
        take effect once the gateway is switched on.
      </p>
    {/if}

    <form class="toolbar" role="search" aria-label="filter consumers" onsubmit={submitSearch}>
      <fieldset class="statuses">
        <legend>status</legend>
        {#each CONSUMER_STATUSES as s (s)}
          {@const on = model.filter.statuses.includes(s)}
          <label class="chk">
            <input
              type="checkbox"
              checked={on}
              disabled={on && model.filter.statuses.length === 1}
              onchange={() => void model.toggleStatus(s)}
            />
            {STATUS_LABEL[s]}
          </label>
        {/each}
        <label class="chk all">
          <input
            type="checkbox"
            checked={model.allStatuses}
            onchange={(e) => void model.setAllStatuses(e.currentTarget.checked)}
          />
          all
        </label>
      </fieldset>
      <div class="field">
        <label for="gw-q">label prefix</label>
        <input id="gw-q" type="search" bind:value={query} placeholder="e.g. ci-" autocomplete="off" />
        <button type="submit">Search</button>
      </div>
      {#if crossTenant}
        <div class="field">
          <label for="gw-tenant">tenant</label>
          <select
            id="gw-tenant"
            value={model.filter.tenantId ?? ''}
            onchange={(e) => void model.setFilter({ tenantId: e.currentTarget.value || null })}
          >
            <option value="">all tenants</option>
            {#each model.tenants as t (t.id)}
              <option value={t.id}>{t.slug} — {t.display_name}</option>
            {/each}
          </select>
          {#if model.tenantsError}
            <span class="field-error" role="status">tenant list unavailable: {model.tenantsError}</span>
          {/if}
        </div>
      {/if}
    </form>

    {#if session.caps.mintModelKeys}
      <div class="page-actions">
        <button type="button" class="primary" data-testid="consumer-new" onclick={() => (creating = true)}>
          + New consumer
        </button>
      </div>
    {/if}

    {#if toast}
      <div class="toast" role="status" data-testid="gateway-toast">
        <span>{toast}</span>
        <button type="button" class="x" aria-label="dismiss" onclick={() => (toast = null)}>×</button>
      </div>
    {/if}
    {#if model.actionError && pending === null}
      <p class="action-error" role="alert">{model.actionError}</p>
    {/if}

    {#if model.refreshError}
      <p class="note warn" role="status">refresh failed: {model.refreshError} — showing the last loaded list</p>
    {/if}

    {#if model.status === 'idle' || model.status === 'loading'}
      <p class="state" aria-busy="true">loading consumers…</p>
    {:else if model.status === 'error'}
      <div class="error" role="alert">
        <p>{model.loadError?.message ?? 'failed to load consumers'}</p>
        {#if model.loadError?.requestId}<p class="request-id">request {model.loadError.requestId}</p>{/if}
        <button type="button" onclick={() => void model.reload()}>Retry</button>
      </div>
    {:else if model.consumers.length === 0}
      <div class="empty" role="status">
        <p class="empty-title">No consumers to show</p>
        <p class="empty-hint">
          {filtered
            ? 'No consumer matches this filter — widen the status set or clear the search.'
            : 'No model consumer exists yet.'}
          External programs reach the models through the OpenAI-compatible endpoint with a <code>ctxm_</code> key issued
          to a consumer.
        </p>
        <p class="base-url">base URL <code>{baseUrl}</code></p>
      </div>
    {:else}
      <section class="card" aria-label="model consumers">
        <header class="card-head">
          <h2>consumers</h2>
          <span class="count">{model.consumers.length}{model.nextCursor !== null ? '+' : ''}</span>
        </header>
        <!-- the scroll region stays keyboard-reachable on narrow viewports even
             when no row carries a control (e.g. only inactive rows for a key
             without self-service — axe scrollable-region-focusable) -->
        <Table label="model consumers" focusableScroll>
          {#snippet head()}
            <tr>
              <th>label</th>
              <th>kind</th>
              <th>status</th>
              {#if crossTenant}<th>tenant</th>{/if}
              <th>keys</th>
              <th>last used</th>
              <th>created</th>
              <th>actions</th>
            </tr>
          {/snippet}
          {#each model.consumers as c (c.id)}
            <ConsumerRow
              consumer={c}
              {crossTenant}
              reactivatable={canReactivate(c, { serverAdmin, mintModelKeys: session.caps.mintModelKeys })}
              busy={model.busyId !== null && (model.busyId === c.id || c.keys.some((k) => k.id === model.busyId))}
              onrevoke={(k) => request({ kind: 'revoke', consumer: c, key: k })}
              ondeactivate={() => request({ kind: 'deactivate', consumer: c })}
              onreactivate={() => request({ kind: 'reactivate', consumer: c })}
              editable={canEditPolicy(c, tierCaps)}
              rotatable={canRotate(c, tierCaps)}
              onedit={() => {
                toast = null
                editing = c
              }}
              onrotate={() => {
                toast = null
                rotating = c
              }}
              onendgrace={(k) => request({ kind: 'endgrace', consumer: c, key: k })}
            />
          {/each}
        </Table>
        {#if model.nextCursor !== null}
          <div class="more">
            <button type="button" disabled={model.loadingMore} onclick={() => void model.loadMore()}>
              {model.loadingMore ? 'loading…' : 'Load more'}
            </button>
          </div>
        {/if}
      </section>
      {#if model.moreError}
        <p class="action-error" role="alert">{model.moreError}</p>
      {/if}
    {/if}
  {/if}
</section>

{#if creating}
  {#await loadCreateDialog()}
    <p class="sr-status" role="status" aria-busy="true">loading the create dialog…</p>
  {:then { default: CreateDialog }}
    <CreateDialog {model} tenants={model.tenants} {baseUrl} onclose={() => (creating = false)} />
  {:catch err}
    <p class="action-error" role="alert">create dialog unavailable: {err instanceof Error ? err.message : String(err)}</p>
  {/await}
{/if}

{#if editing !== null}
  {#await loadEditDialog()}
    <p class="sr-status" role="status" aria-busy="true">loading the edit dialog…</p>
  {:then { default: EditDialog }}
    <EditDialog
      {model}
      consumer={editing}
      onsaved={(label) => (toast = `Policy of ${label} saved.`)}
      onclose={() => (editing = null)}
    />
  {:catch err}
    <p class="action-error" role="alert">edit dialog unavailable: {err instanceof Error ? err.message : String(err)}</p>
  {/await}
{/if}

{#if rotating !== null}
  {#await loadRotateDialog()}
    <p class="sr-status" role="status" aria-busy="true">loading the rotate dialog…</p>
  {:then { default: RotateDialog }}
    <RotateDialog {model} consumer={rotating} {rotationGraceS} onclose={() => (rotating = null)} />
  {:catch err}
    <p class="action-error" role="alert">rotate dialog unavailable: {err instanceof Error ? err.message : String(err)}</p>
  {/await}
{/if}

{#if pending?.kind === 'endgrace'}
  <ConfirmDialog
    danger
    title="End grace period"
    confirmLabel="End grace period"
    message={`End the grace period of key ${pending.key.hint} of "${pending.consumer.label}" now? The old key is revoked at once. The new key stays valid.`}
    onconfirm={confirmPending}
    oncancel={() => (pending = null)}
  />
{:else if pending?.kind === 'revoke'}
  <ConfirmDialog
    danger
    title="Revoke key"
    requireTyped={hintTail(pending.key.hint)}
    confirmLabel="Revoke"
    message={`Revoke key ${pending.key.hint} of "${pending.consumer.label}"? New requests are refused immediately.${pending.consumer.kind === 'ctx-peer' ? PEER_WARN : ''}`}
    onconfirm={confirmPending}
    oncancel={() => (pending = null)}
  />
{:else if pending?.kind === 'deactivate'}
  <ConfirmDialog
    danger
    title="Deactivate consumer"
    requireTyped={deactivateTyped(pending.consumer)}
    confirmLabel="Deactivate"
    message={`Deactivate "${pending.consumer.label}" and revoke all its keys? New requests are refused immediately.${pending.consumer.kind === 'ctx-peer' ? PEER_WARN : ''}`}
    onconfirm={confirmPending}
    oncancel={() => (pending = null)}
  />
{:else if pending?.kind === 'reactivate'}
  <ConfirmDialog
    title="Reactivate consumer"
    confirmLabel="Reactivate"
    message={`Reactivate "${pending.consumer.label}"? It comes back with status ${STATUS_LABEL.no_key} — its old keys stay revoked; a new key has to be issued.`}
    onconfirm={confirmPending}
    oncancel={() => (pending = null)}
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
  code {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
  }

  .banner {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--warn);
    border-radius: var(--radius);
    color: var(--warn);
    font-size: var(--fs-sm);
  }
  .note {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text-dim);
    font-size: var(--fs-sm);
  }
  .note.warn {
    border-color: var(--warn);
    color: var(--warn);
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
    /* --text, not --danger: light-theme --danger on --danger-dim over the
       canvas measured 3.96:1 by axe (RC-D S3 probe; < 4.5 AA) — the box's
       border and tint carry the tone */
    color: var(--text);
  }
  .request-id {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    color: var(--text-dim) !important;
  }
  .field-error {
    color: var(--danger);
    font-size: var(--fs-xs);
  }
  .action-error {
    margin: 0;
    color: var(--danger);
    font-size: var(--fs-sm);
  }

  .toolbar {
    display: flex;
    align-items: flex-end;
    gap: var(--space-3);
    flex-wrap: wrap;
  }
  .statuses {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-1) var(--space-3);
    margin: 0;
    padding: var(--space-1) var(--space-2);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
  legend {
    padding: 0 var(--space-1);
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-dim);
  }
  .chk {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    min-height: 24px;
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    color: var(--text);
    cursor: pointer;
  }
  .chk.all {
    color: var(--text-dim);
  }
  .field {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
  }
  .field label {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-dim);
  }
  input[type='search'],
  select {
    min-height: 24px;
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-2);
    background: var(--surface-2);
    color: var(--text);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
  }
  button {
    min-height: 24px;
    font-family: var(--font-ui);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-3);
    background: var(--surface-2);
    color: var(--text);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    cursor: pointer;
  }
  button:hover:not(:disabled) {
    border-color: var(--accent);
    color: var(--accent);
  }
  button:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }

  .empty {
    border: 1px dashed var(--border-strong);
    border-radius: var(--radius);
    padding: var(--space-3);
    background: var(--surface-1);
  }
  .empty-title {
    margin: 0;
    color: var(--text);
    font-size: var(--fs-md);
    font-weight: var(--fw-semibold);
  }
  .empty-hint {
    margin: var(--space-1) 0 0;
    color: var(--text-dim);
    font-size: var(--fs-sm);
  }
  .base-url {
    margin: var(--space-2) 0 0;
    color: var(--text-dim);
    font-size: var(--fs-sm);
  }
  .base-url code {
    color: var(--text);
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
  .sr-status {
    position: absolute;
    width: 1px;
    height: 1px;
    overflow: hidden;
    clip-path: inset(50%);
    white-space: nowrap;
  }
  .page-actions {
    display: flex;
    justify-content: flex-end;
  }
  button.primary {
    border-color: var(--accent);
    color: var(--accent);
  }
  .toast {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    background: var(--surface-1);
    color: var(--text);
    font-size: var(--fs-sm);
  }
  .toast .x {
    min-height: 0;
    border: none;
    background: transparent;
    color: var(--text-dim);
    padding: 0 var(--space-1);
    font-size: var(--fs-md);
  }
  .more {
    display: flex;
    justify-content: center;
    padding: var(--space-2) var(--space-3);
    border-top: 1px solid var(--border);
  }
</style>
