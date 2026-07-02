<script lang="ts">
  // Mint-a-key dialog (design 05-tenant-admin §3, Wave TK4) — native <dialog>+
  // showModal (no UI lib, matching BackendDialog). Two views in ONE modal: the
  // create form, then — on success — the show-once RevealOnceKey. The plaintext
  // returned by keys.create() lives only in this dialog's local `created` $state
  // and in RevealOnceKey; ack/dismiss drops both (design §6 hygiene). It is NEVER
  // put on KeysModel/storage/URL/console.
  //
  // Scope mint is server-authoritative: firstScopeOutsideTenant 403s a scope the
  // tenant doesn't own. The client NEVER gates the write on its own scope guess —
  // it offers session.readScopes as a convenience picker plus a free-text field
  // for a freshly-owned scope without a key yet, sends regardless, and renders
  // any 403 cleanly (design §6 "Scope-Leak" / "Client-Mirror = Vorschlag").
  import { onMount } from 'svelte'
  import { toApiError } from '../../lib/api'
  import { session } from '../../lib/auth.svelte'
  import type { ApiKeyCreateResult } from '../../lib/api/types'
  import type { ApiKeyCreateSpec } from '../../lib/api/keys'
  import type { KeysModel } from './keys.svelte'
  import RevealOnceKey from './RevealOnceKey.svelte'

  let { keys, onclose }: { keys: KeysModel; onclose: () => void } = $props()

  let label = $state('')
  // home_scope is REQUIRED by the server (empty → 400); default to the caller's
  // own home scope, editable for a tenant-admin minting into another owned scope.
  let homeScope = $state(session.homeScope ?? '')
  // allowed_scopes: a checkbox set off the caller's read_scopes (the convenience
  // mirror) unioned with a free-text comma field (the escape hatch for a scope
  // not yet represented by any key, design §6).
  let picked = $state<Set<string>>(new Set())
  let extraScopes = $state('')

  let busy = $state(false)
  let error = $state<string | null>(null)
  // The show-once result; non-null switches the dialog to the reveal view. Holds
  // the plaintext transiently — wiped on ack (finish()).
  let created = $state<ApiKeyCreateResult | null>(null)

  let dialogEl: HTMLDialogElement
  let labelInputEl = $state<HTMLInputElement>()
  onMount(() => {
    dialogEl.showModal()
    labelInputEl?.focus()
  })

  const readScopes = $derived(session.readScopes)

  // Server-admin-only "target tenant" override (S2, design 05 FE-D2). A
  // server-admin may mint a RECOVERY key INTO a foreign tenant; capabilitiesFor
  // → viewTenants is the single source of truth (server-admin only). A
  // tenant-admin never sees this field, so a cross-tenant tenant_id is
  // structurally inexpressible from the tenant-admin UI — the unchanged TK4 path
  // NEVER sends tenant_id. The server stays authoritative either way: a non-admin
  // caller's value is ignored/bound to ar.TenantID (04-security-model SEC-6).
  const canTargetTenant = $derived(session.caps.viewTenants)
  let targetTenant = $state('')

  function toggleScope(scope: string): void {
    // Reassign the Set so the $state proxy notices the mutation.
    const next = new Set(picked)
    if (next.has(scope)) next.delete(scope)
    else next.add(scope)
    picked = next
  }

  /** Union of the ticked read-scopes and the parsed free-text, deduped; an empty
   *  result is omitted so the server applies its tenant-aware {shared} default. */
  function collectAllowed(): string[] | undefined {
    const out = new Set<string>(picked)
    for (const s of extraScopes.split(',')) {
      const t = s.trim()
      if (t !== '') out.add(t)
    }
    return out.size > 0 ? [...out] : undefined
  }

  async function submit(): Promise<void> {
    if (busy) return
    const lbl = label.trim()
    if (lbl === '') {
      error = 'label is required'
      return
    }
    const hs = homeScope.trim()
    if (hs === '') {
      error = 'home scope is required'
      return
    }
    busy = true
    error = null
    try {
      const spec: ApiKeyCreateSpec = { label: lbl, home_scope: hs, allowed_scopes: collectAllowed() }
      // tenant_id ONLY when a server-admin (canTargetTenant) typed a target. A
      // tenant-admin can't reach this branch (field not in DOM), so its mint
      // stays byte-identical to TK4 — no tenant_id key on the wire.
      if (canTargetTenant) {
        const tid = targetTenant.trim()
        if (tid !== '') spec.tenant_id = tid
      }
      // create() mints, reloads the live list, and returns the show-once result.
      created = await keys.create(spec)
    } catch (err) {
      // Server-403 (firstScopeOutsideTenant), 400 (reserved/empty scope), etc.
      error = toApiError(err).message
    } finally {
      busy = false
    }
  }

  /** Reveal acknowledged: drop the plaintext from THIS dialog's state too, then
   *  close (the list already refreshed on create success). */
  function finish(): void {
    created = null
    dialogEl.close()
  }

  function close(): void {
    if (busy) return
    dialogEl.close()
  }
</script>

<dialog
  bind:this={dialogEl}
  class="key-dialog"
  aria-labelledby="key-create-title"
  onclose={onclose}
  oncancel={(e) => {
    if (busy) e.preventDefault()
  }}
  onclick={(e) => {
    if (e.target === dialogEl && !busy && created === null) dialogEl.close()
  }}
>
  <form
    method="dialog"
    onsubmit={(e) => {
      e.preventDefault()
      if (created === null) void submit()
    }}
  >
    <header>
      <h2 id="key-create-title">{created === null ? 'New API key' : 'Key created'}</h2>
      <button type="button" class="x" title="close" disabled={busy} onclick={close}>×</button>
    </header>

    <div class="body">
      {#if created !== null}
        <RevealOnceKey apiKey={created.api_key} onack={finish} />
      {:else}
        {#if canTargetTenant}
          <label class="field">
            <span class="lbl">target tenant</span>
            <input
              type="text"
              spellcheck="false"
              placeholder="tenant id (uuid) — empty = your own tenant"
              disabled={busy}
              bind:value={targetTenant}
            />
            <span class="hint">
              server-admin only: mint this key INTO another tenant (recovery). Empty → your own tenant; the server binds
              any non-admin caller to its own tenant regardless.
            </span>
          </label>
        {/if}

        <label class="field">
          <span class="lbl">label</span>
          <input
            bind:this={labelInputEl}
            type="text"
            spellcheck="false"
            placeholder="who/what this key is for"
            disabled={busy}
            bind:value={label}
          />
        </label>

        <label class="field">
          <span class="lbl">home scope</span>
          <input type="text" spellcheck="false" placeholder="e.g. {session.homeScope ?? 'team-scope'}" disabled={busy} bind:value={homeScope} />
          <span class="hint">the key's own write scope (required) — must be a scope your tenant owns.</span>
        </label>

        <div class="field">
          <span class="lbl">allowed scopes</span>
          {#if readScopes.length > 0}
            <div class="scope-grid">
              {#each readScopes as s (s)}
                <label class="check">
                  <input type="checkbox" disabled={busy} checked={picked.has(s)} onchange={() => toggleScope(s)} />
                  <span class="chip">{s}</span>
                </label>
              {/each}
            </div>
          {/if}
          <input
            type="text"
            spellcheck="false"
            placeholder="extra scopes, comma-separated (optional)"
            disabled={busy}
            bind:value={extraScopes}
          />
          <span class="hint">read scopes the key may also see. Empty → the server's tenant-default. The server rejects any scope your tenant doesn't own.</span>
        </div>

        {#if error}
          <p class="problem" role="alert">{error}</p>
        {/if}
      {/if}
    </div>

    {#if created === null}
      <footer>
        <div class="actions">
          <button type="button" class="ghost" disabled={busy} onclick={close}>Cancel</button>
          <button type="submit" disabled={busy}>{busy ? 'creating…' : 'Create key'}</button>
        </div>
      </footer>
    {/if}
  </form>
</dialog>

<style>
  .key-dialog {
    width: min(34rem, calc(100vw - 2rem));
    max-height: calc(100dvh - 4rem);
    padding: 0;
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    background: var(--surface-1);
    color: var(--text);
  }
  .key-dialog::backdrop {
    background: var(--backdrop);
  }
  form {
    display: flex;
    flex-direction: column;
    max-height: calc(100dvh - 4rem);
  }
  header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: var(--space-2) var(--space-3);
    border-bottom: 1px solid var(--border);
  }
  h2 {
    margin: 0;
    font-family: var(--font-mono);
    font-size: 1rem;
    font-weight: 600;
  }
  .x {
    background: transparent;
    border: none;
    color: var(--text-dim);
    font-size: 1.3rem;
    line-height: 1;
    cursor: pointer;
    padding: 0 var(--space-1);
    font-family: inherit;
  }
  .x:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  .body {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    padding: var(--space-3);
    overflow-y: auto;
  }
  .field {
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
  .field input[type='text'] {
    font-family: var(--font-mono);
    font-size: 0.85rem;
    padding: var(--space-1) var(--space-2);
    background: var(--surface-0);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
  }
  .field input:disabled {
    opacity: 0.55;
    cursor: not-allowed;
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
  .problem {
    margin: 0;
    font-size: 0.78rem;
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius);
    border: 1px solid var(--danger);
    color: var(--danger);
    background: var(--danger-dim);
  }
  footer {
    border-top: 1px solid var(--border);
    padding: var(--space-2) var(--space-3);
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: var(--space-2);
  }
  button.ghost {
    background: transparent;
  }
  footer button {
    font-family: var(--font-ui);
    font-size: 0.82rem;
    padding: var(--space-1) var(--space-3);
    background: var(--surface-2);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
    cursor: pointer;
  }
  footer button:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
</style>
