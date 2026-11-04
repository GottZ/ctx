<script lang="ts">
  // Model API area (design/05 §4.6, wave W05-05) — the READ surface over the
  // gateway consumers: the keyset-paged list with status filter, label-prefix
  // search and (server-admin) tenant filter, polled every 30 s while the tab is
  // visible (ConsumersModel). Creating, deactivating and revoking are W05-06 —
  // this page carries no create button and no mutation control. Strings are
  // English inline like every key/admin surface (design/05 §4.6 "Strings").
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
  import { untrack } from 'svelte'
  import { session } from '../../lib/auth.svelte'
  import { CONSUMER_STATUSES, DEFAULT_CONSUMER_STATUSES } from '../../lib/api/modelapi'
  import Table from '../../lib/ui/Table.svelte'
  import { ConsumersModel } from './consumers.svelte'
  import { STATUS_LABEL, consumerTone, fmtDay, fmtMinute, keyTone } from './format'

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
    untrack(() => model.start())
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
  // serves the SPA (no secret; design/05 §4.6 empty state).
  const baseUrl = typeof location === 'undefined' ? '/v1' : `${location.origin}/v1`
</script>

<section class="area">
  <header>
    <h1>Model API</h1>
    <p class="sub">
      Consumers of the OpenAI-compatible endpoint — external programs and ctx peers, each reaching the models with its
      own <code>ctxm_</code> keys. Status is computed by the server; this view is read-only.
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
        <!-- read-only rows carry no focusable control: the scroll region itself
             is keyboard-reachable on narrow viewports (axe scrollable-region-focusable) -->
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
            </tr>
          {/snippet}
          {#each model.consumers as c (c.id)}
            <tr data-consumer-row class:off={!c.active}>
              <td class="label">
                <span class="label-text">{c.label}</span>
                {#if c.description}<span class="desc">{c.description}</span>{/if}
              </td>
              <td><span class="badge" class:peer={c.kind === 'ctx-peer'}>{c.kind}</span></td>
              <td>
                <span class="badge {consumerTone(c.status)}" data-consumer-status
                  >{STATUS_LABEL[c.status] ?? c.status}</span
                >
              </td>
              {#if crossTenant}<td><span class="chip">{c.tenant_slug}</span></td>{/if}
              <td class="keys">
                {#if c.keys.length === 0}
                  <span class="dim">no live key</span>
                {:else}
                  <ul>
                    {#each c.keys as k (k.id)}
                      <li>
                        <code class="hint">{k.hint}</code>
                        <span class="badge {keyTone(k.status)}" data-key-status>{k.status}</span>
                        <span class="when">{k.expires_at ? `expires ${fmtDay(k.expires_at)}` : 'no expiry'}</span>
                      </li>
                    {/each}
                  </ul>
                {/if}
              </td>
              <td class="when">
                {#if c.last_used_at}
                  {fmtMinute(c.last_used_at)}
                  <span class="approx" title="recorded at one-minute precision">±1 min</span>
                {:else}
                  <span class="dim">never</span>
                {/if}
              </td>
              <td class="when">{fmtDay(c.created_at)}</td>
            </tr>
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
    color: var(--danger);
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
  tr.off .label-text {
    opacity: 0.55;
  }
  .label {
    font-family: var(--font-mono);
  }
  .label-text {
    white-space: nowrap;
  }
  .desc {
    display: block;
    color: var(--text-dim);
    font-family: var(--font-ui);
    font-size: var(--fs-xs);
  }
  .chip {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    color: var(--text-dim);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
    white-space: nowrap;
  }
  .badge {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    text-transform: uppercase;
    letter-spacing: var(--label-tracking);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
    color: var(--text-dim);
    white-space: nowrap;
  }
  .badge.ok {
    color: var(--ok);
    border-color: var(--ok);
  }
  .badge.warn {
    color: var(--warn);
    border-color: var(--warn);
  }
  .badge.peer {
    color: var(--accent);
    border-color: var(--accent);
  }
  .keys ul {
    margin: 0;
    padding: 0;
    list-style: none;
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .keys li {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    white-space: nowrap;
  }
  .hint {
    color: var(--text);
  }
  .dim {
    color: var(--text-faint);
  }
  .when {
    color: var(--text-dim);
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }
  .approx {
    font-size: var(--fs-xs);
    color: var(--text-dim);
  }
  .more {
    display: flex;
    justify-content: center;
    padding: var(--space-2) var(--space-3);
    border-top: 1px solid var(--border);
  }
</style>
