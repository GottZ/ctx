<script lang="ts">
  // Alias catalog /gateway/models (design/05 §3.3, §4.6 ModelsPage; wave
  // W05-13 — RC-D C5). Heading "Model catalog"; the table of
  // AliasesModel.aliases (scope, name, kind, role, sensitivity, enabled) —
  // write controls (Edit, Enable/Disable) only where catalog.rowControls
  // allows: the server's `writable` AND, below server-admin, a scope other
  // than `_global` (two independent locks, inv05 D8). "+ New alias" opens
  // AliasEditorDialog (name / kind free, scope per tier, sensitivity per
  // catalog.aliasSensitivityOptions, AliasPolicyForm, enabled explicit);
  // "Edit alias <name>" opens it with name and kind read-only; lowering the
  // sensitivity confirms first (inside the editor). Server-admin only:
  // "Suggest from pool" (drafts, enabled:false) and per row "Preview chain of
  // <name>" (AliasPreviewPanel, C7 shape: alias by name through one consumer).
  // A tenant-admin never sees either and never fires model-alias-preview.
  //
  // The route inherits the /gateway prefix guard (caps.viewModelApi); the page
  // self-gates on the same cap (defense in depth, precedent GatewayPage). The
  // editor and the preview are their own lazy chunks (they load on first use).
  //
  // Row order: `_global` first, then the tenant scopes; inside a scope the
  // live (enabled) aliases first, then the disabled ones (drafts, retired
  // names), each by name — the operator's working set stays on top.
  import { untrack } from 'svelte'
  import { session } from '../../lib/auth.svelte'
  import { toApiError } from '../../lib/api'
  import type { ModelAliasView } from '../../lib/api/types'
  import Table from '../../lib/ui/Table.svelte'
  import { AliasesModel } from './aliases.svelte'
  import { rowControls } from './catalog'
  import { aliasVisionBadge } from './vision'

  const model = new AliasesModel()

  $effect(() => {
    if (!session.caps.viewModelApi) return
    untrack(() => void model.load())
  })

  const serverAdmin = $derived(session.caps.tier === 'server-admin')

  function byCatalogOrder(a: ModelAliasView, b: ModelAliasView): number {
    const ga = a.scope === '_global' ? 0 : 1
    const gb = b.scope === '_global' ? 0 : 1
    if (ga !== gb) return ga - gb
    if (a.scope !== b.scope) return a.scope < b.scope ? -1 : 1
    if (a.enabled !== b.enabled) return a.enabled ? -1 : 1
    return a.name < b.name ? -1 : a.name > b.name ? 1 : 0
  }
  const rows = $derived([...model.aliases].sort(byCatalogOrder))

  // --- editor (lazy chunk) ---
  type Editing = { row: ModelAliasView | null; seq: number }
  let editing = $state<Editing | null>(null)
  let editSeq = 0
  const loadEditor = () => import('./AliasEditorDialog.svelte')
  function openEditor(row: ModelAliasView | null): void {
    toast = null
    pageError = null
    editing = { row, seq: ++editSeq }
  }

  // --- preview (server-admin, lazy chunk) ---
  let previewRow = $state<ModelAliasView | null>(null)
  const loadPreview = () => import('./AliasPreviewPanel.svelte')

  // --- page-level outcome lines ---
  let toast = $state<string | null>(null)
  let pageError = $state<string | null>(null)

  async function toggle(row: ModelAliasView): Promise<void> {
    toast = null
    pageError = null
    try {
      await model.update(row.id, { enabled: !row.enabled })
      toast = `Alias ${row.name} (${row.scope}) ${row.enabled ? 'disabled — it is offered to no consumer' : 'enabled'}.`
    } catch (err) {
      pageError = toApiError(err).message
    }
  }

  async function suggest(): Promise<void> {
    toast = null
    pageError = null
    try {
      const out = await model.suggestFromPool()
      const made = out.created.map((a) => a.name)
      toast =
        made.length === 0
          ? `The pool offers no new alias (skipped: ${out.skipped.join(', ') || 'none'}).`
          : `Drafted ${made.join(', ')} as disabled _global aliases — review and enable them.${out.skipped.length > 0 ? ` Skipped: ${out.skipped.join(', ')}.` : ''}`
    } catch (err) {
      pageError = toApiError(err).message
    }
  }
</script>

<section class="area" aria-labelledby="models-title">
  <header>
    <h1 id="models-title">Model catalog</h1>
    <p class="sub">
      The model names external callers send to the OpenAI-compatible endpoint — each alias maps a name to a backend
      role, with a sensitivity floor and a policy. A tenant alias takes the place of a <code>_global</code> alias of
      the same name for that tenant.
    </p>
  </header>

  {#if session.caps.tier === 'loading'}
    <p class="state" aria-busy="true">resolving session…</p>
  {:else if !session.caps.viewModelApi}
    <p class="banner" role="alert">The model catalog needs a tenant-admin key while the gateway is switched on.</p>
  {:else}
    <div class="page-actions">
      {#if serverAdmin}
        <button type="button" disabled={model.busyId !== null || model.status !== 'ready'} onclick={() => void suggest()}>
          {model.busyId === '' ? 'working…' : 'Suggest from pool'}
        </button>
      {/if}
      <button type="button" class="primary" disabled={model.status === 'error'} onclick={() => openEditor(null)}>
        + New alias
      </button>
    </div>

    {#if toast}
      <div class="toast" role="status" data-testid="models-toast">
        <span>{toast}</span>
        <button type="button" class="x" aria-label="dismiss" onclick={() => (toast = null)}>×</button>
      </div>
    {/if}
    {#if pageError}
      <p class="action-error" role="alert">{pageError}</p>
    {/if}

    {#if model.status === 'idle' || model.status === 'loading'}
      <p class="state" aria-busy="true">loading the catalog…</p>
    {:else if model.status === 'error'}
      <div class="error" role="alert">
        <p>{model.loadError?.message ?? 'failed to load the model catalog'}</p>
        {#if model.loadError?.requestId}<p class="request-id">request {model.loadError.requestId}</p>{/if}
        <button type="button" onclick={() => void model.load()}>Retry</button>
      </div>
    {:else if rows.length === 0}
      <div class="empty" role="status">
        <p class="empty-title">No model alias yet</p>
        <p class="empty-hint">
          External callers can only name a model through an alias. Create one with “+ New alias”{serverAdmin
            ? ' or draft them from the pool with “Suggest from pool”'
            : ''}.
        </p>
      </div>
    {:else}
      <section class="card" aria-label="model aliases">
        <header class="card-head">
          <h2>aliases</h2>
          <span class="count">{rows.length}</span>
        </header>
        <Table label="model aliases" focusableScroll>
          {#snippet head()}
            <tr>
              <th>scope</th>
              <th>name</th>
              <th>kind</th>
              <th>role</th>
              <th>sensitivity</th>
              <th>status</th>
              <th>actions</th>
            </tr>
          {/snippet}
          {#each rows as a (a.id)}
            {@const ctl = rowControls(a, serverAdmin)}
            {@const busy = model.busyId === a.id}
            {@const vision = aliasVisionBadge(a)}
            <tr data-alias-row class:off={!a.enabled}>
              <td><span class="scope" data-alias-scope={a.scope}>{a.scope}</span></td>
              <td>
                <span class="name" data-alias-name>{a.name}</span>
                {#if a.description}<span class="desc">{a.description}</span>{/if}
              </td>
              <td>
                <span class="chip">{a.kind}</span>
                <!-- FOLGE-Z Z5: whether the alias accepts images, and why (effective_vision + vision_source) -->
                {#if vision}<span class="vision-chip" class:on={vision.tone === 'on'} title={vision.title} data-alias-vision>{vision.text}</span>{/if}
              </td>
              <td class="mono">{a.role}</td>
              <td class="mono">{a.sensitivity}</td>
              <td><span class="state-chip" class:on={a.enabled} data-alias-enabled>{a.enabled ? 'enabled' : 'disabled'}</span></td>
              <td class="actions">
                {#if ctl.edit}
                  <button type="button" aria-label={`Edit alias ${a.name}`} disabled={busy} onclick={() => openEditor(a)}>
                    Edit
                  </button>
                {/if}
                {#if ctl.toggle}
                  <button
                    type="button"
                    aria-label={`${a.enabled ? 'Disable' : 'Enable'} alias ${a.name}`}
                    disabled={busy}
                    onclick={() => void toggle(a)}
                  >
                    {a.enabled ? 'Disable' : 'Enable'}
                  </button>
                {/if}
                {#if serverAdmin}
                  <button type="button" aria-label={`Preview chain of ${a.name}`} onclick={() => (previewRow = a)}>
                    Preview
                  </button>
                {/if}
                {#if !ctl.edit && !ctl.toggle && !serverAdmin}
                  <span class="ro">read-only</span>
                {/if}
              </td>
            </tr>
          {/each}
        </Table>
      </section>
    {/if}

    {#if serverAdmin && previewRow !== null}
      {#key previewRow.id}
        {#await loadPreview()}
          <p class="state" role="status" aria-busy="true">loading the chain preview…</p>
        {:then { default: PreviewPanel }}
          {#if previewRow}
            <PreviewPanel {model} row={previewRow} onclose={() => (previewRow = null)} />
          {/if}
        {:catch err}
          <p class="action-error" role="alert">chain preview unavailable: {err instanceof Error ? err.message : String(err)}</p>
        {/await}
      {/key}
    {/if}
  {/if}
</section>

{#if editing}
  {#key editing.seq}
    {#await loadEditor()}
      <p class="sr-status" role="status" aria-busy="true">loading the alias editor…</p>
    {:then { default: Editor }}
      <Editor
        {model}
        row={editing.row}
        {serverAdmin}
        homeScope={session.homeScope}
        ondone={(m) => (toast = m)}
        onclose={() => (editing = null)}
      />
    {:catch err}
      <p class="action-error" role="alert">alias editor unavailable: {err instanceof Error ? err.message : String(err)}</p>
    {/await}
  {/key}
{/if}

<style>
  .area {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }
  header {
    border-bottom: 1px solid var(--border);
    padding-bottom: var(--space-2);
  }
  h1 {
    margin: 0;
    font-size: var(--fs-xl);
    font-weight: var(--fw-semibold);
    letter-spacing: var(--track-1);
  }
  .sub {
    margin: var(--space-1) 0 0;
    color: var(--text-dim);
    font-size: var(--fs-sm);
  }
  code {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
  }
  .banner {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--warn);
    border-radius: var(--radius);
    color: var(--warn);
    font-size: var(--fs-sm);
  }
  .state {
    margin: 0;
    color: var(--text-faint);
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
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
    font-size: var(--fs-sm);
  }
  .error p {
    margin: 0;
    /* body text on the tint: danger-on-danger-dim misses AA in the light theme */
    color: var(--text);
  }
  .request-id {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    color: var(--text-dim) !important;
  }
  .action-error {
    margin: 0;
    color: var(--danger);
    font-size: var(--fs-sm);
  }
  button {
    min-height: 24px;
    font-family: var(--font-ui);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-3);
    background: var(--surface-2);
    color: var(--text);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    cursor: pointer;
  }
  button:hover:not(:disabled) {
    border-color: var(--accent);
    color: var(--accent);
  }
  button:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  button.primary {
    border-color: var(--accent);
    color: var(--accent);
  }
  .page-actions {
    display: flex;
    justify-content: flex-end;
    flex-wrap: wrap;
    gap: var(--space-2);
  }
  .toast {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    background: var(--surface-1);
    color: var(--text);
    font-size: var(--fs-sm);
  }
  .toast .x {
    min-height: 24px;
    min-width: 24px;
    border: none;
    background: transparent;
    color: var(--text-dim);
    padding: 0 var(--space-1);
    font-size: var(--fs-md);
  }
  .empty {
    border: 1px dashed var(--border-strong);
    border-radius: var(--radius);
    padding: var(--space-3);
    background: var(--surface-1);
  }
  .empty-title {
    margin: 0;
    color: var(--text);
    font-size: var(--fs-md);
    font-weight: var(--fw-semibold);
  }
  .empty-hint {
    margin: var(--space-1) 0 0;
    color: var(--text-dim);
    font-size: var(--fs-sm);
  }
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
    font-size: var(--fs-md);
    font-weight: var(--fw-semibold);
  }
  .count {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    color: var(--text-dim);
  }
  .scope,
  .mono {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
  }
  .scope {
    color: var(--text-dim);
  }
  .name {
    display: block;
    font-family: var(--font-mono);
    font-weight: var(--fw-semibold);
  }
  .desc {
    display: block;
    color: var(--text-dim);
    font-size: var(--fs-xs);
  }
  tr.off .name {
    color: var(--text-dim);
  }
  .chip,
  .state-chip {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    color: var(--text-dim);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
    white-space: nowrap;
  }
  .vision-chip {
    display: inline-block;
    margin-top: 2px;
    font-family: var(--font-mono);
    font-size: var(--label-size);
    color: var(--text-dim);
    border: 1px dashed var(--border-strong);
    border-radius: var(--radius);
    padding: 0 var(--space-1);
    white-space: nowrap;
  }
  .vision-chip.on {
    color: var(--text);
    border-style: solid;
  }
  .state-chip.on {
    color: var(--ok);
    border-color: var(--ok);
  }
  .actions {
    white-space: nowrap;
  }
  .actions button + button {
    margin-left: var(--space-1);
  }
  .ro {
    color: var(--text-faint);
    font-size: var(--fs-xs);
  }
  .sr-status {
    position: absolute;
    width: 1px;
    height: 1px;
    overflow: hidden;
    clip-path: inset(50%);
    white-space: nowrap;
  }
</style>
