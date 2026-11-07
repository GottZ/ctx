<script lang="ts">
  // Edit a model consumer's policy (design/05 §4.5 Feld-Tier-UX, §4.6; wave
  // W05-07 — RC-D C5). One Modal (its own lazy chunk, like
  // ConsumerCreateDialog) with label, description, tenant_scope, models /
  // "all offered", prompt sensitivity, admission lane (LaneSelect) and the
  // network allowlist. Every control mirrors the server's field tiers through
  // policy.editTiers(consumer, caps): lower sensitivities disabled "server-admin
  // only" for a tenant-admin; without mintModelKeys every widening control is
  // disabled (unchecked models, "all offered", the scope, "any source") and a
  // prefix outside the stored allowlist blocks Save with the reason; a
  // ctx-peer row has no edit control below server-admin (the page never opens
  // this dialog for it). Save sends policy.buildUpdatePatch — the changed
  // fields only — through model.update; a refusal (403 "admin key required",
  // 400 field, 409 label, 429 quota) stays in the dialog as role="alert" and
  // never reads as success. The server remains the authority.
  import { onMount } from 'svelte'
  import { toApiError } from '../../lib/api'
  import { session } from '../../lib/auth.svelte'
  import { listModelAliases, type PromptSensitivity } from '../../lib/api/modelapi'
  import { listScopes } from '../../lib/api/scopes'
  import type { ConsumerView, ExternalLane, ModelAliasView } from '../../lib/api/types'
  import Modal from '../../lib/ui/Modal.svelte'
  import type { ConsumersModel } from './consumers.svelte'
  import LaneSelect from './LaneSelect.svelte'
  import { GLOBAL_SCOPE, offeredAliases, scopeOptions } from './mint'
  import { NOTHING_CHANGED, buildUpdatePatch, editFormFrom, editTiers } from './policy'

  interface Props {
    model: ConsumersModel
    consumer: ConsumerView
    onclose: () => void
    /** success: the consumer's label after the save (the page's toast) */
    onsaved?: (label: string) => void
  }
  let { model, consumer, onclose, onsaved }: Props = $props()

  const serverAdmin = $derived(session.caps.tier === 'server-admin')
  const tiers = $derived(editTiers(consumer, { serverAdmin, mintModelKeys: session.caps.mintModelKeys }))

  // --- form state, seeded once from the consumer the dialog opened for ---
  // svelte-ignore state_referenced_locally
  const start = editFormFrom(consumer)
  let label = $state(start.label)
  let description = $state(start.description)
  let scope = $state(start.scope ?? '')
  let allModels = $state(start.allModels)
  let picked = $state<string[]>([...start.models])
  let sensitivity = $state<PromptSensitivity>(start.sensitivity)
  let lane = $state<ExternalLane>(start.lane)
  let cidrsText = $state(start.cidrsText)
  let anySource = $state(start.cidrsText === '')

  // --- catalog + scopes (the offer of the chosen scope, like the create dialog) ---
  let aliases = $state<ModelAliasView[]>([])
  let aliasesStatus = $state<'loading' | 'ready' | 'error'>('loading')
  let aliasesError = $state<string | null>(null)
  let tenantScopes = $state<string[]>([])

  let busy = $state(false)
  let error = $state<string | null>(null)
  let dialogEl: HTMLDialogElement | undefined = $state()
  let labelInputEl = $state<HTMLInputElement>()

  const scopes = $derived.by(() => {
    const own = scopeOptions({ serverAdmin, tenantScopes, homeScope: session.homeScope })
    return consumer.tenant_scope !== null && !own.includes(consumer.tenant_scope) ? [consumer.tenant_scope, ...own] : own
  })
  const offered = $derived(offeredAliases(aliases, scope === '' ? null : scope))
  const built = $derived(
    buildUpdatePatch(
      consumer,
      { label, description, scope: scope === '' ? null : scope, allModels, models: picked, sensitivity, lane, cidrsText: anySource ? '' : cidrsText },
      tiers,
    ),
  )
  /** The client-side reason Save is blocked — shown unless it is just "nothing changed". */
  const blocker = $derived('error' in built && built.error !== NOTHING_CHANGED ? built.error : null)

  async function loadAliases(): Promise<void> {
    try {
      const res = await listModelAliases()
      aliases = res.model_aliases
      aliasesStatus = 'ready'
    } catch (err) {
      aliasesError = toApiError(err).message
      aliasesStatus = 'error'
    }
  }

  async function loadScopes(): Promise<void> {
    try {
      // server-admin: the consumer's own tenant; a tenant-admin reads its own
      const res = await listScopes(serverAdmin ? consumer.tenant_id : undefined)
      tenantScopes = res.scopes.map((s) => s.scope)
    } catch {
      tenantScopes = []
    }
  }

  onMount(() => {
    labelInputEl?.focus()
    void loadAliases()
    void loadScopes()
  })

  function togglePick(id: string): void {
    picked = picked.includes(id) ? picked.filter((x) => x !== id) : [...picked, id]
  }

  async function submit(): Promise<void> {
    if (busy || !('patch' in built)) return
    busy = true
    error = null
    try {
      const res = await model.update(consumer.id, built.patch)
      onsaved?.(res.consumer.label)
      dialogEl?.close()
    } catch (err) {
      error = toApiError(err).message
    } finally {
      busy = false
    }
  }

  function close(): void {
    if (busy) return
    dialogEl?.close()
  }
</script>

<Modal bind:dialogEl width="40rem" dismissable={!busy} ariaLabelledby="consumer-edit-title" onclose={onclose}>
  <form
    method="dialog"
    onsubmit={(e) => {
      e.preventDefault()
      void submit()
    }}
  >
    <header>
      <h2 id="consumer-edit-title">Edit policy — {consumer.label}</h2>
      <button type="button" class="x" title="close" aria-label="close" disabled={busy} onclick={close}>×</button>
    </header>

    <div class="body">
      <label class="field">
        <span class="lbl">label</span>
        <input bind:this={labelInputEl} type="text" spellcheck="false" maxlength="100" disabled={busy} bind:value={label} />
      </label>

      <label class="field">
        <span class="lbl">description</span>
        <input type="text" maxlength="1000" placeholder="optional" disabled={busy} bind:value={description} />
      </label>

      <div class="field">
        <label class="lbl" for="ce-scope">scope</label>
        <select id="ce-scope" aria-describedby="ce-scope-hint" bind:value={scope} disabled={busy || !tiers.mayChangeScope}>
          <option value="">none — global models only</option>
          {#each scopes as s (s)}
            <option value={s}>{s}</option>
          {/each}
        </select>
        <span class="hint" id="ce-scope-hint">
          {tiers.mayChangeScope
            ? 'the scope whose models the consumer is offered besides the global ones.'
            : 'changing the scope widens access — self-service is off, a server-admin can change it.'}
        </span>
      </div>

      <fieldset class="models" data-testid={aliasesStatus !== 'loading' ? 'edit-models-ready' : undefined}>
        <legend>models</legend>
        <label class="check all" class:off={!allModels && !tiers.mayEnableAllModels}>
          <input
            type="checkbox"
            bind:checked={allModels}
            disabled={busy || (!allModels && !tiers.mayEnableAllModels)}
          />
          all offered models incl. future
        </label>
        {#if aliasesStatus === 'loading'}
          <p class="hint" aria-busy="true">loading the catalog…</p>
        {:else if aliasesStatus === 'error'}
          <p class="field-error" role="status">model catalog unavailable: {aliasesError}</p>
        {:else if offered.length === 0}
          <p class="hint">no model is offered to {scope === '' ? 'the global catalog' : `scope ${scope}`}.</p>
        {:else}
          <div class="model-grid">
            {#each offered as a (a.id)}
              {@const on = allModels || picked.includes(a.id)}
              <label class="check" class:off={!on && !tiers.mayWidenModels}>
                <input
                  type="checkbox"
                  data-alias-id={a.id}
                  checked={on}
                  disabled={busy || allModels || (!on && !tiers.mayWidenModels)}
                  onchange={() => togglePick(a.id)}
                />
                <span class="alias-name">{a.name}</span>
                <span class="chip">{a.kind}</span>
                <span class="chip src">{a.scope === GLOBAL_SCOPE ? 'global' : a.scope}</span>
              </label>
            {/each}
          </div>
        {/if}
        {#if !tiers.mayWidenModels}
          <p class="hint">granting further models needs self-service — removing one is always possible.</p>
        {/if}
      </fieldset>

      <fieldset class="opts">
        <legend>prompt sensitivity</legend>
        {#each tiers.sensitivity as o (o.value)}
          <label class="check" class:off={o.disabled}>
            <input type="radio" name="edit-sensitivity" value={o.value} disabled={busy || o.disabled} bind:group={sensitivity} />
            {o.value}
            {#if o.note}<span class="note">{o.note}</span>{/if}
          </label>
        {/each}
      </fieldset>

      <LaneSelect bind:value={lane} current={consumer.admission_lane} {serverAdmin} peer={consumer.kind === 'ctx-peer'} disabled={busy} />

      <div class="field">
        <label class="check">
          <input type="checkbox" bind:checked={anySource} disabled={busy || (!anySource && !tiers.mayWidenCidrs)} />
          any source
        </label>
        <label class="field">
          <span class="lbl">network allowlist</span>
          <textarea
            rows="2"
            spellcheck="false"
            placeholder="CIDR prefixes, e.g. 10.0.0.0/8, fd00::/8"
            disabled={busy || anySource}
            bind:value={cidrsText}
          ></textarea>
        </label>
        {#if !tiers.mayWidenCidrs}
          <span class="hint">self-service is off: the allowlist can only be narrowed (prefixes inside the stored ones).</span>
        {/if}
      </div>

      {#if blocker}
        <p class="problem" data-testid="edit-blocker">{blocker}</p>
      {/if}
      {#if error}
        <p class="problem" role="alert">{error}</p>
      {/if}
    </div>

    <footer>
      <div class="actions">
        <button type="button" class="ghost" disabled={busy} onclick={close}>Cancel</button>
        <button type="submit" disabled={busy || !('patch' in built)}>{busy ? 'saving…' : 'Save'}</button>
      </div>
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
  .field input,
  .field select,
  .field textarea {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-2);
    background: var(--surface-0);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
  }
  .field textarea {
    resize: vertical;
  }
  .hint {
    margin: 0;
    font-size: var(--fs-xs);
    color: var(--text-dim);
  }
  .field-error {
    font-size: var(--fs-xs);
    color: var(--danger);
  }
  fieldset {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    margin: 0;
    padding: var(--space-2);
    border: 1px solid var(--border);
    border-radius: var(--radius);
  }
  fieldset.opts {
    flex-direction: row;
    flex-wrap: wrap;
    gap: var(--space-1) var(--space-3);
  }
  .model-grid {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
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
  .check.all {
    color: var(--text);
  }
  .alias-name {
    font-weight: var(--fw-semibold);
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
  .note {
    font-family: var(--font-ui);
    font-size: var(--fs-xs);
    color: var(--text-dim);
  }
  .problem {
    margin: 0;
    font-size: var(--fs-xs);
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius);
    border: 1px solid var(--danger);
    /* --text, not --danger: light-theme --danger on --danger-dim over the
       dialog surface computes to ≈ 4.3:1 (< 4.5 AA for small text) */
    color: var(--text);
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
  footer button.ghost {
    background: transparent;
  }
  footer button:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
</style>
