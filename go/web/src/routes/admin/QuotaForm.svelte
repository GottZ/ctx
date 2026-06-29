<script lang="ts">
  // Per-scope quota editor (design 04 §3 A4) — SCOPE-keyed, never tenant-id-keyed
  // (quota_manage.go:94). Loads the scope's current quota (tenant-quota-get), lets
  // a server-admin edit the four budget dimensions + on_exceed + enabled, and
  // writes them (tenant-quota-set) followed by an explicit re-get so the rendered
  // state is the persisted one, not the optimistic local fields. A blank limit
  // field = that dimension is UNLIMITED (null), never 0 — the null↔unlimited
  // mapping lives in the pure quota-form.svelte helper (the A4 vitest gate).
  import { getTenantQuota, setTenantQuota } from '../../lib/api/quota'
  import { toApiError, type ApiError } from '../../lib/api'
  import type { ResourceStatus } from '../../lib/resource.svelte'
  import type { QuotaSpec } from '../../lib/api/types'
  import {
    ON_EXCEED_OPTIONS,
    fieldsFromView,
    toQuotaSpec,
    type QuotaFormFields,
  } from './quota-form.svelte'

  interface Props {
    scope: string
  }
  let { scope }: Props = $props()

  let status = $state<ResourceStatus>('idle')
  let loadError = $state<ApiError | null>(null)
  let saveError = $state<ApiError | null>(null)
  let formError = $state<string | null>(null)
  let saving = $state(false)
  let saved = $state(false)
  // The nil-policy signal off the loaded view (no quota row → fully unlimited).
  let unlimited = $state(false)

  // Editable fields ($state so the bound inputs stay reactive, Svelte-5).
  let fields = $state<QuotaFormFields>({
    enabled: true,
    dailyCost: '',
    monthlyCost: '',
    dailyCalls: '',
    onExceed: 'external_off',
  })

  // The scope whose quota is currently loaded into `fields`. Reading the `scope`
  // PROP and `loadedScope` as $state inside the effect makes it re-run if the
  // prop ever changes (the form reads scope reactively, §kalibrierung), while the
  // idle-guard fires the load exactly once per scope — no reload loop, surviving
  // the boot-restore race like the sibling admin panels.
  let loadedScope = $state<string | null>(null)

  $effect(() => {
    if (scope && scope !== loadedScope && status !== 'loading') {
      void load(scope)
    }
  })

  async function load(s: string): Promise<void> {
    status = 'loading'
    loadError = null
    saved = false
    try {
      const res = await getTenantQuota(s)
      fields = fieldsFromView(res.quota)
      unlimited = res.quota.unlimited === true
      loadedScope = s
      status = 'ready'
    } catch (err) {
      loadError = toApiError(err)
      loadedScope = s // pin so the guard does not re-fire the failing load
      status = 'error'
    }
  }

  /** Validate the fields into a payload; a parse/scope error is surfaced as the
   * inline formError (not a request) and returns null so submit aborts. */
  function buildSpec(): QuotaSpec | null {
    try {
      return toQuotaSpec(scope, fields)
    } catch (err) {
      formError = err instanceof Error ? err.message : String(err)
      return null
    }
  }

  async function submit(e: SubmitEvent): Promise<void> {
    e.preventDefault()
    saveError = null
    formError = null
    saved = false
    const spec = buildSpec()
    if (spec === null) return
    saving = true
    try {
      await setTenantQuota(spec)
      // Re-get: render the persisted state, not the local optimistic fields.
      const res = await getTenantQuota(scope)
      fields = fieldsFromView(res.quota)
      unlimited = res.quota.unlimited === true
      saved = true
    } catch (err) {
      saveError = toApiError(err)
    } finally {
      saving = false
    }
  }

  // svelte-check: ids are scope-derived so multiple forms on one page stay unique.
  const safeId = $derived(scope.replace(/[^a-zA-Z0-9_-]/g, '_'))
</script>

<form class="quota" aria-label="quota for scope {scope}" onsubmit={submit}>
  <header class="head">
    <h3 class="scope">{scope}</h3>
    {#if status === 'ready' && unlimited}
      <span class="hint" title="no quota row — every dimension is unlimited">no quota — unlimited</span>
    {/if}
  </header>

  {#if status === 'loading' || status === 'idle'}
    <p class="state" aria-busy="true">loading quota…</p>
  {:else if status === 'error'}
    <div class="error" role="alert">
      <p>{loadError?.message}</p>
      {#if loadError?.requestId}<p class="request-id">request {loadError.requestId}</p>{/if}
      <button type="button" onclick={() => void load(scope)}>Retry</button>
    </div>
  {:else}
    <div class="grid">
      <label class="check">
        <input type="checkbox" bind:checked={fields.enabled} />
        <span>enabled</span>
      </label>

      <label for="dc-{safeId}">daily cost (USD)</label>
      <input
        id="dc-{safeId}"
        type="text"
        inputmode="decimal"
        placeholder="unlimited"
        bind:value={fields.dailyCost}
      />

      <label for="mc-{safeId}">monthly cost (USD)</label>
      <input
        id="mc-{safeId}"
        type="text"
        inputmode="decimal"
        placeholder="unlimited"
        bind:value={fields.monthlyCost}
      />

      <label for="dcalls-{safeId}">daily calls</label>
      <input
        id="dcalls-{safeId}"
        type="text"
        inputmode="numeric"
        placeholder="unlimited"
        bind:value={fields.dailyCalls}
      />

      <label for="oe-{safeId}">on exceed</label>
      <select id="oe-{safeId}" bind:value={fields.onExceed}>
        {#each ON_EXCEED_OPTIONS as opt (opt)}
          <option value={opt}>{opt}</option>
        {/each}
      </select>
    </div>

    <p class="note">A blank limit means that dimension is unlimited (null) — not 0.</p>

    {#if formError}<p class="error inline" role="alert">{formError}</p>{/if}
    {#if saveError}
      <div class="error" role="alert">
        <p>{saveError.message}</p>
        {#if saveError.requestId}<p class="request-id">request {saveError.requestId}</p>{/if}
      </div>
    {/if}

    <div class="actions">
      <button type="submit" disabled={saving}>{saving ? 'saving…' : 'save quota'}</button>
      {#if saved}<span class="saved" role="status">saved</span>{/if}
    </div>
  {/if}
</form>

<style>
  .quota {
    background: var(--surface-1);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    padding: var(--space-2) var(--space-3) var(--space-3);
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .head {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    border-bottom: 1px solid var(--border);
    padding-bottom: var(--space-1);
  }
  .scope {
    margin: 0;
    font-family: var(--font-mono);
    font-size: 0.9rem;
    font-weight: 600;
    color: var(--text);
  }
  .hint {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    text-transform: uppercase;
    letter-spacing: var(--label-tracking);
    color: var(--text-faint);
  }
  .grid {
    display: grid;
    grid-template-columns: max-content 1fr;
    gap: var(--space-2) var(--space-3);
    align-items: center;
  }
  .check {
    grid-column: 1 / -1;
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    font-size: 0.85rem;
    color: var(--text);
  }
  label {
    font-size: 0.85rem;
    color: var(--text-dim);
  }
  input[type='text'],
  select {
    width: 100%;
    box-sizing: border-box;
    background: var(--surface-2);
    border: 1px solid var(--border);
    border-radius: var(--radius);
    color: var(--text);
    font-family: var(--font-mono);
    font-size: 0.85rem;
    padding: var(--space-1) var(--space-2);
  }
  input:focus,
  select:focus {
    outline: 1px solid var(--accent);
    border-color: var(--accent);
  }
  .note {
    margin: 0;
    color: var(--text-faint);
    font-size: 0.78rem;
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
    gap: var(--space-1);
    align-items: flex-start;
    border: 1px solid var(--danger);
    border-radius: var(--radius);
    background: var(--danger-dim);
    padding: var(--space-2);
    font-size: 0.82rem;
  }
  .error.inline {
    flex-direction: row;
  }
  .error p {
    margin: 0;
    color: var(--danger);
  }
  .request-id {
    font-family: var(--font-mono);
    font-size: 0.72rem;
    color: var(--text-dim) !important;
  }
  .actions {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  button {
    background: var(--surface-2);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
    font-size: 0.82rem;
    padding: var(--space-1) var(--space-3);
    cursor: pointer;
  }
  button:disabled {
    opacity: 0.6;
    cursor: default;
  }
  .saved {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    text-transform: uppercase;
    letter-spacing: var(--label-tracking);
    color: var(--ok);
  }
</style>
