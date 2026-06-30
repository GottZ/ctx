<script lang="ts">
  // Tenant-admin area shell (design 05-tenant-admin §1/§3, Waves TK3–TK6) — the
  // surface over the tenant's API keys (the keys ARE the member-proxys). Now
  // functional: list (TK3) + create-with-show-once-reveal (TK4) + revoke with
  // confirm, show-revoked toggle and self-revoke guard (TK5) + read-only quota
  // (TK6). All mutations go through the KeysModel; the child dialogs/cards fill
  // their own files. The server stays authoritative on every write.
  //
  // Self-gate: manageTenantKeys (tenant-admin+ or server-admin, capabilitiesFor
  // §3) mirrors the server's requireTenantAdmin — a member key gets a banner,
  // not a doomed 403 request. The gate is UX only; the server is authoritative.
  import ConfirmDialog from '../../lib/components/ConfirmDialog.svelte'
  import { session } from '../../lib/auth.svelte'
  import type { ApiKeyView, TenantRole } from '../../lib/api/types'
  import { KeysModel } from './keys.svelte'
  import { ScopesSelfModel } from './scopes.svelte' // FE-9 / SS1
  import { activeOwnerCount, controlDisabled } from './role-guards'
  import KeyCreateDialog from './KeyCreateDialog.svelte'
  import QuotaCard from './QuotaCard.svelte'
  import ScopeSelfCard from './ScopeSelfCard.svelte' // FE-9 / SS1
  import TenantUsageCard from './TenantUsageCard.svelte' // FE-10 / SS2

  // KeysModel carries its own $state runes, so a plain const instance is
  // reactive in the template (§ Svelte-5 class-runes). Load is driven by an
  // $effect (not onMount) so a boot-time restore race — caps='loading' at mount
  // (§6/R6) — still triggers the fetch the moment access resolves.
  const keys = new KeysModel()

  // FE-9 / SS1: the single self-service scope model — drives BOTH the scope card
  // (list + create) and the usage card (FE-10), and the key-create CTA's
  // atKeyLimit gate (FE-10). One instance → one load (scope-list + tenant-usage-
  // get in parallel) per visit. Same class-runes reactivity as KeysModel.
  const scopesModel = new ScopesSelfModel()

  // TK4: create-dialog open flag. TK5: the row pending revoke (null = no dialog)
  // — a fresh ConfirmDialog mounts per target so showModal re-fires each time.
  let creating = $state(false)
  let revokeTarget = $state<ApiKeyView | null>(null)

  // TK7b: active owners in the (server-tenant-isolated) list — drives the
  // last-owner guard so a demote/deactivate that would orphan the tenant is
  // disabled. $derived so it re-counts whenever the list reloads after a mutation.
  const ownerCount = $derived(activeOwnerCount(keys.keys))

  $effect(() => {
    if (session.caps.manageTenantKeys && keys.status === 'idle') {
      void keys.load()
    }
  })

  // FE-9 / SS1: same gated, idle-guarded load as the keys table — a tenant-admin
  // is the only tier the scope-list / tenant-usage-get actions answer for (server
  // authoritative). The $effect (not onMount) re-fires the moment a boot restore
  // resolves caps from 'loading'.
  $effect(() => {
    if (session.caps.manageTenantKeys && scopesModel.status === 'idle') {
      void scopesModel.load()
    }
  })

  // TK7b: promote/demote a key's tenant_role. The <select> is server-controlled
  // (one-way value + onchange, like FilterPanel) — on success keys.update reloads
  // and the select re-syncs to the authoritative role. A rejected change (last-
  // owner 409 / self-lockout 403) is swallowed here: keys.update already surfaced
  // it on actionError and rethrew, so we only stop the unhandled rejection. The
  // server stays authoritative; the disabled state is comfort only.
  async function changeRole(k: ApiKeyView, role: TenantRole): Promise<void> {
    if (role === k.tenant_role) return
    try {
      await keys.update({ id: k.id, tenant_role: role })
    } catch {
      /* actionError set by the model; the select stays on the rejected value until
         the next list refresh — the server is the source of truth (design §3). */
    }
  }

  // TK7b: (de)activate a key. Reactivation (active:false→true) is the toggle's
  // unique power — revoke (soft-delete, TK5) can only deactivate. Same swallow.
  async function toggleActive(k: ApiKeyView): Promise<void> {
    try {
      await keys.update({ id: k.id, active: !k.active })
    } catch {
      /* actionError set by the model; k.active is unchanged so the button label
         stays correct without a resync. */
    }
  }

  function requestRevoke(k: ApiKeyView): void {
    // Self-revoke is hard-disabled in the UI (button never reaches here for the
    // own key), but guard once more so the dialog can never target it (§6/§7.5).
    if (isOwnKey(k.id)) return
    revokeTarget = k
  }

  function confirmRevoke(): Promise<void> {
    const target = revokeTarget
    if (target === null) return Promise.resolve()
    // remove() throws on failure → ConfirmDialog stays open and surfaces the
    // 404-no-oracle "key not found" (neutral, no foreign-key oracle). On success
    // it reloaded the list; defer the unmount to a macrotask so ConfirmDialog
    // finishes its own close() before {#if revokeTarget} tears it down.
    return keys.remove(target.id).then(() => {
      setTimeout(() => (revokeTarget = null), 0)
    })
  }

  function isOwnKey(id: string): boolean {
    return session.apiKeyId !== null && id === session.apiKeyId
  }

  // tenant_role is NOT on the list SELECT until the additive read-enrichment
  // (LÜCKE 1, TK7a/TK7b) lands — so it is optional on ApiKeyView. We render the
  // badge ONLY when the field is actually present; absent → '—' (faint). This
  // never fakes 'member' for everyone (design §6 "nie raten") while keeping the
  // column ready for TK7b.
  function roleClass(role: string | undefined): string {
    if (role === 'owner') return 'role-owner'
    if (role === 'admin') return 'role-admin'
    return 'role-member'
  }

  // last_used_at is omitempty (null until first use) → render '—', never an
  // epoch / Invalid-Date (design §6, types.ts:574).
  function fmtUsed(ts: string | undefined): string {
    if (!ts) return '—'
    const d = new Date(ts)
    return Number.isNaN(d.getTime()) ? '—' : d.toLocaleString()
  }
</script>

<section class="area">
  <header>
    <h1>Tenant</h1>
    <p class="sub">
      API keys for <strong>{session.tenantDisplayName ?? session.tenantSlug ?? 'your tenant'}</strong> — they ARE the
      tenant's member proxys. Create, reveal once, and revoke them here; quota is read-only.
    </p>
  </header>

  {#if session.caps.tier === 'loading'}
    <p class="state" aria-busy="true">resolving session…</p>
  {:else if !session.caps.manageTenantKeys}
    <p class="banner" role="alert">
      tenant-admin access required — your key is a member of this tenant, so key management is hidden (the server answers
      403). Ask an owner or admin of the tenant to manage keys.
    </p>
  {:else}
    <div class="toolbar">
      <!-- FE-10 / SS2: the key-create CTA is disabled at the tenant's key cap
           (atKeyLimit, server-authoritative — it 429s a mint over the limit). -->
      <button
        type="button"
        class="cta"
        disabled={scopesModel.atKeyLimit}
        title={scopesModel.atKeyLimit
          ? 'key limit reached — revoke a key or ask a server-admin to raise the cap'
          : undefined}
        onclick={() => (creating = true)}>+ New key</button>
      <label class="toggle">
        <input
          type="checkbox"
          checked={keys.showRevoked}
          disabled={keys.status === 'loading'}
          onchange={() => void keys.toggleRevoked()}
        />
        show revoked
      </label>
    </div>

    {#if scopesModel.atKeyLimit}
      <!-- FE-10 / SS2 -->
      <p class="limit" role="status">
        key limit reached ({scopesModel.usage?.key_count}/{scopesModel.usage?.max_keys}) — revoke a key or ask a
        server-admin to raise the cap before minting another.
      </p>
    {/if}

    {#if keys.actionError}
      <p class="action-error" role="alert">{keys.actionError}</p>
    {/if}

    {#if keys.status === 'loading' || keys.status === 'idle'}
      <p class="state" aria-busy="true">loading tenant keys…</p>
    {:else if keys.status === 'error'}
      <div class="error" role="alert">
        <p>{keys.loadError?.message ?? 'failed to load tenant keys'}</p>
        {#if keys.loadError?.requestId}<p class="request-id">request {keys.loadError.requestId}</p>{/if}
        <button type="button" onclick={() => void keys.reload()}>Retry</button>
      </div>
    {:else if keys.keys.length === 0}
      <div class="empty" role="status">
        <p class="empty-title">No keys to show</p>
        <p class="empty-hint">
          Mint one with <strong>New key</strong> above. If you've revoked keys before, toggle <strong>show revoked</strong>
          to see them.
        </p>
      </div>
    {:else}
      <section class="card" aria-label="tenant API keys">
        <header class="card-head">
          <h2>api keys</h2>
          <span class="count">{keys.keys.length}</span>
        </header>
        <div class="scroll">
          <table>
            <thead>
              <tr>
                <th>label</th>
                <th>role</th>
                <th>home scope</th>
                <th>allowed scopes</th>
                <th>status</th>
                <th>last used</th>
                <th>created</th>
                <th class="col-action">revoke</th>
              </tr>
            </thead>
            <tbody>
              {#each keys.keys as k (k.id)}
                <tr class:own={isOwnKey(k.id)} class:off={!k.active}>
                  <td class="label">
                    <span class="label-text">{k.label}</span>
                    {#if isOwnKey(k.id)}<span class="this-key" title="the key you are signed in with">this key</span>{/if}
                  </td>
                  <td>
                    {#if k.tenant_role}
                      <select
                        class="role-select {roleClass(k.tenant_role)}"
                        aria-label={`role for ${k.label}`}
                        value={k.tenant_role}
                        disabled={controlDisabled(k, ownerCount, isOwnKey(k.id), keys.busyId === k.id)}
                        onchange={(e) => void changeRole(k, e.currentTarget.value as TenantRole)}
                      >
                        <option value="member">member</option>
                        <option value="admin">admin</option>
                        <option value="owner">owner</option>
                      </select>
                    {:else}
                      <span class="dim">—</span>
                    {/if}
                  </td>
                  <td><span class="chip">{k.home_scope}</span></td>
                  <td class="scopes">
                    {#if k.allowed_scopes.length === 0}
                      <span class="dim">—</span>
                    {:else}
                      {#each k.allowed_scopes as s (s)}<span class="chip">{s}</span>{/each}
                    {/if}
                  </td>
                  <td class="status-cell">
                    {#if k.active}
                      <span class="badge ok">active</span>
                    {:else}
                      <span class="badge revoked">revoked</span>
                    {/if}
                    <button
                      type="button"
                      class="toggle-active"
                      aria-label={`activation for ${k.label}`}
                      disabled={controlDisabled(k, ownerCount, isOwnKey(k.id), keys.busyId === k.id)}
                      onclick={() => void toggleActive(k)}
                    >
                      {keys.busyId === k.id ? '…' : k.active ? 'deactivate' : 'activate'}
                    </button>
                  </td>
                  <td class="when">{fmtUsed(k.last_used_at)}</td>
                  <td class="when">{k.created_at.slice(0, 10)}</td>
                  <td class="col-action">
                    {#if !k.active}
                      <span class="dim">—</span>
                    {:else if isOwnKey(k.id)}
                      <button
                        type="button"
                        class="revoke"
                        disabled
                        title="This is the key you're signed in with — create and test a replacement key first, then revoke this one from there. No self-service recovery."
                      >
                        revoke
                      </button>
                    {:else}
                      <button
                        type="button"
                        class="revoke danger"
                        disabled={keys.busyId === k.id}
                        onclick={() => requestRevoke(k)}
                      >
                        {keys.busyId === k.id ? '…' : 'revoke'}
                      </button>
                    {/if}
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      </section>
    {/if}

    <!-- FE-9 / SS1: tenant-admin self-service scope provisioning (list + create). -->
    <ScopeSelfCard model={scopesModel} />

    <!-- FE-10 / SS2: structural scope+key usage against the per-tenant caps. -->
    <TenantUsageCard model={scopesModel} />

    <!-- TK6: read-only quota transparency for the tenant's own scope. -->
    <QuotaCard />
  {/if}
</section>

{#if creating}
  <KeyCreateDialog {keys} onclose={() => (creating = false)} />
{/if}

{#if revokeTarget}
  <ConfirmDialog
    danger
    title="Revoke key"
    message={`Revoke "${revokeTarget.label}"? This soft-deletes it immediately — any client using it loses access at once. It cannot be un-revoked from here.`}
    confirmLabel="Revoke"
    onconfirm={confirmRevoke}
    oncancel={() => (revokeTarget = null)}
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
    font-size: 1.35rem;
    font-weight: 600;
    letter-spacing: 0.01em;
  }
  .sub {
    margin: var(--space-1) 0 0;
    color: var(--text-dim);
    font-size: 0.85rem;
  }
  .sub strong {
    color: var(--text);
    font-weight: 600;
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

  .empty {
    border: 1px dashed var(--border-strong);
    border-radius: var(--radius);
    padding: var(--space-3);
    background: var(--surface-1);
  }
  .empty-title {
    margin: 0;
    color: var(--text);
    font-size: 0.95rem;
    font-weight: 600;
  }
  .empty-hint {
    margin: var(--space-1) 0 0;
    color: var(--text-dim);
    font-size: 0.85rem;
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
  tr.off .label-text,
  tr.off .scopes {
    opacity: 0.55;
  }
  tr.own {
    background: var(--accent-dim);
  }
  .label {
    font-family: var(--font-mono);
    white-space: nowrap;
  }
  .this-key {
    margin-left: var(--space-1);
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--accent);
    border: 1px solid var(--accent);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
  }
  .scopes {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-1);
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

  /* TK7b: the role <select> wears the same chip/badge skin as the static badge so
     the row reads the same; the role-* class tints it by current role. */
  .role-select {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    text-transform: uppercase;
    letter-spacing: var(--label-tracking);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
    background: var(--surface-2);
    color: var(--text-dim);
    cursor: pointer;
  }
  .role-select.role-owner {
    color: var(--accent);
    border-color: var(--accent);
  }
  .role-select.role-admin {
    color: var(--warn);
    border-color: var(--warn);
  }
  .role-select:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }

  /* TK7b: activation toggle sits beside the status badge (deactivate/activate). */
  .status-cell {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
  }
  .toggle-active {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    padding: 0 var(--space-1);
    background: transparent;
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text-dim);
    cursor: pointer;
    white-space: nowrap;
  }
  .toggle-active:hover:not(:disabled) {
    border-color: var(--accent);
    color: var(--accent);
  }
  .toggle-active:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  .badge.ok {
    color: var(--ok);
    border-color: var(--ok);
  }
  .badge.revoked {
    color: var(--text-faint);
    border-color: var(--border-strong);
  }
  .dim {
    color: var(--text-faint);
  }
  .when {
    color: var(--text-dim);
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }

  .toolbar {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    flex-wrap: wrap;
  }
  .cta {
    font-family: var(--font-ui);
    font-size: 0.82rem;
    padding: var(--space-1) var(--space-3);
    background: var(--surface-2);
    border: 1px solid var(--accent);
    border-radius: var(--radius);
    color: var(--accent);
    cursor: pointer;
  }
  .cta:hover:not(:disabled) {
    background: var(--accent-dim);
  }
  /* FE-10 / SS2: key-create CTA disabled at the tenant key cap. */
  .cta:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  /* FE-10 / SS2: key-limit hint banner under the toolbar. */
  .limit {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--warn);
    border-radius: var(--radius);
    color: var(--warn);
    font-size: 0.8rem;
  }
  .toggle {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    font-size: 0.82rem;
    color: var(--text-dim);
    cursor: pointer;
  }
  .toggle input:disabled {
    cursor: not-allowed;
  }
  .action-error {
    margin: 0;
    padding: var(--space-1) var(--space-2);
    border: 1px solid var(--danger);
    background: var(--danger-dim);
    border-radius: var(--radius);
    color: var(--danger);
    font-size: 0.8rem;
  }
  .col-action {
    text-align: right;
    white-space: nowrap;
  }
  .revoke {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    padding: 0 var(--space-2);
    background: transparent;
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text-dim);
    cursor: pointer;
  }
  .revoke.danger {
    border-color: var(--danger);
    color: var(--danger);
  }
  .revoke.danger:hover {
    background: var(--danger-dim);
  }
  .revoke:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  /* Mobile: the 8-column table (incl. the revoke action) overflows on a phone →
     it scrolls horizontally via .scroll (the cheap, layout-stable degradation).
     A per-key card-stack at @375px is the design §6 stretch; horizontal scroll
     keeps every column reachable in the meantime. */
</style>
