<script lang="ts">
  // Server-admin provisioning for ONE tenant (design 05 §4 A3c, Wave FE-6) —
  // mounted on TenantDetail (/admin/tenants/:id, server-admin only). Two write
  // surfaces, both server-authoritative:
  //
  //  1. Provision a scope — the client sends ONLY the bare `name` plus this
  //     tenant's `tenant_id`; the server builds the full `<slug>:<name>` from the
  //     TARGET tenant (S1 — prefix injection is structurally impossible
  //     client-side) and echoes it back. We render the server-returned scope
  //     read-only; the client NEVER builds the prefix.
  //  2. Mint a recovery key INTO this tenant — createApiKey with `tenant_id`
  //     pre-bound to the page's tenant (S2; the server-admin override). The
  //     plaintext rides through RevealOnceKey (show-once hygiene) and is never
  //     put on any model/storage/URL.
  //
  // Authorization stays server-side: a cross-tenant / unowned-scope write answers
  // 403, an over-quota write 429, a bad name 400 — every failure surfaces as the
  // inline banner (the FE renders, it does not enforce).
  import { createScope } from '../../lib/api/scopes'
  import { createApiKey } from '../../lib/api/keys'
  import { toApiError } from '../../lib/api'
  import type { ApiKeyCreateResult, ScopeOverview } from '../../lib/api/types'
  import RevealOnceKey from '../tenant/RevealOnceKey.svelte'

  let {
    tenantId,
    scopes,
    onprovisioned,
  }: {
    tenantId: string
    /** This tenant's own scopes (scope-overview filtered) — the mint scope picker. */
    scopes: ScopeOverview[]
    /** Re-load the parent's scope list after a successful scope-create (no full
     *  page reload — keeps this card + its reveal mounted). */
    onprovisioned: () => void | Promise<void>
  } = $props()

  // --- Scope provision ---
  let scopeName = $state('')
  let scopeBusy = $state(false)
  let scopeError = $state<string | null>(null)
  // The FULL server-built scope from the last create — render read-only (S1).
  let createdScope = $state<string | null>(null)

  async function provisionScope(): Promise<void> {
    if (scopeBusy) return
    const name = scopeName.trim()
    if (name === '') {
      scopeError = 'scope name is required'
      return
    }
    scopeBusy = true
    scopeError = null
    try {
      // Bare name + target tenant_id; the server prepends <slug>: and returns the
      // full name — we show that, never a client-built prefix.
      const res = await createScope({ name, tenant_id: tenantId })
      createdScope = res.scope
      scopeName = ''
      await onprovisioned()
    } catch (err) {
      // 400 (name charset / prefix injection), 403 (cross-tenant / unowned), 429
      // (over the tenant's max_scopes) — all rendered, never gated client-side.
      scopeError = toApiError(err).message
    } finally {
      scopeBusy = false
    }
  }

  // --- Recovery key mint (server-admin → this tenant, S2) ---
  let minting = $state(false)
  let keyLabel = $state('')
  // Default the home scope to one this tenant owns (the mint must use a scope of
  // the TARGET tenant). Snapshotting the prop once is intentional — `scopes` is
  // settled before this card mounts (TenantDetail renders it post-load).
  // svelte-ignore state_referenced_locally
  let keyHome = $state(scopes[0]?.scope ?? '')
  let keyPicked = $state<Set<string>>(new Set())
  let keyBusy = $state(false)
  let keyError = $state<string | null>(null)
  // Show-once plaintext; non-null switches the card to the reveal view.
  let minted = $state<ApiKeyCreateResult | null>(null)

  function toggleScope(scope: string): void {
    const next = new Set(keyPicked)
    if (next.has(scope)) next.delete(scope)
    else next.add(scope)
    keyPicked = next
  }

  async function mintKey(): Promise<void> {
    if (keyBusy) return
    const lbl = keyLabel.trim()
    if (lbl === '') {
      keyError = 'label is required'
      return
    }
    const hs = keyHome.trim()
    if (hs === '') {
      keyError = 'home scope is required'
      return
    }
    keyBusy = true
    keyError = null
    try {
      const allowed = keyPicked.size > 0 ? [...keyPicked] : undefined
      // tenant_id pre-bound to THIS tenant (S2 server-admin override); the server
      // is authoritative — a non-admin caller's tenant_id would be ignored/403'd.
      minted = await createApiKey({ label: lbl, home_scope: hs, allowed_scopes: allowed, tenant_id: tenantId })
    } catch (err) {
      // 403 firstScopeOutsideTenant (scope not owned by the target), 400, etc.
      keyError = toApiError(err).message
    } finally {
      keyBusy = false
    }
  }

  /** Reveal acknowledged — drop the plaintext from THIS card's state and reset the
   *  form back to the collapsed CTA. */
  function ackMint(): void {
    minted = null
    minting = false
    keyLabel = ''
    keyPicked = new Set()
  }

  function cancelMint(): void {
    if (keyBusy) return
    minting = false
    keyError = null
  }
</script>

<section class="card" aria-label="provision scope and key">
  <header class="card-head">
    <h2>provision</h2>
    <span class="count">server-admin</span>
  </header>
  <p class="note">
    Activate this tenant: provision a scope (the server namespaces it to this tenant) and mint a recovery owner
    key. Both writes are server-authoritative — a cross-tenant or unowned target answers 403.
  </p>

  <div class="block">
    <h3>add scope</h3>
    <div class="row">
      <input
        class="grow"
        type="text"
        spellcheck="false"
        aria-label="new scope name"
        placeholder="bare name, e.g. research"
        disabled={scopeBusy}
        bind:value={scopeName}
        onkeydown={(e) => {
          if (e.key === 'Enter') {
            e.preventDefault()
            void provisionScope()
          }
        }}
      />
      <button type="button" disabled={scopeBusy} onclick={() => void provisionScope()}>
        {scopeBusy ? 'provisioning…' : 'Provision scope'}
      </button>
    </div>
    <span class="hint">
      The server prepends this tenant's namespace and returns the full scope — the client never builds the prefix.
    </span>

    {#if createdScope}
      <label class="created" role="status">
        <span class="lbl">provisioned scope</span>
        <input type="text" readonly aria-label="provisioned scope" value={createdScope} />
      </label>
    {/if}
    {#if scopeError}
      <p class="problem" role="alert">{scopeError}</p>
    {/if}
  </div>

  <div class="block">
    <h3>mint recovery key</h3>
    {#if minted !== null}
      <RevealOnceKey apiKey={minted.api_key} onack={ackMint} />
    {:else if minting}
      <label class="field">
        <span class="lbl">key label</span>
        <input
          type="text"
          spellcheck="false"
          aria-label="key label"
          placeholder="who/what this key is for"
          disabled={keyBusy}
          bind:value={keyLabel}
        />
      </label>
      <label class="field">
        <span class="lbl">home scope</span>
        <input
          type="text"
          spellcheck="false"
          aria-label="home scope"
          placeholder="a scope this tenant owns"
          disabled={keyBusy}
          bind:value={keyHome}
        />
      </label>
      {#if scopes.length > 0}
        <div class="field">
          <span class="lbl">allowed scopes</span>
          <div class="scope-grid">
            {#each scopes as s (s.scope)}
              <label class="check">
                <input
                  type="checkbox"
                  disabled={keyBusy}
                  checked={keyPicked.has(s.scope)}
                  onchange={() => toggleScope(s.scope)}
                />
                <span class="chip">{s.scope}</span>
              </label>
            {/each}
          </div>
        </div>
      {/if}
      {#if keyError}
        <p class="problem" role="alert">{keyError}</p>
      {/if}
      <div class="actions">
        <button type="button" class="ghost" disabled={keyBusy} onclick={cancelMint}>Cancel</button>
        <button type="button" disabled={keyBusy} onclick={() => void mintKey()}>
          {keyBusy ? 'minting…' : 'Mint key'}
        </button>
      </div>
    {:else}
      <button type="button" class="cta" onclick={() => (minting = true)}>+ Mint key for this tenant</button>
      <span class="hint">
        Recovery path (design §4 A3c): mints a key bound to THIS tenant (tenant_id) and reveals the plaintext once.
      </span>
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
    font-size: 0.95rem;
    font-weight: 600;
  }
  .count {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-dim);
  }
  .note {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    color: var(--text-dim);
    font-size: 0.8rem;
    border-bottom: 1px solid var(--border);
  }
  .block {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-3);
    border-bottom: 1px solid var(--border);
  }
  .block:last-child {
    border-bottom: none;
  }
  h3 {
    margin: 0;
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-faint);
  }
  .row {
    display: flex;
    gap: var(--space-2);
    align-items: stretch;
  }
  .grow {
    flex: 1;
    min-width: 0;
  }
  input[type='text'] {
    font-family: var(--font-mono);
    font-size: 0.85rem;
    padding: var(--space-1) var(--space-2);
    background: var(--surface-0);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
  }
  input:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  input[readonly] {
    border-color: var(--accent);
    color: var(--text);
  }
  .field,
  .created {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .lbl {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-faint);
  }
  .hint {
    font-size: 0.72rem;
    color: var(--text-faint);
  }
  .scope-grid {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
  }
  .check {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    cursor: pointer;
  }
  .check input:disabled {
    cursor: not-allowed;
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
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: var(--space-2);
  }
  button {
    font-family: var(--font-ui);
    font-size: 0.82rem;
    padding: var(--space-1) var(--space-3);
    background: var(--surface-2);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
    cursor: pointer;
    white-space: nowrap;
  }
  button:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  button.ghost {
    background: transparent;
  }
  button.cta {
    align-self: flex-start;
  }
  .problem {
    margin: 0;
    font-size: 0.78rem;
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius);
    border: 1px solid var(--danger);
    color: var(--danger);
    background: var(--danger-dim);
  }
</style>
