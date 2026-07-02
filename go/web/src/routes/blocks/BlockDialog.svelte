<script lang="ts">
  // Block create/edit dialog (block-workbench W4, design plan §W4). Native
  // <dialog>+showModal (no UI lib — matches the rest of the frontend). On edit
  // only the changed fields are PATCHed (blockDiff in the model). A sensitivity
  // DOWNGRADE needs an explicit in-dialog confirm step → confirm_sensitivity_-
  // downgrade:true; the server stays authoritative (it 400s without it, the
  // exact analogue of the BackendDialog trust-elevation step). The save flow +
  // field errors live in the injected BlockEditModel; this component stays thin.
  import { onMount, untrack } from 'svelte'
  import { isSensitivityDowngrade, SENSITIVITY_LEVELS, type BlockDraft } from '../../lib/blocks/edit'
  import type { BlockEditModel, EditMode } from './edit.svelte'
  import { session } from '../../lib/auth.svelte'

  let {
    mode,
    model,
    /** edit only: the full block UUID (the server resolves ids in HomeScope only). */
    id = '',
    /** Seed values for the form (edit: the block being edited; create: blank/defaults). */
    initial,
    onclose,
  }: {
    mode: EditMode
    model: BlockEditModel
    id?: string
    initial?: Partial<BlockDraft>
    onclose: () => void
  } = $props()

  const homeScope = session.whoami?.home_scope ?? ''
  // Scope options = home_scope, plus 'shared' when the key is granted it
  // (read_scopes mirrors the server's AllowedScopes gate; the server 403s an
  // ungranted scope on write). A foreign-scope block is never editable here.
  const scopeOptions = (() => {
    const opts = [homeScope]
    if ((session.whoami?.read_scopes ?? []).includes('shared') && homeScope !== 'shared') opts.push('shared')
    return opts.filter((s) => s !== '')
  })()

  // The dialog is mounted fresh per open (modal — the page behind is inert), so
  // the props are a one-shot snapshot; untrack reads them as initial values for
  // the editable draft without Svelte flagging a missed reactive capture.
  const init = untrack<BlockDraft>(() => ({
    category: initial?.category ?? '',
    title: initial?.title ?? '',
    content: initial?.content ?? '',
    tags: initial?.tags ? [...initial.tags] : [],
    scope: initial?.scope ?? homeScope,
    sensitivity: initial?.sensitivity ?? '',
  }))
  // The pre-edit snapshot the model diffs against (edit) / detects a downgrade against.
  const original: BlockDraft = untrack(() => ({ ...init }))

  let category = $state(init.category)
  let title = $state(init.title)
  let content = $state(init.content)
  let tagInput = $state(init.tags.join(', '))
  let scope = $state(init.scope)
  let sensitivity = $state(init.sensitivity)

  let saving = $state(false)

  function collectDraft(): BlockDraft {
    return {
      category: category.trim(),
      title: title.trim(),
      content,
      tags: tagInput
        .split(',')
        .map((t) => t.trim())
        .filter((t) => t !== ''),
      scope,
      sensitivity,
    }
  }

  // A lowering of the sensitivity rank — the server demands a confirm flag, so
  // the dialog shows an in-dialog confirm step BEFORE the first send (edit only;
  // a create has no prior sensitivity to lower).
  const downgrade = $derived(mode === 'edit' && isSensitivityDowngrade(original.sensitivity, sensitivity))

  function clientError(): string | null {
    if (category.trim() === '') return 'category is required'
    if (title.trim() === '') return 'title is required'
    if (content.trim() === '') return 'content is required'
    return null
  }
  let clientErr = $state<string | null>(null)

  let dialogEl: HTMLDialogElement
  onMount(() => {
    model.reset()
    dialogEl.showModal()
  })

  async function commit(): Promise<void> {
    clientErr = clientError()
    if (clientErr) return
    // First click on a downgrading edit → show the confirm step, do not send.
    // (The model also flips needsConfirm on the server's 400 — this is the
    // proactive client-side gate so the user confirms before the round-trip.)
    if (downgrade && !model.needsConfirm) {
      model.needsConfirm = true
      return
    }
    saving = true
    try {
      const ok = await model.save({ mode, draft: collectDraft(), id, original })
      // A second 400-driven needsConfirm (e.g. the rank changed since open) is
      // surfaced as the confirm step; otherwise a success closes the dialog.
      if (ok) dialogEl.close()
    } finally {
      saving = false
    }
  }

  function backToForm(): void {
    model.needsConfirm = false
  }
</script>

<dialog bind:this={dialogEl} onclose={onclose} class="block-dialog">
  <form
    method="dialog"
    onsubmit={(e) => {
      e.preventDefault()
      void commit()
    }}
  >
    <header>
      <h2>{mode === 'create' ? 'New block' : 'Edit block'}</h2>
      <button type="button" class="x" title="close" onclick={() => dialogEl.close()}>×</button>
    </header>

    <div class="body">
      <div class="two">
        <label class="field">
          <span class="lbl">category</span>
          <input type="text" spellcheck="false" placeholder="e.g. learnings" disabled={saving} bind:value={category} />
        </label>
        <label class="field narrow">
          <span class="lbl">scope</span>
          <select disabled={saving || scopeOptions.length <= 1} bind:value={scope}>
            {#each scopeOptions as s (s)}<option value={s}>{s}</option>{/each}
          </select>
        </label>
      </div>

      <label class="field">
        <span class="lbl">title</span>
        <input type="text" spellcheck="false" placeholder="precise, upsert key with category+scope" disabled={saving} bind:value={title} />
      </label>

      <label class="field">
        <span class="lbl">content</span>
        <textarea rows="10" spellcheck="false" placeholder="block body (markdown)" disabled={saving} bind:value={content}></textarea>
      </label>

      <label class="field">
        <span class="lbl">tags</span>
        <input
          type="text"
          spellcheck="false"
          placeholder="comma-separated"
          disabled={saving}
          value={tagInput}
          oninput={(e) => (tagInput = e.currentTarget.value)}
        />
      </label>

      <label class="field">
        <span class="lbl">sensitivity</span>
        <select disabled={saving} bind:value={sensitivity}>
          <option value="">(server default)</option>
          {#each SENSITIVITY_LEVELS as lvl (lvl.value)}<option value={lvl.value} title={lvl.tip}>{lvl.value}</option>{/each}
        </select>
        <span class="hint">
          {sensitivity === '' ? 'leave to pool default' : SENSITIVITY_LEVELS.find((l) => l.value === sensitivity)?.tip}
        </span>
      </label>

      {#if downgrade}
        <p class="problem warn" role="status">
          lowering sensitivity {original.sensitivity} → {sensitivity} opens this block to lower-trust backends —
          you'll confirm on save. Enforcement is server-side and fail-closed.
        </p>
      {/if}

      {#if clientErr}
        <p class="problem error" role="alert">{clientErr}</p>
      {:else if model.error && !model.needsConfirm && model.fieldErrors.length === 0}
        <p class="problem error" role="alert">{model.error}</p>
      {/if}
      {#each model.fieldErrors as fe (fe.field)}
        <p class="problem error" role="alert"><code>{fe.field}</code>: {fe.message}</p>
      {/each}
    </div>

    <footer>
      {#if model.needsConfirm}
        <p class="confirm-text">
          sensitivity decides which backends may see this block. Confirm the downgrade to {sensitivity}?
        </p>
        <div class="actions">
          <button type="button" class="ghost" disabled={saving} onclick={backToForm}>Back</button>
          <button type="submit" class="danger" disabled={saving}>{saving ? 'saving…' : 'Confirm & save'}</button>
        </div>
      {:else}
        <div class="actions">
          <button type="button" class="ghost" disabled={saving} onclick={() => dialogEl.close()}>Cancel</button>
          <button type="submit" disabled={saving}>{saving ? 'saving…' : mode === 'create' ? 'Create' : 'Save'}</button>
        </div>
      {/if}
    </footer>
  </form>
</dialog>

<style>
  .block-dialog {
    width: min(46rem, calc(100vw - 2rem));
    max-height: calc(100dvh - 4rem);
    padding: 0;
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    background: var(--surface-1);
    color: var(--text);
  }
  .block-dialog::backdrop {
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
    flex: 0 1 11rem;
  }
  .lbl {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-faint);
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
    line-height: var(--lh-body);
  }
  .field input:disabled,
  .field select:disabled,
  .field textarea:disabled {
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
