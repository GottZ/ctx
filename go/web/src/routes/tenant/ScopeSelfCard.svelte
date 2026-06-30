<script lang="ts">
  // FE-9 / FE-SS1 — tenant-admin self-service scope provisioning (design
  // 05-frontend-a3-selfservice §4 SS1). The tenant-admin names a BARE scope; the
  // server namespaces it to `<tenant_slug>:<name>` from ar.TenantID (S1 — prefix
  // injection is structurally impossible client-side) and echoes the FULL scope,
  // which we render read-only. The a-z0-9- client check is fast feedback ONLY —
  // a name carrying ':' (or any other charset violation) is blocked before the
  // doomed round-trip; the server stays authoritative (a 400 charset / 429 over-
  // limit / 403 lands on model.actionError). At the tenant's scope cap the create
  // is disabled + a banner shows — visibility only, the server caps hard (429).
  //
  // Consumes the already-built ScopesSelfModel (FE-M3) — never re-implements the
  // flow; the model owns load (scope-list + tenant-usage-get) + create (bare name
  // → reload). TenantPage instantiates the single model and drives its load.
  import { session } from '../../lib/auth.svelte'
  import type { ScopesSelfModel } from './scopes.svelte'

  let { model }: { model: ScopesSelfModel } = $props()

  let scopeName = $state('')
  // Client-side charset reject (no request sent); distinct from model.actionError
  // (the server's 400/429/403). Cleared at the start of every submit.
  let clientError = $state<string | null>(null)
  // The FULL server-built scope from the last successful create — render read-only
  // (S1: we show the server's value, never a client-built prefix).
  let createdScope = $state<string | null>(null)

  const NAME_RE = /^[a-z0-9-]+$/

  // Cosmetic live preview of the server-built name; the authoritative value always
  // returns in ScopeCreateResult.scope. A null slug (loading) degrades to 'tenant'.
  const preview = $derived(`${session.tenantSlug ?? 'tenant'}:${scopeName.trim()}`)
  // A non-empty name that violates the charset can never be valid server-side
  // either → flag it live (tints the preview) before the user even submits.
  const nameInvalid = $derived(scopeName.trim() !== '' && !NAME_RE.test(scopeName.trim()))

  async function submit(): Promise<void> {
    if (model.busy || model.atScopeLimit) return
    const name = scopeName.trim()
    clientError = null
    if (name === '') {
      clientError = 'scope name is required'
      return
    }
    if (!NAME_RE.test(name)) {
      // Client-block, NO request (the server would 400 it anyway, S1): lowercase
      // letters, digits and hyphens only — a ':' would address a foreign namespace.
      clientError = 'lowercase letters, digits and hyphens only — no ":"'
      return
    }
    try {
      const res = await model.create(name)
      createdScope = res.scope
      scopeName = ''
    } catch {
      /* model.actionError carries the server 400/429/403 — rendered below. */
    }
  }
</script>

<section class="card" aria-label="self-service scopes">
  <header class="card-head">
    <h2>your scopes</h2>
    {#if model.status === 'ready'}<span class="count">{model.scopes.length}</span>{/if}
  </header>

  <div class="card-body">
    {#if model.status === 'loading' || model.status === 'idle'}
      <p class="state" aria-busy="true">loading scopes…</p>
    {:else if model.status === 'error'}
      <div class="error" role="alert">
        <p>{model.loadError?.message ?? 'failed to load scopes'}</p>
        {#if model.loadError?.requestId}<p class="request-id">request {model.loadError.requestId}</p>{/if}
        <button type="button" onclick={() => void model.reload()}>Retry</button>
      </div>
    {:else}
      {#if model.scopes.length === 0}
        <p class="empty" role="status">No scopes yet — create your first below.</p>
      {:else}
        <ul class="scope-list" aria-label="tenant scopes">
          {#each model.scopes as s (s.scope)}
            <li class="chip">{s.scope}</li>
          {/each}
        </ul>
      {/if}

      {#if model.atScopeLimit}
        <p class="limit" role="status">
          scope limit reached ({model.usage?.scope_count}/{model.usage?.max_scopes}) — revoke one or ask a
          server-admin to raise the cap.
        </p>
      {/if}

      <div class="create">
        <div class="row">
          <input
            class="grow"
            type="text"
            spellcheck="false"
            aria-label="new scope name"
            placeholder="bare name, e.g. research"
            disabled={model.busy || model.atScopeLimit}
            bind:value={scopeName}
            onkeydown={(e) => {
              if (e.key === 'Enter') {
                e.preventDefault()
                void submit()
              }
            }}
          />
          <button type="button" disabled={model.busy || model.atScopeLimit} onclick={() => void submit()}>
            {model.busy ? 'creating…' : 'Create scope'}
          </button>
        </div>

        {#if scopeName.trim() !== ''}
          <span class="preview" class:bad={nameInvalid} aria-label="scope preview">{preview}</span>
        {:else}
          <span class="hint">The server prepends your tenant namespace — you send only the bare name.</span>
        {/if}

        {#if createdScope}
          <label class="created" role="status">
            <span class="lbl">created scope</span>
            <input type="text" readonly aria-label="created scope" value={createdScope} />
          </label>
        {/if}

        {#if clientError}
          <p class="problem" role="alert">{clientError}</p>
        {:else if model.actionError}
          <p class="problem" role="alert">{model.actionError}</p>
        {/if}
      </div>
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
    color: var(--text-dim);
  }
  .card-body {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    padding: var(--space-3);
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
  .error button {
    font-family: var(--font-ui);
    font-size: 0.8rem;
    padding: var(--space-1) var(--space-3);
    background: var(--surface-2);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
    cursor: pointer;
  }
  .empty {
    margin: 0;
    color: var(--text-dim);
    font-size: 0.85rem;
  }
  .scope-list {
    margin: 0;
    padding: 0;
    list-style: none;
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
  .limit {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--warn);
    border-radius: var(--radius);
    color: var(--warn);
    font-size: 0.8rem;
  }
  .create {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
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
  .preview {
    font-family: var(--font-mono);
    font-size: 0.78rem;
    color: var(--text-dim);
  }
  .preview.bad {
    color: var(--danger);
  }
  .hint {
    font-size: 0.72rem;
    color: var(--text-faint);
  }
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
