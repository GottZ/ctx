<script lang="ts">
  // Backend create/edit dialog (design §3.5). The backend is edited as ONE
  // tuple (host+protocol+model_map+api_key_ref together — a host flip without a
  // protocol flip 404s). Built on the shared Modal shell (design 05 §7 Q9 — the
  // native <dialog>+showModal wiring lives in lib/ui/Modal.svelte). Trust
  // ELEVATION needs an explicit in-dialog confirm step → confirm_trust_elevation
  // :true; the server stays authoritative (it 400s without it). On edit only the
  // changed fields are PATCHed (backendDiff).
  import { untrack } from 'svelte'
  import { toApiError } from '../../../lib/api'
  import type { BackendListItem } from '../../../lib/api/types'
  import {
    backendDiff,
    createSpec,
    draftFromBackend,
    fieldErrors,
    isEmptySpec,
    isTrustElevation,
    LOCALITIES,
    modelMapToRows,
    PROTOCOLS,
    PROVIDER_CLASSES,
    rowsToModelMap,
    TRUST_LEVELS,
    trustRank,
    type BackendFieldError,
    type ModelMapRow,
  } from '../../../lib/backends'
  import ModelMapEditor from './ModelMapEditor.svelte'
  import RolesSelect from './RolesSelect.svelte'
  import Modal from '../../../lib/ui/Modal.svelte'

  let {
    mode,
    backend = null,
    save,
    onclose,
  }: {
    mode: 'create' | 'edit'
    backend?: BackendListItem | null
    /** Returns the server warnings on success; throws ApiError on failure. */
    save: (args: {
      mode: 'create' | 'edit'
      name: string
      original: BackendListItem | null
      draft: ReturnType<typeof draftFromBackend>
      confirmTrust: boolean
    }) => Promise<string[]>
    onclose: () => void
  } = $props()

  // The dialog is mounted fresh per open (modal — the table behind is inert),
  // so the props are a one-shot snapshot; untrack reads them as initial values
  // for the editable draft without Svelte flagging a missed reactive capture.
  const init = untrack(() => {
    const b = backend
    const d = b ? draftFromBackend(b) : null
    return {
      name: b?.name ?? '',
      baseUrl: d?.base_url ?? '',
      protocol: d?.protocol ?? 'openai',
      providerClass: d?.provider_class ?? 'generic',
      apiKeyRef: d?.api_key_ref ?? '',
      locality: d?.locality ?? '',
      numCtx: d?.num_ctx ?? 0,
      trust: d?.trust ?? 'public',
      roles: d ? [...d.roles] : [],
      mmRows: b ? modelMapToRows(b.model_map) : [],
    }
  })

  let name = $state(init.name)
  let baseUrl = $state(init.baseUrl)
  let protocol = $state(init.protocol)
  let providerClass = $state(init.providerClass)
  let apiKeyRef = $state(init.apiKeyRef)
  let locality = $state(init.locality)
  let numCtx = $state(init.numCtx)
  let trust = $state(init.trust)
  let roles = $state<string[]>(init.roles)
  let mmRows = $state<ModelMapRow[]>(init.mmRows)

  let saving = $state(false)
  let error = $state<string | null>(null)
  let fieldErrs = $state<BackendFieldError[]>([])
  let confirmStep = $state(false)

  // Locality is auto-derived from base_url on create, so offer "(auto)" there;
  // on edit the backend already has one (LOCALITIES only).
  const localityOptions = $derived(mode === 'create' ? ['', ...LOCALITIES] : [...LOCALITIES])

  // Live elevation flag: create non-public, or edit raising the rank.
  const elevation = $derived(
    mode === 'create' ? trust !== 'public' : backend !== null && isTrustElevation(backend.trust, trust),
  )
  // Downgrade (edit, rank fell): no confirm, but show which pipelines this
  // backend serves — declaratively from roles (llmlog has blind spots, §3.5).
  const downgrade = $derived(mode === 'edit' && backend !== null && trustRank(trust) < trustRank(backend.trust))

  let dialogEl: HTMLDialogElement | undefined = $state()

  function collectDraft() {
    return {
      base_url: baseUrl.trim(),
      protocol,
      provider_class: providerClass,
      api_key_ref: apiKeyRef.trim(),
      locality,
      num_ctx: numCtx,
      trust,
      roles,
      model_map: rowsToModelMap(mmRows),
    }
  }

  function clientError(): string | null {
    if (mode === 'create' && name.trim() === '') return 'name is required'
    if (baseUrl.trim() === '') return 'base_url is required'
    return null
  }

  async function commit(): Promise<void> {
    const ce = clientError()
    if (ce) {
      error = ce
      return
    }
    // First click on an elevating change → show the confirm step, do not send.
    if (elevation && !confirmStep) {
      confirmStep = true
      return
    }
    saving = true
    error = null
    fieldErrs = []
    try {
      const warnings = await save({
        mode,
        name: name.trim(),
        original: backend,
        draft: collectDraft(),
        confirmTrust: elevation,
      })
      void warnings
      dialogEl?.close()
    } catch (err) {
      error = toApiError(err).message
      fieldErrs = fieldErrors(err)
      confirmStep = false // back to the form to fix
    } finally {
      saving = false
    }
  }

  const trustTip = $derived(TRUST_LEVELS.find((t) => t.value === trust)?.tip ?? '')
</script>

<Modal bind:dialogEl width="42rem" {onclose}>
  <form
    method="dialog"
    onsubmit={(e) => {
      e.preventDefault()
      void commit()
    }}
  >
    <header>
      <h2>{mode === 'create' ? 'New backend' : `Edit ${backend?.name}`}</h2>
      <button type="button" class="x" title="close" onclick={() => dialogEl?.close()}>×</button>
    </header>

    <div class="body">
      <label class="field">
        <span class="lbl">name {mode === 'edit' ? '(immutable)' : ''}</span>
        <input
          type="text"
          spellcheck="false"
          placeholder="stable handle, e.g. herbert-chat"
          disabled={mode === 'edit' || saving}
          bind:value={name}
        />
      </label>

      <div class="two">
        <label class="field">
          <span class="lbl">base_url</span>
          <input type="text" spellcheck="false" placeholder="http://host:port" disabled={saving} bind:value={baseUrl} />
        </label>
        <label class="field narrow">
          <span class="lbl">protocol</span>
          <select disabled={saving} bind:value={protocol}>
            {#each PROTOCOLS as p (p)}<option value={p}>{p}</option>{/each}
          </select>
        </label>
      </div>

      <div class="two">
        <label class="field narrow">
          <span class="lbl">provider_class</span>
          <select disabled={saving} bind:value={providerClass}>
            {#each PROVIDER_CLASSES as c (c)}<option value={c}>{c}</option>{/each}
          </select>
        </label>
        <label class="field narrow">
          <span class="lbl">locality</span>
          <select disabled={saving} bind:value={locality}>
            {#each localityOptions as l (l)}
              <option value={l}>{l === '' ? '(auto from host)' : l}</option>
            {/each}
          </select>
        </label>
        <label class="field narrow">
          <span class="lbl">num_ctx</span>
          <input
            type="number"
            step="1"
            min="0"
            disabled={saving}
            value={numCtx}
            oninput={(e) => (numCtx = parseInt(e.currentTarget.value, 10) || 0)}
          />
        </label>
      </div>

      <label class="field">
        <span class="lbl">trust</span>
        <select disabled={saving} bind:value={trust}>
          {#each TRUST_LEVELS as t (t.value)}<option value={t.value} title={t.tip}>{t.value}</option>{/each}
        </select>
        <span class="hint">{trustTip}</span>
      </label>

      {#if elevation}
        <p class="problem warn" role="status">
          {mode === 'create'
            ? `trust "${trust}" is above public — more sensitive content may flow to this backend; you'll confirm on save`
            : `raising trust ${backend?.trust} → ${trust} (rank ${trustRank(backend?.trust ?? '')}→${trustRank(trust)}) — confirm on save`}
        </p>
      {:else if downgrade}
        <p class="problem warn" role="status">
          lowering trust {backend?.trust} → {trust}. This backend serves:
          <strong>{backend && backend.roles.length > 0 ? backend.roles.join(', ') : '—'}</strong>. Enforcement is
          server-side and fail-closed.
        </p>
      {/if}

      <label class="field">
        <span class="lbl">api_key_ref</span>
        <input
          type="text"
          spellcheck="false"
          placeholder="secret name (create it in the vault below) — never the value"
          disabled={saving}
          bind:value={apiKeyRef}
        />
      </label>

      <div class="field">
        <span class="lbl">roles</span>
        <RolesSelect value={roles} disabled={saving} onchange={(r) => (roles = r)} />
      </div>

      <div class="field">
        <span class="lbl">model_map</span>
        <ModelMapEditor rows={mmRows} disabled={saving} onchange={(r) => (mmRows = r)} />
      </div>

      {#if error}
        <p class="problem error" role="alert">{error}</p>
      {/if}
      {#each fieldErrs as fe (fe.field)}
        <p class="problem error" role="alert"><code>{fe.field}</code>: {fe.message}</p>
      {/each}
    </div>

    <footer>
      {#if confirmStep}
        <p class="confirm-text">trust decides which content may flow here. Confirm the elevation?</p>
        <div class="actions">
          <button type="button" class="ghost" disabled={saving} onclick={() => (confirmStep = false)}>Back</button>
          <button type="submit" class="danger" disabled={saving}>{saving ? 'saving…' : 'Confirm & save'}</button>
        </div>
      {:else}
        <div class="actions">
          <button type="button" class="ghost" disabled={saving} onclick={() => dialogEl?.close()}>Cancel</button>
          <button type="submit" disabled={saving}>{saving ? 'saving…' : mode === 'create' ? 'Create' : 'Save'}</button>
        </div>
      {/if}
    </footer>
  </form>
</Modal>

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
  }
  .body {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    padding: var(--space-3);
    overflow-y: auto;
  }
  .two {
    display: flex;
    gap: var(--space-3);
    flex-wrap: wrap;
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    flex: 1;
    min-width: 0;
  }
  .field.narrow {
    flex: 0 1 9rem;
  }
  .lbl {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-faint);
  }
  .field input,
  .field select {
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
    opacity: 0.55;
    cursor: not-allowed;
  }
  .hint {
    font-size: var(--fs-2xs);
    color: var(--text-faint);
  }
  footer {
    border-top: 1px solid var(--border);
    padding: var(--space-2) var(--space-3);
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .confirm-text {
    margin: 0;
    font-size: var(--fs-sm);
    color: var(--warn);
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: var(--space-2);
  }
  button.ghost {
    background: transparent;
  }
  button.danger {
    border-color: var(--danger);
    color: var(--danger);
  }
  .problem {
    margin: 0;
    font-size: var(--fs-xs);
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius);
    border: 1px solid;
  }
  .problem.error {
    color: var(--danger);
    border-color: var(--danger);
    background: var(--danger-dim);
  }
  .problem.warn {
    color: var(--warn);
    border-color: var(--warn);
    background: transparent;
  }
  code {
    font-family: var(--font-mono);
  }
</style>
