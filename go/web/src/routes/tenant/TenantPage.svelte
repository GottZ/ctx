<script lang="ts">
  // Tenant-admin area shell (design 05-tenant-admin §1/§3, Wave TK3) — the
  // READ-ONLY surface over the tenant's API keys (the keys ARE the member-
  // proxys). This wave only LISTS; key creation (TK4), revoke + show-revoked +
  // self-revoke guard (TK5) and the quota form (TK6) land later and fill their
  // own files — they do NOT edit this shell. So the model's mutation set
  // (create/remove/toggleRevoked) is deliberately left UNWIRED here; we read
  // only keys/status/loadError.
  //
  // Self-gate: manageTenantKeys (tenant-admin+ or server-admin, capabilitiesFor
  // §3) mirrors the server's requireTenantAdmin — a member key gets a banner,
  // not a doomed 403 request. The gate is UX only; the server is authoritative.
  import { session } from '../../lib/auth.svelte'
  import { KeysModel } from './keys.svelte'

  // KeysModel carries its own $state runes, so a plain const instance is
  // reactive in the template (§ Svelte-5 class-runes). Load is driven by an
  // $effect (not onMount) so a boot-time restore race — caps='loading' at mount
  // (§6/R6) — still triggers the fetch the moment access resolves.
  const keys = new KeysModel()

  $effect(() => {
    if (session.caps.manageTenantKeys && keys.status === 'idle') {
      void keys.load()
    }
  })

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
      tenant's member proxys. Read-only here: creation, revoke and quota land in later waves.
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
        <p class="empty-title">No active keys yet</p>
        <p class="empty-hint">Key creation arrives in a later wave — this view lists the tenant's keys read-only.</p>
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
                      <span class="badge {roleClass(k.tenant_role)}">{k.tenant_role}</span>
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
                  <td>
                    {#if k.active}
                      <span class="badge ok">active</span>
                    {:else}
                      <span class="badge revoked">revoked</span>
                    {/if}
                  </td>
                  <td class="when">{fmtUsed(k.last_used_at)}</td>
                  <td class="when">{k.created_at.slice(0, 10)}</td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      </section>
    {/if}

    <!-- Read-only stubs for the surfaces that later waves fill (no form, no
         mutation): key creation + members (TK4), quota transparency (TK6). They
         keep the area coherent without pretending the capability exists yet. -->
    <div class="stubs">
      <div class="stub">
        <span class="stub-tag">soon</span>
        <p class="stub-title">Members &amp; key creation</p>
        <p class="stub-body">Minting new tenant keys and managing members arrives in a later wave.</p>
      </div>
      <div class="stub">
        <span class="stub-tag">soon</span>
        <p class="stub-title">Quota</p>
        <p class="stub-body">Read-only quota transparency for the tenant's scopes arrives in a later wave.</p>
      </div>
    </div>
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
  .badge.role-owner {
    color: var(--accent);
    border-color: var(--accent);
  }
  .badge.role-admin {
    color: var(--warn);
    border-color: var(--warn);
  }
  .badge.role-member {
    color: var(--text-dim);
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

  .stubs {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-3);
  }
  .stub {
    flex: 1 1 16rem;
    border: 1px dashed var(--border-strong);
    border-radius: var(--radius);
    padding: var(--space-3);
    background: var(--surface-1);
  }
  .stub-tag {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-faint);
    border: 1px dashed var(--border-strong);
    border-radius: var(--radius);
    padding: 0.05rem var(--space-2);
  }
  .stub-title {
    margin: var(--space-2) 0 0;
    color: var(--text);
    font-weight: 600;
    font-size: 0.9rem;
  }
  .stub-body {
    margin: var(--space-1) 0 0;
    color: var(--text-dim);
    font-size: 0.82rem;
  }
  /* Mobile: the 7-column table overflows on a phone → it scrolls horizontally
     via .scroll (the cheap, layout-stable degradation). The stub cards stack
     on their own via flex-wrap (flex-basis 16rem). The per-key card-stack of
     the table itself is TK5's concern (when the action column lands). */
</style>
