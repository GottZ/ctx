<script lang="ts">
  // Alias editor of /gateway/models (design/05 §3.3 editor rules; wave W05-13
  // — RC-D C5). ONE dialog for both modes: `row` null = "New model alias",
  // a row = "Edit model alias". Server = internal/handler/model_alias_manage.go
  // (authoritative); the rules live in catalog.ts (pure) and are only wired
  // here:
  //   - name and kind are free on create, read-only on edit (LOCKED_ON_EDIT;
  //     buildAliasUpdate has no field for them, the scope is shown as text);
  //   - `enabled` is an explicit checkbox and always on the create wire;
  //   - sensitivity is a radio group from aliasSensitivityOptions — below the
  //     current floor only for a server-admin, and lowering
  //     (needsDowngradeConfirm) opens a confirm FIRST; only the confirmed
  //     request is sent, with confirm_sensitivity_downgrade:true;
  //   - server-admin create: tenant + scope pickers (default: no tenant =
  //     _global; a tenant scope always rides with its tenant_id);
  //   - a 422 `fields` answer lands at its field (aria-invalid + message), the
  //     dialog stays open; any other refusal shows as an alert line.
  // The lowering confirm is a ConfirmDialog WITHOUT the `danger` arm step: the
  // editor's own submit already was the first deliberate click, the confirm is
  // the second (E13-4 pins one click on "Lower sensitivity"); the warning line
  // is styled as a warning through the children snippet.
  import { onMount, untrack } from 'svelte'
  import { toApiError } from '../../lib/api'
  import { listScopes } from '../../lib/api/scopes'
  import { listTenants } from '../../lib/api/tenants'
  import type { ModelAliasKind, ModelAliasView, ModelAliasWriteResult, Tenant } from '../../lib/api/types'
  import ConfirmDialog from '../../lib/components/ConfirmDialog.svelte'
  import Modal from '../../lib/ui/Modal.svelte'
  import AliasPolicyForm from './AliasPolicyForm.svelte'
  import type { AliasesModel } from './aliases.svelte'
  import {
    ALIAS_KINDS,
    ALIAS_SENSITIVITIES,
    DOWNGRADE_WARNING,
    aliasSensitivityOptions,
    buildAliasCreate,
    buildAliasUpdate,
    fieldErrors,
    needsDowngradeConfirm,
    policyKeys,
    type AliasForm,
    type AliasSensitivity,
  } from './catalog'

  let {
    model,
    row = null,
    serverAdmin,
    homeScope = null,
    ondone,
    onclose,
  }: {
    model: AliasesModel
    /** null = create; a row = edit that alias */
    row?: ModelAliasView | null
    serverAdmin: boolean
    /** the caller's home scope (tenant-admin create target), shown as text */
    homeScope?: string | null
    /** a successful write: the line the page shows */
    ondone?: (message: string) => void
    onclose: () => void
  } = $props()

  const asSensitivity = (s: string | null | undefined): AliasSensitivity =>
    (ALIAS_SENSITIVITIES as readonly string[]).includes(s ?? '') ? (s as AliasSensitivity) : 'credentials'
  const asKind = (k: string | undefined): ModelAliasKind =>
    (ALIAS_KINDS as readonly string[]).includes(k ?? '') ? (k as ModelAliasKind) : 'chat'

  // The dialog is mounted per target (the page keys it), so the initial row is the row.
  const initial = untrack(() => row)
  const editing = initial !== null
  const current: AliasSensitivity | null = initial === null ? null : asSensitivity(initial.sensitivity)

  // --- form state ---
  let name = $state(initial?.name ?? '')
  let kind = $state<ModelAliasKind>(asKind(initial?.kind))
  let role = $state(initial?.role ?? '')
  let description = $state(initial?.description ?? '')
  let sensitivity = $state<AliasSensitivity>(current ?? 'credentials')
  // a NEW alias starts disabled (fail-closed — the handler: "a new alias is
  // never live by default"; Fremd-Review C5 S2 #3); enabling is a deliberate tick
  let enabled = $state(initial?.enabled ?? false)
  let policy = $state<Record<string, unknown>>({ ...(initial?.policy ?? {}) })
  let tenantId = $state('')
  let scope = $state('')

  // --- server-admin tenant / scope pickers (create only) ---
  let tenants = $state<Tenant[]>([])
  let tenantsError = $state<string | null>(null)
  let scopes = $state<string[]>([])
  let scopesStatus = $state<'idle' | 'loading' | 'ready' | 'error'>('idle')
  let scopesError = $state<string | null>(null)
  let scopeSeq = 0

  // --- submit ---
  let busy = $state(false)
  let error = $state<string | null>(null)
  let fieldErr = $state<Record<string, string>>({})
  let confirming = $state(false)

  let dialogEl: HTMLDialogElement | undefined = $state()
  let firstEl = $state<HTMLInputElement>()
  let roleEl = $state<HTMLInputElement>()

  const sensOpts = $derived(aliasSensitivityOptions(serverAdmin, current))
  const FORM_FIELDS = new Set(['name', 'kind', 'role', 'description', 'sensitivity', 'enabled', 'scope', 'tenant_id'])
  const policyKeySet = $derived(new Set(policyKeys(kind)))
  // 422 entries without a control of their own (e.g. `policy` as a whole) — shown in the alert line.
  const unplaced = $derived(
    Object.entries(fieldErr).filter(([k]) => !FORM_FIELDS.has(k) && !policyKeySet.has(k) && !policyKeySet.has(k.replace(/^policy\./, ''))),
  )

  onMount(() => {
    // name is read-only on edit: the first editable field takes the focus
    if (editing) roleEl?.focus()
    else firstEl?.focus()
    if (serverAdmin && !editing) void loadTenants()
  })

  async function loadTenants(): Promise<void> {
    try {
      tenants = (await listTenants()).tenants
    } catch (err) {
      tenantsError = toApiError(err).message
    }
  }

  async function loadScopes(id: string): Promise<void> {
    const seq = ++scopeSeq
    scopes = []
    scopesError = null
    if (id === '') {
      scopesStatus = 'idle'
      return
    }
    scopesStatus = 'loading'
    try {
      const res = await listScopes(id)
      if (seq !== scopeSeq) return
      scopes = res.scopes.map((s) => s.scope).filter((s) => s !== '_global')
      scopesStatus = 'ready'
    } catch (err) {
      if (seq !== scopeSeq) return
      scopesError = toApiError(err).message
      scopesStatus = 'error'
    }
  }

  function changeTenant(id: string): void {
    tenantId = id
    scope = ''
    void loadScopes(id)
  }

  function changeKind(next: ModelAliasKind): void {
    kind = next
    // a policy key of the previous kind is no key of this one
    const keep = new Set(policyKeys(next))
    policy = Object.fromEntries(Object.entries(policy).filter(([k]) => keep.has(k)))
  }

  function form(): AliasForm {
    return { name, kind, role, scope, tenantId: tenantId === '' ? undefined : tenantId, sensitivity, description, enabled, policy }
  }

  function lowers(): boolean {
    if (!editing) return needsDowngradeConfirm(null, sensitivity)
    return sensitivity !== current && needsDowngradeConfirm(current, sensitivity)
  }

  /** Validate locally; null = ready to send (after the confirm when lowering). */
  function precheck(): string | null {
    if (!editing && serverAdmin && tenantId !== '' && scope === '') return 'pick a scope of the tenant — a tenant alias lives in one of its scopes'
    const f = form()
    const built = initial === null ? buildAliasCreate(f, true) : buildAliasUpdate(initial, f, true)
    return 'error' in built ? built.error : null
  }

  async function send(): Promise<boolean> {
    const f = form()
    let res: ModelAliasWriteResult
    busy = true
    error = null
    fieldErr = {}
    try {
      if (initial === null) {
        const built = buildAliasCreate(f, lowers())
        if ('error' in built) throw new Error(built.error)
        res = await model.create(built.spec)
      } else {
        const built = buildAliasUpdate(initial, f, lowers())
        if ('error' in built) throw new Error(built.error)
        res = await model.update(initial.id, built.patch)
      }
    } catch (err) {
      const e = toApiError(err)
      const fe = fieldErrors(e)
      fieldErr = fe
      error = Object.keys(fe).length > 0 ? `${e.message} — see the marked fields` : e.message
      return false
    } finally {
      busy = false
    }
    const a = res.model_alias
    const what = initial === null ? 'created' : 'saved'
    const warn = res.warnings && res.warnings.length > 0 ? ` — ${res.warnings.join('; ')}` : ''
    ondone?.(`Alias ${a.name} (${a.scope}) ${what}, ${a.enabled ? 'enabled' : 'disabled'}${warn}.`)
    return true
  }

  async function submit(): Promise<void> {
    if (busy || confirming) return
    const problem = precheck()
    if (problem !== null) {
      error = problem
      fieldErr = {}
      return
    }
    if (lowers()) {
      confirming = true
      return
    }
    if (await send()) dialogEl?.close()
  }

  // The confirm closes itself after this resolves; a refusal is shown in the
  // editor (at its field), not in the confirm — so no rethrow.
  async function confirmLowering(): Promise<void> {
    const ok = await send()
    setTimeout(() => {
      confirming = false
      if (ok) dialogEl?.close()
    }, 0)
  }

  function close(): void {
    if (busy) return
    dialogEl?.close()
  }

  const title = editing ? 'Edit model alias' : 'New model alias'
  const ids = `ae-${Math.random().toString(36).slice(2, 9)}`
</script>

<Modal bind:dialogEl width="44rem" dismissable={!busy && !confirming} ariaLabelledby={`${ids}-title`} {onclose}>
  <form
    method="dialog"
    onsubmit={(e) => {
      e.preventDefault()
      void submit()
    }}
  >
    <header>
      <h2 id={`${ids}-title`}>{title}</h2>
      <button type="button" class="x" title="close" aria-label="close" disabled={busy} onclick={close}>×</button>
    </header>

    <div class="body">
      <div class="row2">
        <div class="field">
          <label class="lbl" for={`${ids}-name`}>name</label>
          <input
            bind:this={firstEl}
            id={`${ids}-name`}
            type="text"
            spellcheck="false"
            autocomplete="off"
            maxlength="200"
            placeholder="the model id callers send, e.g. chat"
            disabled={busy || editing}
            aria-invalid={fieldErr.name ? 'true' : undefined}
            aria-describedby={fieldErr.name ? `${ids}-name-err` : undefined}
            bind:value={name}
          />
          {#if fieldErr.name}<span class="field-error" id={`${ids}-name-err`}>{fieldErr.name}</span>{/if}
        </div>
        <div class="field">
          <label class="lbl" for={`${ids}-kind`}>kind</label>
          <select
            id={`${ids}-kind`}
            value={kind}
            disabled={busy || editing}
            aria-invalid={fieldErr.kind ? 'true' : undefined}
            aria-describedby={fieldErr.kind ? `${ids}-kind-err` : undefined}
            onchange={(e) => changeKind(e.currentTarget.value as ModelAliasKind)}
          >
            {#each ALIAS_KINDS as k (k)}
              <option value={k}>{k}</option>
            {/each}
          </select>
          {#if fieldErr.kind}<span class="field-error" id={`${ids}-kind-err`}>{fieldErr.kind}</span>{/if}
        </div>
      </div>
      {#if editing}
        <p class="hint" id={`${ids}-locked`}>
          name, kind and scope are fixed once created — a rename is a new alias (create it, disable this one).
        </p>
      {/if}

      <div class="field">
        <label class="lbl" for={`${ids}-role`}>role</label>
        <input
          bind:this={roleEl}
          id={`${ids}-role`}
          type="text"
          spellcheck="false"
          autocomplete="off"
          placeholder="the backend role that serves it, e.g. chat"
          disabled={busy}
          aria-invalid={fieldErr.role ? 'true' : undefined}
          aria-describedby={fieldErr.role ? `${ids}-role-err` : undefined}
          bind:value={role}
        />
        {#if fieldErr.role}<span class="field-error" id={`${ids}-role-err`}>{fieldErr.role}</span>{/if}
      </div>

      <div class="field">
        <label class="lbl" for={`${ids}-desc`}>description</label>
        <input
          id={`${ids}-desc`}
          type="text"
          maxlength="1000"
          placeholder="optional"
          disabled={busy}
          aria-invalid={fieldErr.description ? 'true' : undefined}
          aria-describedby={fieldErr.description ? `${ids}-desc-err` : undefined}
          bind:value={description}
        />
        {#if fieldErr.description}<span class="field-error" id={`${ids}-desc-err`}>{fieldErr.description}</span>{/if}
      </div>

      {#if editing && initial}
        <p class="meta">scope <code>{initial.scope}</code></p>
      {:else if serverAdmin}
        <div class="row2">
          <div class="field">
            <label class="lbl" for={`${ids}-tenant`}>tenant</label>
            <select
              id={`${ids}-tenant`}
              value={tenantId}
              disabled={busy}
              aria-invalid={fieldErr.tenant_id ? 'true' : undefined}
              onchange={(e) => changeTenant(e.currentTarget.value)}
            >
              <option value="">none — the _global catalog</option>
              {#each tenants as t (t.id)}
                <option value={t.id}>{t.slug} — {t.display_name}</option>
              {/each}
            </select>
            {#if tenantsError}<span class="field-error" role="status">tenant list unavailable: {tenantsError}</span>{/if}
            {#if fieldErr.tenant_id}<span class="field-error">{fieldErr.tenant_id}</span>{/if}
          </div>
          <div class="field">
            <label class="lbl" for={`${ids}-scope`}>scope</label>
            <select
              id={`${ids}-scope`}
              value={scope}
              disabled={busy || tenantId === '' || scopesStatus !== 'ready'}
              aria-invalid={fieldErr.scope ? 'true' : undefined}
              onchange={(e) => (scope = e.currentTarget.value)}
            >
              {#if tenantId === ''}
                <option value="">_global</option>
              {:else}
                <option value="">{scopesStatus === 'loading' ? 'loading scopes…' : 'pick a scope'}</option>
                {#each scopes as s (s)}
                  <option value={s}>{s}</option>
                {/each}
              {/if}
            </select>
            {#if scopesError}<span class="field-error" role="status">scope list unavailable: {scopesError}</span>{/if}
            {#if fieldErr.scope}<span class="field-error">{fieldErr.scope}</span>{/if}
          </div>
        </div>
      {:else}
        <p class="meta">scope <code>{homeScope ?? 'your home scope'}</code> — a tenant alias lives in your home scope.</p>
      {/if}

      <fieldset
        class="opts"
        class:invalid={!!fieldErr.sensitivity}
        aria-describedby={fieldErr.sensitivity ? `${ids}-sens-err` : undefined}
      >
        <legend>sensitivity</legend>
        {#each sensOpts as o (o.value)}
          <label class="check" class:off={o.disabled}>
            <input
              type="radio"
              name={`${ids}-sensitivity`}
              value={o.value}
              disabled={busy || o.disabled}
              bind:group={sensitivity}
            />
            {o.value}
            {#if o.note}<span class="note">{o.note}</span>{/if}
          </label>
        {/each}
        <p class="hint">the floor of prompts this alias carries — decides which backends may serve it.</p>
        {#if lowers()}
          <p class="warn-line">lowering asks for a confirmation: {DOWNGRADE_WARNING}.</p>
        {/if}
        {#if fieldErr.sensitivity}<p class="field-error" id={`${ids}-sens-err`}>{fieldErr.sensitivity}</p>{/if}
      </fieldset>

      <div class="field">
        <label class="check">
          <input
            type="checkbox"
            disabled={busy}
            aria-invalid={fieldErr.enabled ? 'true' : undefined}
            aria-describedby={`${ids}-enabled-hint`}
            bind:checked={enabled}
          />
          enabled
        </label>
        <span class="hint" id={`${ids}-enabled-hint`}>a disabled alias is offered to no consumer.</span>
        {#if fieldErr.enabled}<span class="field-error">{fieldErr.enabled}</span>{/if}
      </div>

      <AliasPolicyForm {kind} bind:policy errors={fieldErr} disabled={busy} />

      {#if error}
        <div class="problem" role="alert">
          <p>{error}</p>
          {#each unplaced as [k, m] (k)}
            <p><code>{k}</code>: {m}</p>
          {/each}
        </div>
      {/if}
    </div>

    <footer>
      <div class="actions">
        <button type="button" class="ghost" disabled={busy} onclick={close}>Cancel</button>
        <button type="submit" disabled={busy}>
          {busy ? 'saving…' : editing ? 'Save' : 'Create alias'}
        </button>
      </div>
    </footer>
  </form>
</Modal>

{#if confirming}
  <ConfirmDialog
    title="Lower sensitivity"
    confirmLabel="Lower sensitivity"
    onconfirm={confirmLowering}
    oncancel={() => (confirming = false)}
  >
    <p class="warn-line">
      Lower {editing ? `"${initial?.name}" from ${current}` : `the new alias "${name.trim()}" from credentials`} to
      {sensitivity}? {DOWNGRADE_WARNING}.
    </p>
  </ConfirmDialog>
{/if}

<style>
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
  .row2 {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(14rem, 1fr));
    gap: var(--space-3);
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    min-width: 0;
  }
  .lbl,
  legend {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-dim);
  }
  legend {
    padding: 0 var(--space-1);
  }
  .field input[type='text'],
  .field select {
    min-height: 24px;
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-2);
    background: var(--surface-0);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
  }
  .field input:disabled,
  .field select:disabled {
    opacity: 0.7;
    cursor: not-allowed;
  }
  [aria-invalid='true'] {
    border-color: var(--danger);
  }
  .hint {
    margin: 0;
    font-size: var(--fs-xs);
    color: var(--text-dim);
  }
  .meta {
    margin: 0;
    font-size: var(--fs-sm);
    color: var(--text-dim);
  }
  code {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    color: var(--text);
  }
  .field-error {
    margin: 0;
    font-size: var(--fs-xs);
    color: var(--danger);
  }
  fieldset {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-1) var(--space-3);
    margin: 0;
    padding: var(--space-2);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
  fieldset.invalid {
    border-color: var(--danger);
  }
  fieldset .hint,
  fieldset .warn-line,
  fieldset .field-error {
    flex-basis: 100%;
  }
  .check {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    min-height: 24px;
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    color: var(--text);
    cursor: pointer;
  }
  .check.off {
    color: var(--text-dim);
    cursor: not-allowed;
  }
  .note {
    font-family: var(--font-ui);
    font-size: var(--fs-xs);
    color: var(--text-dim);
  }
  .warn-line {
    margin: 0;
    font-size: var(--fs-xs);
    padding: var(--space-1) var(--space-2);
    border: 1px solid var(--warn);
    border-radius: var(--radius);
    color: var(--warn);
  }
  .problem {
    font-size: var(--fs-xs);
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius);
    border: 1px solid var(--danger);
    color: var(--text);
    background: var(--danger-dim);
  }
  .problem p {
    margin: 0;
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
  footer button {
    min-height: 24px;
    font-family: var(--font-ui);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-3);
    background: var(--surface-2);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
    cursor: pointer;
  }
  footer button.ghost {
    background: transparent;
  }
  footer button:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
</style>
