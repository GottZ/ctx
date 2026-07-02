<script lang="ts" module>
  // Tenant-create dialog (design 05-frontend-a3-selfservice §4 A3a, Wave FE-A3a) —
  // native <dialog>+showModal, no UI lib, mirroring KeyCreateDialog. Two views in
  // ONE modal: the create form, then — on success — the show-once RevealOnceKey
  // for the compound owner-key. The compound tenant-create is atomic (K10): tenant
  // row + initial auto-prefixed scope + minted owner-key. The owner-key PLAINTEXT
  // (TenantCreateResult.owner_key, a string) lives only in this dialog's local
  // `created` $state and in RevealOnceKey; ack/dismiss drops both. It is NEVER put
  // on TenantsModel/storage/URL/console (TenantsModel.create never retains it).
  //
  // Pure helpers below live in the module script so the node-only vitest can cover
  // them without a DOM (design 04 §5.5), like ConfirmDialog's slug-match logic.

  /** Tenant-slug charset gate — mirrors the server's slugPattern
   *  (tenant_manage.go:46): 1..24 chars, ASCII-lowercase alnum + INTERNAL hyphen
   *  (no leading/trailing hyphen). Client-side it is only quick feedback; the
   *  server is authoritative and answers 400 on a breach. */
  export const SLUG_RE = /^[a-z0-9](?:[a-z0-9-]{0,22}[a-z0-9])?$/
  export function validSlug(slug: string): boolean {
    return SLUG_RE.test(slug)
  }

  /** Parse an optional positive-integer cap field. '' → undefined (omit the field
   *  → the server applies its own default, BEQ-1b also 25/50). A non-integer or
   *  < 1 value → 'invalid' (the form blocks submit and shows a hint). */
  export function parseLimit(raw: string): number | undefined | 'invalid' {
    const t = raw.trim()
    if (t === '') return undefined
    const n = Number(t)
    if (!Number.isInteger(n) || n < 1) return 'invalid'
    return n
  }
</script>

<script lang="ts">
  import { onMount } from 'svelte'
  import { toApiError } from '../../lib/api'
  import type { TenantCreateResult } from '../../lib/api/types'
  import type { TenantsModel } from './tenants.svelte'
  import RevealOnceKey from '../tenant/RevealOnceKey.svelte'

  let { tenants, onclose }: { tenants: TenantsModel; onclose: () => void } = $props()

  let slug = $state('')
  let displayName = $state('')
  // Default caps PREFILLED visibly (BEQ-1b): 25 scopes / 50 keys so an operator
  // never accidentally provisions an uncapped tenant. Clearing a field omits it →
  // the server applies its own default.
  let maxScopes = $state('25')
  let maxKeys = $state('50')

  let busy = $state(false)
  let error = $state<string | null>(null)
  // The reveal-once compound result; non-null switches the dialog to the reveal
  // view. Holds the owner-key plaintext transiently — wiped on ack (finish()).
  let created = $state<TenantCreateResult | null>(null)

  let dialogEl: HTMLDialogElement
  let slugInputEl = $state<HTMLInputElement>()
  onMount(() => {
    dialogEl.showModal()
    slugInputEl?.focus()
  })

  async function submit(): Promise<void> {
    if (busy) return
    const s = slug.trim()
    if (s === '') {
      error = 'slug is required'
      return
    }
    if (!validSlug(s)) {
      error = 'slug must be 1–24 chars: lowercase a–z, 0–9, internal hyphen (no leading/trailing -)'
      return
    }
    const dn = displayName.trim()
    if (dn === '') {
      error = 'display name is required'
      return
    }
    const ms = parseLimit(maxScopes)
    if (ms === 'invalid') {
      error = 'max scopes must be a positive whole number (or empty for the server default)'
      return
    }
    const mk = parseLimit(maxKeys)
    if (mk === 'invalid') {
      error = 'max keys must be a positive whole number (or empty for the server default)'
      return
    }
    busy = true
    error = null
    try {
      // create() provisions atomically, reloads the live register, and returns
      // the reveal-once compound result (owner-key plaintext for the reveal view).
      created = await tenants.create({ slug: s, display_name: dn, max_scopes: ms, max_keys: mk })
    } catch (err) {
      // Server-409 (slug exists), 400 (reserved/charset/empty), etc.
      error = toApiError(err).message
    } finally {
      busy = false
    }
  }

  /** Reveal acknowledged: drop the plaintext from THIS dialog's state too, then
   *  close (the register already refreshed on create success). */
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
  class="tenant-dialog"
  aria-labelledby="tenant-create-title"
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
      <h2 id="tenant-create-title">{created === null ? 'New tenant' : 'Tenant created'}</h2>
      <button type="button" class="x" title="close" disabled={busy} onclick={close}>×</button>
    </header>

    <div class="body">
      {#if created !== null}
        <p class="provisioned" role="status">
          Tenant <strong>{created.tenant.slug}</strong> is live with its initial scope
          <code>{created.scope}</code> and an owner key. Hand the key below to the tenant owner
          out-of-band — it is the only credential to administer the tenant.
        </p>
        <RevealOnceKey apiKey={created.owner_key} onack={finish} />
      {:else}
        <label class="field">
          <span class="lbl">slug</span>
          <input
            bind:this={slugInputEl}
            type="text"
            spellcheck="false"
            autocapitalize="off"
            autocomplete="off"
            placeholder="e.g. acme-research"
            disabled={busy}
            bind:value={slug}
          />
          <span class="hint">
            URL-safe id: lowercase a–z, 0–9, internal hyphen, ≤24 chars. Becomes the
            <code>{(slug.trim() || 'slug')}:…</code> scope prefix — cannot be changed later.
          </span>
        </label>

        <label class="field">
          <span class="lbl">display name</span>
          <input
            type="text"
            spellcheck="false"
            placeholder="e.g. Acme Research"
            disabled={busy}
            bind:value={displayName}
          />
        </label>

        <div class="caps">
          <label class="field">
            <span class="lbl">max scopes</span>
            <input type="number" min="1" step="1" inputmode="numeric" disabled={busy} bind:value={maxScopes} />
          </label>
          <label class="field">
            <span class="lbl">max keys</span>
            <input type="number" min="1" step="1" inputmode="numeric" disabled={busy} bind:value={maxKeys} />
          </label>
        </div>
        <span class="hint">
          Hard tenant caps (the server enforces them with 429). Prefilled with the
          defaults — clear a field to fall back to the server default.
        </span>

        {#if error}
          <p class="problem" role="alert">{error}</p>
        {/if}
      {/if}
    </div>

    {#if created === null}
      <footer>
        <div class="actions">
          <button type="button" class="ghost" disabled={busy} onclick={close}>Cancel</button>
          <button type="submit" disabled={busy}>{busy ? 'creating…' : 'Create tenant'}</button>
        </div>
      </footer>
    {/if}
  </form>
</dialog>

<style>
  .tenant-dialog {
    width: min(34rem, calc(100vw - 2rem));
    max-height: calc(100dvh - 4rem);
    padding: 0;
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    background: var(--surface-1);
    color: var(--text);
  }
  .tenant-dialog::backdrop {
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
    font-size: var(--fs-base);
    font-weight: var(--fw-semibold);
  }
  .x {
    background: transparent;
    border: none;
    color: var(--text-dim);
    font-size: var(--fs-xl);
    line-height: var(--lh-solid);
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
  .field input {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
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
  .caps {
    display: flex;
    gap: var(--space-3);
  }
  .caps .field {
    flex: 1;
  }
  .hint {
    font-size: var(--fs-2xs);
    color: var(--text-faint);
  }
  .hint code,
  .provisioned code {
    font-family: var(--font-mono);
    color: var(--text-dim);
  }
  .provisioned {
    margin: 0;
    font-size: var(--fs-sm);
    line-height: var(--lh-body);
    color: var(--text-dim);
  }
  .provisioned strong {
    color: var(--text);
  }
  .problem {
    margin: 0;
    font-size: var(--fs-xs);
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
    font-size: var(--fs-sm);
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
