<script lang="ts">
  // Create a model consumer with its first key (design/05 §4.6 component
  // ConsumerCreateDialog, wave W05-06) — built on the shared Modal shell, two
  // views in ONE modal like routes/tenant/KeyCreateDialog.svelte: the form,
  // then — on success — the show-once RevealOnceKey, reused UNCHANGED.
  //
  // Reveal-once hygiene (design/05 §5 B-6, SECURITY-CRITICAL): the plaintext
  // `api_key` exists in the answer of ConsumersModel.create() (which keeps
  // nothing) and is handed to RevealOnceKey, which snapshots it at mount; the
  // dialog then drops its own copy at once (`revealKey = ''`), so after the
  // mount the only copy is RevealOnceKey's local state, wiped on ack. It is
  // never written to the model, the session, sessionStorage/localStorage, the
  // URL or the console, and no error path carries it (a failed create has no
  // plaintext). Esc during the reveal closes the dialog — the unmount drops it.
  //
  // Fields (server = internal/handler/model_consumers.go, authoritative):
  // label*, description, tenant (server-admin, from tenant-list), tenant_scope
  // (from scope-list of the chosen tenant; a tenant-admin is offered its home
  // scope only — Lead decision 1, see mint.ts scopeOptions), models (the offer
  // of the chosen scope — mint.ts offeredAliases mirrors modelalias.Offers;
  // ids on the wire) or "all offered incl. future", expiry (30/90/365 days or a
  // date; "never" only without a lifetime ceiling), prompt sensitivity (lower
  // values server-admin only, §4.5), network allowlist (CIDR form check).
  // There is NO kind control and the spec type has no kind field — the SPA
  // creates app consumers only (peers: /admin/peers, W05-14). Server errors
  // (400 field, 403 tier, 409 label, 429 quota) stay at the dialog.
  //
  // FOLGE-Z Z4 (no silent values, startlimits.ts): the admission lane is a
  // visible, always-sent choice (server-admin: urgent preselected; tenant-admin:
  // bulk + the reason), the "start limits" block shows per op the effective
  // values a new consumer of the TARGET tenant inherits (gateway-limits-get of
  // that tenant, re-read when a server-admin switches the tenant) and sends
  // only the changed fields as `limits`; the reveal shows the answer's
  // effective_limits + lane next to the key — "not reported" when absent.
  import { onMount, tick } from 'svelte'
  import { toApiError } from '../../lib/api'
  import { session } from '../../lib/auth.svelte'
  import { listModelAliases, type PromptSensitivity } from '../../lib/api/modelapi'
  import { getSubjectLimits } from '../../lib/api/gatewaypanels'
  import { listScopes } from '../../lib/api/scopes'
  import type { ExternalLane, GatewayOp, ModelAliasView, Tenant } from '../../lib/api/types'
  import LaneSelect from './LaneSelect.svelte'
  import Modal from '../../lib/ui/Modal.svelte'
  import RevealOnceKey from '../tenant/RevealOnceKey.svelte'
  import type { ConsumersModel } from './consumers.svelte'
  import {
    GLOBAL_SCOPE,
    TEMPLATES,
    buildCreateSpec,
    defaultExpiry,
    expiryOptions,
    offeredAliases,
    scopeOptions,
    sensitivityOptions,
    templateModels,
    type ExpiryChoice,
  } from './mint'
  import {
    EFFECTIVE_FIELDS,
    EFFECTIVE_LABEL,
    START_DIMS,
    START_DIM_LABEL,
    applyTemplateStart,
    buildStartLimits,
    createLaneDefault,
    createLaneHint,
    fieldKey,
    inheritedLines,
    laneShareNote,
    limitsSubject,
    prefillInputs,
    revealLane,
    revealLimits,
    startDefaults,
    startFieldSpec,
    startOps,
    tenantCaps,
    type RevealLimits,
    type StartDefaults,
    type StartInputs,
  } from './startlimits'

  let {
    model,
    tenants,
    baseUrl,
    onclose,
  }: {
    model: ConsumersModel
    /** server-admin: the tenant-list rows (the page loads them for its filter) */
    tenants: Tenant[]
    /** <origin>/v1 — shown beside the reveal, no secret */
    baseUrl: string
    onclose: () => void
  } = $props()

  const serverAdmin = $derived(session.caps.tier === 'server-admin')
  // whoami model_api.key_max_lifetime_s — default AND ceiling of a new app
  // key's expiry (0 = no ceiling); null when the server sends no policy.
  const maxLifetimeS = $derived(session.whoami?.capabilities?.model_api?.key_max_lifetime_s ?? null)

  // --- form state ---
  let label = $state('')
  let description = $state('')
  // server-admin target tenant; the caller's own tenant sends no tenant_id
  // svelte-ignore state_referenced_locally
  let tenantChoice = $state(session.tenantId ?? '')
  let scope = $state('')
  let allModels = $state(false)
  let picked = $state<string[]>([])
  // svelte-ignore state_referenced_locally
  const initialExpiry = defaultExpiry(maxLifetimeS)
  let expiryValue = $state(initialExpiry.kind === 'days' ? String(initialExpiry.days) : 'date')
  let expiryDate = $state('')
  let sensitivity = $state<PromptSensitivity>('credentials')
  let cidrsText = $state('')
  // FOLGE-Z Z4: lane + start limits (startlimits.ts)
  // svelte-ignore state_referenced_locally
  let lane = $state<ExternalLane>(createLaneDefault(session.caps.tier === 'server-admin'))
  let defaults = $state<StartDefaults>({})
  // the dimensions the target tenant's own (tenant_tenant) row caps — a server-admin value above stays capped
  let caps = $state<StartDefaults>({})
  // the inherited values the block does not edit, one line per op
  let inherited = $state<Partial<Record<GatewayOp, string>>>({})
  let inputs = $state<StartInputs>({})
  let startNotes = $state<Record<string, string>>({})
  let limitsStatus = $state<'loading' | 'ready' | 'error'>('loading')
  let limitsError = $state<string | null>(null)
  let limitsSeq = 0

  // --- catalog + scopes ---
  let aliases = $state<ModelAliasView[]>([])
  let aliasesStatus = $state<'loading' | 'ready' | 'error'>('loading')
  let aliasesError = $state<string | null>(null)
  let tenantScopes = $state<string[]>([])
  let scopesStatus = $state<'loading' | 'ready' | 'error'>('loading')
  let scopesError = $state<string | null>(null)
  let scopeSeq = 0

  // --- submit / reveal ---
  let busy = $state(false)
  let error = $state<string | null>(null)
  let errorParam = $state<string | null>(null)
  // FOLGE-Z Z4: the start-limit field a client-side check refused (`<op>.<dim>`)
  let errorField = $state<string | null>(null)
  let revealing = $state(false)
  // Holds the plaintext only between create() and RevealOnceKey's mount.
  let revealKey = $state('')
  let createdLabel = $state('')
  let createdHint = $state('')
  // FOLGE-Z Z4: what the answer says the consumer starts with (no secret)
  let createdLane = $state('')
  let createdLimits = $state<RevealLimits>({ reported: false })

  let dialogEl: HTMLDialogElement | undefined = $state()
  let labelInputEl = $state<HTMLInputElement>()

  const targetTenant = $derived(serverAdmin && tenantChoice !== '' && tenantChoice !== session.tenantId ? tenantChoice : null)
  const tenantRows = $derived(
    tenants.some((t) => t.id === session.tenantId) || session.tenantId === null
      ? tenants
      : [{ id: session.tenantId, slug: session.tenantSlug ?? 'own', display_name: 'your tenant' } as Tenant, ...tenants],
  )
  const scopes = $derived(scopeOptions({ serverAdmin, tenantScopes, homeScope: session.homeScope }))
  const offered = $derived(offeredAliases(aliases, scope === '' ? null : scope))
  const expiryOpts = $derived(expiryOptions(maxLifetimeS))
  const sensOpts = $derived(sensitivityOptions(serverAdmin))
  const ready = $derived(aliasesStatus !== 'loading' && scopesStatus !== 'loading' && limitsStatus !== 'loading')
  const ops = $derived(startOps(offered))
  // the tenant whose effective limits a new consumer inherits (server-admin: the chosen one)
  const limitsTenant = $derived(serverAdmin && tenantChoice !== '' ? tenantChoice : session.tenantId)
  const expiryChoice = $derived<ExpiryChoice>(
    expiryValue === 'date'
      ? { kind: 'date', date: expiryDate }
      : expiryValue === 'never'
        ? { kind: 'never' }
        : { kind: 'days', days: Number(expiryValue) },
  )

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

  async function loadScopes(tenantId: string | null): Promise<void> {
    const seq = ++scopeSeq
    scopesStatus = 'loading'
    scopesError = null
    try {
      const res = await listScopes(tenantId ?? undefined)
      if (seq !== scopeSeq) return
      tenantScopes = res.scopes.map((s) => s.scope)
      scopesStatus = 'ready'
    } catch (err) {
      if (seq !== scopeSeq) return
      tenantScopes = []
      scopesError = toApiError(err).message
      scopesStatus = 'error'
    }
  }

  /** FOLGE-Z Z4: the effective limits of the target tenant ⇒ the prefill (fail-closed: no read ⇒ no values). */
  async function loadLimits(tenantId: string | null): Promise<void> {
    const seq = ++limitsSeq
    const subject = limitsSubject(serverAdmin, tenantId)
    limitsStatus = 'loading'
    limitsError = null
    defaults = {}
    caps = {}
    inherited = {}
    inputs = {}
    startNotes = {}
    if (subject === null) {
      limitsError = 'no tenant known for this key'
      limitsStatus = 'error'
      return
    }
    try {
      const res = await getSubjectLimits(subject)
      if (seq !== limitsSeq) return
      defaults = startDefaults(res)
      caps = tenantCaps(res)
      inherited = inheritedLines(res)
      inputs = prefillInputs(defaults, ['chat', 'embed', 'rerank'])
      limitsStatus = 'ready'
    } catch (err) {
      if (seq !== limitsSeq) return
      limitsError = toApiError(err).message
      limitsStatus = 'error'
    }
  }

  onMount(() => {
    labelInputEl?.focus()
    void loadAliases()
    void loadScopes(null)
    void loadLimits(limitsTenant)
  })

  function changeTenant(value: string): void {
    tenantChoice = value
    // a scope and the picks belong to the previous tenant
    scope = ''
    picked = []
    void loadScopes(targetTenant)
    // the start limits too: the new tenant may carry its own rows
    void loadLimits(limitsTenant)
  }

  function setInput(key: string, value: string): void {
    inputs = { ...inputs, [key]: value }
    if (startNotes[key] !== undefined) {
      const { [key]: _drop, ...rest } = startNotes
      startNotes = rest
    }
  }

  function changeScope(value: string): void {
    scope = value
    // keep only the picks the new scope is offered
    const now = new Set(offeredAliases(aliases, value === '' ? null : value).map((a) => a.id))
    picked = picked.filter((id) => now.has(id))
  }

  function togglePick(id: string): void {
    picked = picked.includes(id) ? picked.filter((x) => x !== id) : [...picked, id]
  }

  function applyTemplate(id: string, kind: string, days: number): void {
    allModels = false
    picked = templateModels(offered, kind)
    const opt = expiryOpts.find((o) => o.value === String(days))
    if (opt && !opt.disabled) expiryValue = opt.value
    // FOLGE-Z Z4: the template's concurrency + RPM (clamped visibly below server-admin)
    if (limitsStatus === 'ready') {
      const t = applyTemplateStart(id, inputs, defaults, serverAdmin)
      inputs = t.inputs
      startNotes = t.notes
    }
  }

  async function submit(): Promise<void> {
    if (busy || revealing) return
    const built = buildCreateSpec({
      label,
      description,
      tenantId: targetTenant,
      scope: scope === '' ? null : scope,
      allModels,
      models: picked,
      offered,
      expiry: expiryChoice,
      maxLifetimeS,
      sensitivity,
      cidrsText,
    })
    errorField = null
    if ('error' in built) {
      error = built.error
      errorParam = null
      return
    }
    const start = buildStartLimits({ ops, defaults, inputs, serverAdmin })
    if ('error' in start) {
      error = start.error
      errorParam = null
      errorField = start.field
      return
    }
    busy = true
    error = null
    errorParam = null
    try {
      const res = await model.create({ ...built.spec, admission_lane: lane, ...(start.limits !== null ? { limits: start.limits } : {}) })
      createdLabel = res.consumer.label
      createdHint = res.consumer.keys[0]?.hint ?? ''
      revealKey = res.api_key
      revealing = true
      // after the key is handed over (review finding 10): nothing here may stand
      // between a committed create and its one plaintext. Every op of the answer
      // — rerank too, even without a rerank alias today (review finding 4).
      createdLane = revealLane(res.consumer.admission_lane)
      createdLimits = revealLimits(res.effective_limits, ['chat', 'embed', 'rerank'])
      // RevealOnceKey snapshots the prop at mount; drop the dialog's copy right after.
      await tick()
      revealKey = ''
    } catch (err) {
      const e = toApiError(err)
      error = e.message
      errorParam = typeof e.details?.param === 'string' ? e.details.param : null
    } finally {
      busy = false
    }
  }

  /** Reveal acknowledged (RevealOnceKey wiped its copy): close; the list already reloaded on create. */
  function finish(): void {
    revealing = false
    revealKey = ''
    dialogEl?.close()
  }

  function close(): void {
    if (busy) return
    dialogEl?.close()
  }

  function handleClose(): void {
    revealing = false
    revealKey = ''
    onclose()
  }
</script>

<Modal bind:dialogEl width="40rem" dismissable={!busy} ariaLabelledby="consumer-create-title" onclose={handleClose}>
  <form
    method="dialog"
    onsubmit={(e) => {
      e.preventDefault()
      if (!revealing) void submit()
    }}
  >
    <header>
      <h2 id="consumer-create-title">{revealing ? 'Consumer created' : 'New model consumer'}</h2>
      <button type="button" class="x" title="close" aria-label="close" disabled={busy} onclick={close}>×</button>
    </header>

    <div class="body">
      {#if revealing}
        <p class="created">
          Consumer <strong>{createdLabel}</strong>{#if createdHint}, key <code>{createdHint}</code>{/if}
        </p>
        <RevealOnceKey apiKey={revealKey} onack={finish} />
        <p class="base-url" data-testid="base-url">
          OpenAI-compatible base URL <code>{baseUrl}</code> — use the key above as the API key.
        </p>
        <section class="effective" data-testid="reveal-limits" aria-label="what the consumer starts with">
          <p class="lane-line" data-testid="reveal-lane">admission lane: <strong>{createdLane}</strong></p>
          {#if !createdLimits.reported}
            <p class="hint">effective limits: not reported by this server — see the consumer's limits panel.</p>
          {:else}
            <table class="eff">
              <caption>effective limits, as the server answered</caption>
              <thead>
                <tr>
                  <th scope="col">limit</th>
                  {#each createdLimits.ops as o (o.op)}<th scope="col">{o.op}</th>{/each}
                </tr>
              </thead>
              <tbody>
                {#each EFFECTIVE_FIELDS as f, i (f)}
                  <tr>
                    <th scope="row">{EFFECTIVE_LABEL[f]}</th>
                    {#each createdLimits.ops as o (o.op)}
                      {#if o.reported}
                        {@const d = o.dims[i]}
                        <td>
                          <span class="v" data-reveal={`${o.op}.${f}`}>{d.text}</span>
                          <span class="src" data-reveal-source={`${o.op}.${f}`}>{d.source}</span>
                        </td>
                      {:else}
                        <td>not reported</td>
                      {/if}
                    {/each}
                  </tr>
                {/each}
              </tbody>
            </table>
          {/if}
        </section>
      {:else}
        <div class="templates" role="group" aria-label="templates">
          <span class="lbl">template</span>
          {#each TEMPLATES as t (t.id)}
            <button type="button" class="chip-btn" disabled={busy || !ready} onclick={() => applyTemplate(t.id, t.kind, t.expiryDays)}>
              {t.label}
            </button>
          {/each}
          <span class="hint">preselects the offered models of that type, a 365-day expiry and start limits (chat 8 / 120 rpm, embedding 4 / 240 rpm)</span>
        </div>

        <label class="field">
          <span class="lbl">label</span>
          <input
            bind:this={labelInputEl}
            type="text"
            spellcheck="false"
            maxlength="100"
            placeholder="who/what calls the models, e.g. ci-pipeline"
            disabled={busy}
            bind:value={label}
          />
        </label>

        <label class="field">
          <span class="lbl">description</span>
          <input type="text" maxlength="1000" placeholder="optional" disabled={busy} bind:value={description} />
        </label>

        {#if serverAdmin}
          <div class="field">
            <label class="lbl" for="cc-tenant">tenant</label>
            <select
              id="cc-tenant"
              aria-describedby="cc-tenant-hint"
              value={tenantChoice}
              disabled={busy}
              onchange={(e) => changeTenant(e.currentTarget.value)}
            >
              {#each tenantRows as t (t.id)}
                <option value={t.id}>{t.slug} — {t.display_name}</option>
              {/each}
            </select>
            <span class="hint" id="cc-tenant-hint">server-admin: the tenant the consumer belongs to.</span>
          </div>
        {/if}

        <div class="field">
          <label class="lbl" for="cc-scope">scope</label>
          <select
            id="cc-scope"
            aria-describedby="cc-scope-hint"
            value={scope}
            disabled={busy || scopesStatus === 'loading'}
            onchange={(e) => changeScope(e.currentTarget.value)}
          >
            <option value="">none — global models only</option>
            {#each scopes as s (s)}
              <option value={s}>{s}</option>
            {/each}
          </select>
          {#if scopesError}
            <span class="field-error" id="cc-scope-hint" role="status">scope list unavailable: {scopesError}</span>
          {:else}
            <span class="hint" id="cc-scope-hint">
              {serverAdmin
                ? 'the scope whose models the consumer is offered besides the global ones.'
                : 'your home scope adds its own models (they take the place of a global model of the same name).'}
            </span>
          {/if}
        </div>

        <fieldset class="models" class:invalid={errorParam === 'models'} data-testid={ready ? 'models-ready' : undefined}>
          <legend>models</legend>
          <label class="check all">
            <input type="checkbox" checked={allModels} disabled={busy} onchange={(e) => (allModels = e.currentTarget.checked)} />
            all offered models incl. future
          </label>
          {#if allModels}
            <p class="warn-line">
              Grants every model this consumer's scope is offered — including models added later.
            </p>
          {/if}
          {#if aliasesStatus === 'loading'}
            <p class="hint" aria-busy="true">loading the model catalog…</p>
          {:else if aliasesStatus === 'error'}
            <p class="field-error" role="status">model catalog unavailable: {aliasesError}</p>
          {:else if offered.length === 0}
            <p class="hint">no model is offered to {scope === '' ? 'the global catalog' : `scope ${scope}`} yet.</p>
          {:else}
            <div class="model-grid">
              {#each offered as a (a.id)}
                <label class="check">
                  <input
                    type="checkbox"
                    data-alias-id={a.id}
                    checked={allModels || picked.includes(a.id)}
                    disabled={busy || allModels}
                    onchange={() => togglePick(a.id)}
                  />
                  <span class="alias-name">{a.name}</span>
                  <span class="chip">{a.kind}</span>
                  <span class="chip src">{a.scope === GLOBAL_SCOPE ? 'global' : a.scope}</span>
                </label>
              {/each}
            </div>
          {/if}
        </fieldset>

        <fieldset class="opts">
          <legend>expires</legend>
          {#each expiryOpts as o (o.value)}
            <label class="check" class:off={o.disabled}>
              <input type="radio" name="expiry" value={o.value} disabled={busy || o.disabled} bind:group={expiryValue} />
              {o.label}
              {#if o.note}<span class="note">{o.note}</span>{/if}
            </label>
          {/each}
          {#if expiryValue === 'date'}
            <input class="date" type="date" aria-label="expiry date" disabled={busy} bind:value={expiryDate} />
          {/if}
          <p class="hint">
            {maxLifetimeS === null
              ? 'the server sets the ceiling.'
              : maxLifetimeS === 0
                ? 'this server sets no ceiling.'
                : `server maximum: ${Math.floor(maxLifetimeS / 86_400)} days.`}
          </p>
        </fieldset>

        <LaneSelect bind:value={lane} current={createLaneDefault(serverAdmin)} {serverAdmin} peer={false} disabled={busy} note={createLaneHint(serverAdmin)} />
        {#if laneShareNote(lane)}<p class="hint lane-share" data-testid="lane-share">{laneShareNote(lane)}</p>{/if}

        <fieldset class="start" data-testid="start-limits">
          <legend>start limits</legend>
          {#if limitsStatus === 'loading'}
            <p class="hint" aria-busy="true">loading the effective limits…</p>
          {:else if limitsStatus === 'error'}
            <p class="field-error" role="status" data-testid="start-limits-error">
              start limits unavailable: {limitsError} — the consumer starts with the server's values; the answer shows them.
            </p>
          {:else}
            {#each ops as op (op)}
              <div class="start-op" role="group" aria-label={`${op} start limits`}>
                <span class="op-name">{op}</span>
                {#each START_DIMS as dim (dim)}
                  {@const spec = startFieldSpec(defaults, op, dim, serverAdmin)}
                  {@const key = fieldKey(op, dim)}
                  {@const cap = serverAdmin ? caps[op]?.[dim] : undefined}
                  {@const note = startNotes[key] ?? (typeof cap === 'number' ? `the tenant's own row caps this at ${cap.toLocaleString('en-US')}` : undefined)}
                  <label class="start-field">
                    <span class="dim">{START_DIM_LABEL[dim]}</span>
                    <input
                      type="text"
                      inputmode="numeric"
                      spellcheck="false"
                      aria-label={`${op} ${START_DIM_LABEL[dim]}`}
                      aria-invalid={errorParam === `limits.${key}` || errorField === key ? 'true' : undefined}
                      aria-describedby={note ? `cc-note-${op}-${dim}` : undefined}
                      placeholder={spec.placeholder}
                      disabled={busy || spec.disabled}
                      value={inputs[key] ?? ''}
                      oninput={(e) => setInput(key, e.currentTarget.value)}
                    />
                    {#if note}<span class="note" id={`cc-note-${op}-${dim}`} data-start-note={key}>{note}</span>{/if}
                  </label>
                {/each}
                {#if inherited[op]}<p class="inherited" data-start-inherited={op}>also inherited: {inherited[op]}</p>{/if}
              </div>
            {/each}
            <p class="hint">
              {serverAdmin
                ? 'prefilled with what a new consumer of this tenant inherits; a changed value is stored on the consumer.'
                : 'prefilled with what a new consumer of your tenant inherits; you can lower a value — raising is server-admin only.'}
            </p>
          {/if}
        </fieldset>

        <fieldset class="opts">
          <legend>prompt sensitivity</legend>
          {#each sensOpts as o (o.value)}
            <label class="check" class:off={o.disabled}>
              <input type="radio" name="sensitivity" value={o.value} disabled={busy || o.disabled} bind:group={sensitivity} />
              {o.value}
              {#if o.note}<span class="note">{o.note}</span>{/if}
            </label>
          {/each}
          <p class="hint">how sensitive this consumer's prompts are taken to be — decides which backends may serve them.</p>
        </fieldset>

        <label class="field">
          <span class="lbl">network allowlist</span>
          <textarea
            rows="2"
            spellcheck="false"
            placeholder="optional — CIDR prefixes, e.g. 10.0.0.0/8, fd00::/8; empty = any source"
            disabled={busy}
            bind:value={cidrsText}
          ></textarea>
        </label>

        {#if error}
          <p class="problem" role="alert">{error}</p>
        {/if}
      {/if}
    </div>

    {#if !revealing}
      <footer>
        <div class="actions">
          <button type="button" class="ghost" disabled={busy} onclick={close}>Cancel</button>
          <button type="submit" disabled={busy || !ready}>{busy ? 'creating…' : 'Create consumer'}</button>
        </div>
      </footer>
    {/if}
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
  .field textarea,
  .date {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-2);
    background: var(--surface-0);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
  }
  .date {
    align-self: flex-start;
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
  .templates {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
  }
  .chip-btn {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    min-height: 24px;
    padding: 0 var(--space-2);
    background: var(--surface-2);
    color: var(--text);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    cursor: pointer;
  }
  .chip-btn:hover:not(:disabled) {
    border-color: var(--accent);
    color: var(--accent);
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
  fieldset.opts .hint {
    flex-basis: 100%;
  }
  fieldset.invalid {
    border-color: var(--danger);
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
  .warn-line {
    margin: 0;
    font-size: var(--fs-xs);
    padding: var(--space-1) var(--space-2);
    border: 1px solid var(--warn);
    border-radius: var(--radius);
    color: var(--warn);
  }
  .problem {
    margin: 0;
    font-size: var(--fs-xs);
    padding: var(--space-1) var(--space-2);
    border-radius: var(--radius);
    border: 1px solid var(--danger);
    /* --text, not --danger (as ConsumerEditDialog/RotateDialog): light-theme
       --danger on --danger-dim over the dialog surface measured 4.3:1 by axe
       (RC-D S3 probe; < 4.5 AA for small text) */
    color: var(--text);
    background: var(--danger-dim);
  }
  .created {
    margin: 0;
    font-size: var(--fs-sm);
    color: var(--text);
  }
  .created code,
  .base-url code {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
  }
  .base-url {
    margin: 0;
    font-size: var(--fs-sm);
    color: var(--text-dim);
  }
  .base-url code {
    color: var(--text);
  }
  .effective {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .lane-line {
    margin: 0;
    font-size: var(--fs-sm);
    color: var(--text);
  }
  .eff {
    border-collapse: collapse;
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
  }
  .eff caption {
    text-align: left;
    color: var(--text-dim);
    padding-bottom: var(--space-1);
  }
  .eff th,
  .eff td {
    text-align: left;
    padding: 0 var(--space-2) var(--space-1) 0;
    vertical-align: top;
  }
  .eff thead th {
    color: var(--text-dim);
    font-weight: var(--fw-medium);
  }
  .eff .v {
    display: block;
    color: var(--text);
  }
  .eff .src {
    display: block;
    color: var(--text-dim);
  }
  fieldset.start {
    gap: var(--space-2);
  }
  .start-op {
    display: grid;
    grid-template-columns: 4rem repeat(4, minmax(0, 1fr));
    gap: var(--space-1) var(--space-2);
    align-items: start;
  }
  .op-name {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    font-weight: var(--fw-semibold);
    padding-top: var(--space-3);
  }
  .start-field {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }
  .start-field .dim {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    color: var(--text-dim);
  }
  .start-field input {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-2);
    background: var(--surface-0);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
    min-width: 0;
  }
  .inherited {
    grid-column: 2 / -1;
    margin: 0;
    font-size: var(--fs-xs);
    color: var(--text-dim);
  }
  .lane-share {
    margin-top: calc(-1 * var(--space-2));
  }
  .start-field input:disabled {
    opacity: 0.55;
    cursor: not-allowed;
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
  footer button:disabled,
  .chip-btn:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
</style>
