<script lang="ts">
  // Project-provisioning wizard (design 04 §7-U12, wave U12) — a GUIDED sequencer
  // over the REAL compound model (workflow-seams.md §7.1/§7.2), mounted in /admin
  // (server-admin). It reuses the existing provision surfaces (the tenant-create
  // compound, scope-create, api-key-create) and adds NO backend compound.
  //
  // Two entry modes:
  //   new       3 ordered steps — tenant-create (K10 compound) then scope-create
  //             (repo scope) then api-key-create (K12 agent-key template).
  //   existing  2 steps — a repo scope + an agent key INTO an existing tenant
  //             (the §9.7 two-manage-calls case).
  //
  // Reveal-once hygiene (design 05 §6): the owner-key and agent-key PLAINTEXT live
  // ONLY in this component local state (revealed via RevealOnceKey, wiped on ack);
  // the ProvisionModel keeps only the non-secret checkpoint (tenant + repo scope +
  // stage) so a close/reopen RESUMES the flow and never re-shows a key.
  import { toApiError } from '../../lib/api'
  import type { ApiKeyCreateResult, TenantCreateResult, Tenant } from '../../lib/api/types'
  import { ProvisionModel } from './provision.svelte'
  import { validSlug, parseLimit } from './TenantCreateDialog.svelte'
  import RevealOnceKey from '../tenant/RevealOnceKey.svelte'
  import Modal from '../../lib/ui/Modal.svelte'

  let {
    model,
    tenants,
    onclose,
  }: {
    model: ProvisionModel
    /** The server-admin tenant register — the existing-tenant picker source. */
    tenants: Tenant[]
    onclose: () => void
  } = $props()

  let dialogEl: HTMLDialogElement | undefined = $state()

  // --- Step 1 (tenant-create) form ---
  let slug = $state('')
  let displayName = $state('')
  let maxScopes = $state('25')
  let maxKeys = $state('50')

  // --- Existing-tenant picker (entry, alt-flow) ---
  let pickedTenant = $state('')

  // --- Step 2 (scope-create) form ---
  let scopeName = $state('')

  // --- Step 3 (api-key-create) form ---
  let keyLabel = $state('')

  // --- Reveal-once transient plaintext (NEVER on the model) ---
  let ownerKey = $state<string | null>(null)
  let agentKey = $state<string | null>(null)

  // Total step count drives the "Step X of Y" indicator (3 with a new tenant, 2
  // for the existing-tenant alt-flow; unknown at the entry chooser).
  let totalSteps = $derived(model.mode === 'existing' ? 2 : 3)
  let stepNo = $derived.by(() => {
    if (model.mode === 'existing') return model.stage === 'scope' ? 1 : model.stage === 'key' ? 2 : 0
    return model.stage === 'tenant' ? 1 : model.stage === 'scope' ? 2 : model.stage === 'key' ? 3 : 0
  })

  async function submitTenant(): Promise<void> {
    if (model.busy) return
    const s = slug.trim()
    if (s === '') {
      model.error = 'slug is required'
      return
    }
    if (!validSlug(s)) {
      model.error = 'slug must be 1–24 chars: lowercase a–z, 0–9, internal hyphen (no leading/trailing -)'
      return
    }
    const dn = displayName.trim()
    if (dn === '') {
      model.error = 'display name is required'
      return
    }
    const ms = parseLimit(maxScopes)
    if (ms === 'invalid') {
      model.error = 'max scopes must be a positive whole number (or empty for the server default)'
      return
    }
    const mk = parseLimit(maxKeys)
    if (mk === 'invalid') {
      model.error = 'max keys must be a positive whole number (or empty for the server default)'
      return
    }
    try {
      const res: TenantCreateResult = await model.createTenantStep({
        slug: s,
        display_name: dn,
        max_scopes: ms,
        max_keys: mk,
      })
      // Reveal the owner-key once; the model already advanced to the scope stage,
      // so acking the reveal drops straight into step 2.
      ownerKey = res.owner_key
    } catch {
      // model.error is already set by the step; the slug/name inputs are kept.
    }
  }

  function ackOwner(): void {
    ownerKey = null
  }

  function continueExisting(): void {
    const id = pickedTenant
    if (id === '') {
      model.error = 'pick a tenant'
      return
    }
    const t = tenants.find((x) => x.id === id)
    if (t === undefined) {
      model.error = 'unknown tenant'
      return
    }
    model.chooseExisting(t.id, t.slug)
  }

  async function submitScope(): Promise<void> {
    if (model.busy) return
    const name = scopeName.trim()
    if (name === '') {
      model.error = 'repo scope name is required'
      return
    }
    try {
      await model.createScopeStep(name)
      // Success advances to the key stage; keep no client-built prefix — the
      // model holds the server-built scope. Only clear the input on success so a
      // 409 keeps the draft (U10).
      scopeName = ''
    } catch {
      // model.error set; the typed name is retained for a retry.
    }
  }

  async function submitKey(): Promise<void> {
    if (model.busy) return
    const lbl = keyLabel.trim()
    if (lbl === '') {
      model.error = 'agent key label is required'
      return
    }
    try {
      const res: ApiKeyCreateResult = await model.mintAgentKeyStep(lbl)
      agentKey = res.api_key
    } catch {
      // model.error set; the label input is kept.
    }
  }

  function ackAgent(): void {
    agentKey = null
  }

  /** Finish a completed run: reset the checkpoint and close (no key lingers). */
  function finish(): void {
    model.reset()
    dialogEl?.close()
  }

  /** Cancel/close WITHOUT resetting — a mid-flow checkpoint stays resumable
   *  (design 04 §7-U12: after step 2 the repo scope exists, the wizard resumes). */
  function cancel(): void {
    if (model.busy) return
    dialogEl?.close()
  }

  // Guard the backdrop/Esc while a write is in flight or a key is being revealed
  // (a reveal-once value must be acknowledged, not dismissed by a stray click).
  let revealing = $derived(ownerKey !== null || agentKey !== null)
</script>

<Modal
  bind:dialogEl
  width="34rem"
  dismissable={!model.busy && !revealing}
  backdropClose={!model.busy && !revealing}
  ariaLabelledby="provision-wizard-title"
  {onclose}
>
  <section class="wizard" aria-label="project provisioning wizard">
    <header>
      <h2 id="provision-wizard-title">Provision project</h2>
      {#if stepNo > 0}
        <span class="steps" aria-label="wizard progress">step {stepNo} of {totalSteps}</span>
      {/if}
      <button type="button" class="x" title="close" aria-label="close" disabled={model.busy} onclick={cancel}>×</button>
    </header>

    <div class="body">
      {#if ownerKey !== null}
        <!-- Step 1 reveal: the compound owner key (reveal-once). -->
        <p class="lead" role="status">
          Tenant <strong>{model.tenantSlug}</strong> is live with its initial scope and an owner key.
          Store the owner key now — then continue to the repo scope.
        </p>
        <RevealOnceKey apiKey={ownerKey} onack={ackOwner} />
      {:else if agentKey !== null}
        <!-- Step 3 reveal: the K12 agent key (reveal-once). -->
        <p class="lead" role="status">
          Agent key for <code>{model.repoScope}</code> minted. It reads and writes ONLY that repo scope
          (allowed=[], write=[]). Store it now — it cannot be retrieved again.
        </p>
        <RevealOnceKey apiKey={agentKey} onack={ackAgent} />
      {:else if model.stage === 'entry'}
        <p class="lead">Provision a repo corpus: a repo scope plus a scoped agent key, over the atomic tenant compound.</p>
        <div class="choices">
          <button type="button" class="choice" onclick={() => model.chooseNew()}>
            <span class="ct">New tenant + repo scope</span>
            <span class="cd">Create a fresh tenant (compound: tenant, main scope, owner key), then its repo scope + agent key.</span>
          </button>
          <div class="choice alt">
            <span class="ct">Repo scope in an existing tenant</span>
            <span class="cd">Add a repo scope + agent key to a tenant that already exists (2 steps).</span>
            <div class="row">
              <label class="grow">
                <span class="sr">target tenant</span>
                <select aria-label="target tenant" bind:value={pickedTenant} disabled={tenants.length === 0}>
                  <option value="">select a tenant…</option>
                  {#each tenants as t (t.id)}
                    <option value={t.id}>{t.slug}{t.display_name ? ` — ${t.display_name}` : ''}</option>
                  {/each}
                </select>
              </label>
              <button type="button" onclick={continueExisting} disabled={pickedTenant === ''}>Continue</button>
            </div>
          </div>
        </div>
      {:else if model.stage === 'tenant'}
        <h3>Step 1 — create tenant</h3>
        <label class="field">
          <span class="lbl">slug</span>
          <input
            type="text"
            spellcheck="false"
            autocapitalize="off"
            autocomplete="off"
            aria-label="tenant slug"
            placeholder="e.g. acme-research"
            disabled={model.busy}
            bind:value={slug}
          />
          <span class="hint">
            URL-safe id: lowercase a–z, 0–9, internal hyphen, ≤24 chars. Becomes the
            <code>{(slug.trim() || 'slug')}:…</code> scope prefix — cannot change later.
          </span>
        </label>
        <label class="field">
          <span class="lbl">display name</span>
          <input type="text" spellcheck="false" aria-label="display name" placeholder="e.g. Acme Research" disabled={model.busy} bind:value={displayName} />
        </label>
        <div class="caps">
          <label class="field">
            <span class="lbl">max scopes</span>
            <input type="number" min="1" step="1" inputmode="numeric" aria-label="max scopes" disabled={model.busy} bind:value={maxScopes} />
          </label>
          <label class="field">
            <span class="lbl">max keys</span>
            <input type="number" min="1" step="1" inputmode="numeric" aria-label="max keys" disabled={model.busy} bind:value={maxKeys} />
          </label>
        </div>
        {#if model.error}<p class="problem" role="alert">{model.error}</p>{/if}
        <div class="actions">
          <button type="button" class="ghost" disabled={model.busy} onclick={cancel}>Cancel</button>
          <button type="button" disabled={model.busy} onclick={() => void submitTenant()}>
            {model.busy ? 'creating…' : 'Create tenant'}
          </button>
        </div>
      {:else if model.stage === 'scope'}
        <h3>Step {model.mode === 'existing' ? '1' : '2'} — repo scope</h3>
        <p class="lead">
          {#if model.tenantSlug}
            Provisioning into tenant <strong>{model.tenantSlug}</strong>. The server namespaces the repo scope.
          {:else}
            The server namespaces the repo scope to the target tenant.
          {/if}
        </p>
        <label class="field">
          <span class="lbl">repo scope name</span>
          <input
            type="text"
            spellcheck="false"
            aria-label="repo scope name"
            placeholder="bare name, e.g. myrepo"
            disabled={model.busy}
            bind:value={scopeName}
            onkeydown={(e) => {
              if (e.key === 'Enter') {
                e.preventDefault()
                void submitScope()
              }
            }}
          />
          <span class="hint">The server prepends this tenant namespace and returns the full scope — the client never builds the prefix.</span>
        </label>
        {#if model.error}<p class="problem" role="alert">{model.error}</p>{/if}
        <div class="actions">
          <button type="button" class="ghost" disabled={model.busy} onclick={cancel}>Cancel</button>
          <button type="button" disabled={model.busy} onclick={() => void submitScope()}>
            {model.busy ? 'creating…' : 'Create scope'}
          </button>
        </div>
      {:else if model.stage === 'key'}
        <h3>Step {model.mode === 'existing' ? '2' : '3'} — agent key</h3>
        <label class="field">
          <span class="lbl">home scope</span>
          <input type="text" readonly aria-label="agent home scope" value={model.repoScope ?? ''} />
          <span class="hint">K12 template: the agent key reads and writes ONLY this repo scope (allowed=[], write=[]).</span>
        </label>
        <label class="field">
          <span class="lbl">agent key label</span>
          <input
            type="text"
            spellcheck="false"
            aria-label="agent key label"
            placeholder="who/what this key is for"
            disabled={model.busy}
            bind:value={keyLabel}
          />
        </label>
        {#if model.error}<p class="problem" role="alert">{model.error}</p>{/if}
        <div class="actions">
          <button type="button" class="ghost" disabled={model.busy} onclick={cancel}>Cancel</button>
          <button type="button" disabled={model.busy} onclick={() => void submitKey()}>
            {model.busy ? 'minting…' : 'Mint agent key'}
          </button>
        </div>
      {:else if model.stage === 'done'}
        <p class="lead" role="status">
          Repo scope <code>{model.repoScope}</code> is provisioned with a scoped agent key. The background
          pipelines pick up the new scope automatically.
        </p>
        <div class="actions">
          <button type="button" onclick={finish}>Finish</button>
        </div>
      {/if}
    </div>
  </section>
</Modal>

<style>
  .wizard {
    display: flex;
    flex-direction: column;
    max-height: calc(100dvh - 4rem);
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
    font-size: var(--fs-base);
    font-weight: var(--fw-semibold);
  }
  .steps {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-dim);
  }
  .x {
    margin-left: auto;
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
  h3 {
    margin: 0;
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-faint);
  }
  .lead {
    margin: 0;
    font-size: var(--fs-sm);
    line-height: var(--lh-body);
    color: var(--text-dim);
  }
  .lead strong {
    color: var(--text);
  }
  .choices {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .choice {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    text-align: left;
    padding: var(--space-2) var(--space-3);
    background: var(--surface-2);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
    cursor: pointer;
  }
  .choice.alt {
    cursor: default;
    background: var(--surface-1);
  }
  .ct {
    font-family: var(--font-ui);
    font-size: var(--fs-sm);
    font-weight: var(--fw-semibold);
  }
  .cd {
    font-size: var(--fs-xs);
    color: var(--text-dim);
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
  .sr {
    position: absolute;
    width: 1px;
    height: 1px;
    overflow: hidden;
    clip-path: inset(50%);
    white-space: nowrap;
  }
  input,
  select {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-2);
    background: var(--surface-0);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
  }
  input:disabled,
  select:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  input[readonly] {
    border-color: var(--accent);
    color: var(--text);
  }
  .caps {
    display: flex;
    gap: var(--space-3);
  }
  .caps .field {
    flex: 1;
  }
  .row {
    display: flex;
    gap: var(--space-2);
    align-items: stretch;
    margin-top: var(--space-1);
  }
  .grow {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
  }
  .hint {
    font-size: var(--fs-2xs);
    color: var(--text-faint);
  }
  .hint code,
  .lead code {
    font-family: var(--font-mono);
    color: var(--text-dim);
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: var(--space-2);
  }
  button {
    font-family: var(--font-ui);
    font-size: var(--fs-sm);
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
</style>
