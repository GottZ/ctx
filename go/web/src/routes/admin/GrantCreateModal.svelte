<script lang="ts">
  // Create-a-read-grant modal (design 04 §7.1/§5.5, wave U11) — built on the
  // shared Modal shell (Q9), the TypeForm precedent. Grants THIS tenant (the
  // grantee) read access to a foreign scope via the pre-built tenants.ts client
  // (createTenantGrant). The deny-fault contract: a server 400/409 (reserved
  // '_'-scope, unknown grantee/scope, grant already exists — tenant_manage.go
  // :511) NEVER closes the modal and NEVER clears the input; the message shows in
  // a role=alert band and the draft is preserved verbatim. Only a 2xx closes +
  // triggers the parent re-read.
  import { onMount } from 'svelte'
  import { toApiError } from '../../lib/api'
  import { createTenantGrant } from '../../lib/api/tenants'
  import Modal from '../../lib/ui/Modal.svelte'

  let {
    tenantId,
    tenantSlug,
    onclose,
    onsaved,
  }: { tenantId: string; tenantSlug: string; onclose: () => void; onsaved: () => void } = $props()

  let scope = $state('')
  let busy = $state(false)
  let err = $state<string | null>(null)

  let dialogEl: HTMLDialogElement | undefined = $state()
  let firstInputEl = $state<HTMLInputElement>()
  onMount(() => firstInputEl?.focus())

  async function submit(): Promise<void> {
    if (busy) return
    const granted = scope.trim()
    if (granted === '') {
      err = 'a scope is required'
      return
    }
    busy = true
    err = null
    try {
      await createTenantGrant({ grantee_tenant: tenantId, granted_scope: granted })
      // Success is the ONLY path that closes + refreshes; anything else keeps the draft.
      onsaved()
      dialogEl?.close()
    } catch (e) {
      err = toApiError(e).message
    } finally {
      busy = false
    }
  }

  function close(): void {
    if (busy) return
    dialogEl?.close()
  }
</script>

<Modal
  bind:dialogEl
  width="34rem"
  dismissable={!busy}
  backdropClose={!busy}
  ariaLabelledby="grant-form-title"
  {onclose}
>
  <form
    method="dialog"
    onsubmit={(e) => {
      e.preventDefault()
      void submit()
    }}
  >
    <header>
      <h2 id="grant-form-title">Grant scope access</h2>
      <button type="button" class="x" title="close" disabled={busy} onclick={close}>×</button>
    </header>

    <div class="body">
      <p class="note" role="note">
        Give <code>{tenantSlug}</code> read access to a scope owned by another tenant. Enter the full scope name
        (e.g. <code>other:main</code>). System scopes (<code>_</code>-prefixed) are never grantable.
      </p>

      <label class="field">
        <span class="lbl">granted scope</span>
        <input
          bind:this={firstInputEl}
          bind:value={scope}
          type="text"
          spellcheck="false"
          autocapitalize="off"
          autocomplete="off"
          placeholder="e.g. other:main"
          disabled={busy}
          aria-label="granted scope"
        />
      </label>

      {#if err}
        <p class="problem" role="alert">{err}</p>
      {/if}
    </div>

    <footer>
      <button type="button" class="ghost" disabled={busy} onclick={close}>Cancel</button>
      <button type="submit" class="primary" disabled={busy}>{busy ? 'Granting…' : 'Grant access'}</button>
    </footer>
  </form>
</Modal>

<style>
  form {
    display: flex;
    flex-direction: column;
  }
  header {
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
  .x {
    margin-left: auto;
    background: transparent;
    border: none;
    color: var(--text-dim);
    font-size: var(--fs-xl);
    line-height: var(--lh-solid);
    cursor: pointer;
  }
  .x:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }
  .body {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    padding: var(--space-3);
  }
  .note {
    margin: 0;
    color: var(--text-dim);
    font-size: var(--fs-sm);
    line-height: var(--lh-body);
  }
  .note code {
    font-family: var(--font-mono);
    color: var(--accent);
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .lbl {
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
  }
  input {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-2);
    background: var(--surface-2);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
  }
  input:disabled {
    opacity: 0.6;
  }
  .problem {
    margin: 0;
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius);
    border: 1px solid var(--danger);
    color: var(--danger);
    background: var(--danger-dim);
  }
  footer {
    display: flex;
    justify-content: flex-end;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border-top: 1px solid var(--border);
  }
  footer button {
    font-family: var(--font-ui);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-3);
    border-radius: var(--radius);
    cursor: pointer;
  }
  .ghost {
    background: var(--surface-2);
    border: 1px solid var(--border-strong);
    color: var(--text);
  }
  .primary {
    background: var(--surface-2);
    border: 1px solid var(--accent);
    color: var(--accent);
  }
  footer button:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
</style>
