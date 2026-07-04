<script lang="ts">
  // Type-registry create/edit form (design 04 §4.7, wave U10) — built on the
  // shared Modal shell (Q9), the settings widgetFor precedent for the declarative
  // policy fields. ALL form MECHANICS (field seed, wholesale-replace-safe write
  // spec, the 422-draft error contract, the effect hints) live in the pure
  // types-admin.svelte.ts module (vitest-covered); this component only binds them.
  //
  // Builtin types (design §4.7): the KEY is locked (URL identity) and there is no
  // delete here (the list disables it), but the POLICY fields stay editable for
  // the server-admin — a builtin's shipped defaults can be tuned, not removed.
  //
  // The 422-DRAFT invariant (the U10 gate): a write error NEVER closes the modal
  // and NEVER resets a field. submitErrorFrom classifies it (422/400 → at-field,
  // else a banner); the bound $state fields keep the user's input verbatim.
  import { onMount, untrack } from 'svelte'
  import type { BlockTypeView } from '../../../lib/api/types'
  import { putType } from '../../../lib/api/types-registry'
  import Modal from '../../../lib/ui/Modal.svelte'
  import {
    PARENT_MODES,
    RETRIEVAL_POLICIES,
    effectHint,
    emptyFields,
    fieldsFromType,
    isBuiltin,
    submitErrorFrom,
    toWriteSpec,
    type SubmitError,
  } from './types-admin.svelte'

  let { type, onclose, onsaved }: { type: BlockTypeView | null; onclose: () => void; onsaved: () => void } =
    $props()

  // Seed the fields ONCE from the edited row (or blank on create) — the modal is
  // remounted per open ({#if formOpen}), so the initial snapshot is exactly right;
  // untrack silences the reactive-capture lint (the intent is a one-shot seed).
  // Kept as one $state object so a write error leaves EVERY field untouched.
  let f = $state(untrack(() => (type === null ? emptyFields() : fieldsFromType(type))))
  const builtin = $derived(type !== null && isBuiltin(type))
  const hints = $derived(effectHint(f))

  let busy = $state(false)
  let err = $state<SubmitError | null>(null)

  let dialogEl: HTMLDialogElement | undefined = $state()
  let firstInputEl = $state<HTMLInputElement>()
  onMount(() => firstInputEl?.focus())

  async function submit(): Promise<void> {
    if (busy) return
    const name = f.name.trim()
    if (name === '') {
      err = { message: 'a type key is required', kind: 'field', keepOpen: true }
      return
    }
    let spec
    try {
      spec = toWriteSpec(f) // may throw on a bad numeric field — surfaced at-field
    } catch (e) {
      err = submitErrorFrom(e)
      return
    }
    busy = true
    err = null
    try {
      await putType(name, spec)
      // Success is the ONLY path that closes + refreshes; anything else keeps the draft.
      onsaved()
      dialogEl?.close()
    } catch (e) {
      err = submitErrorFrom(e)
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
  width="38rem"
  dismissable={!busy}
  backdropClose={!busy}
  ariaLabelledby="type-form-title"
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
      <h2 id="type-form-title">{type === null ? 'New type' : `Edit ${f.name}`}</h2>
      <button type="button" class="x" title="close" disabled={busy} onclick={close}>×</button>
    </header>

    <div class="body">
      {#if builtin}
        <p class="note" role="note">
          Builtin type — the key and deletion are locked (operator-managed default). Policy fields
          below stay editable; the server is the final gate.
        </p>
      {/if}

      <label class="field">
        <span class="lbl">key</span>
        <input
          bind:this={firstInputEl}
          type="text"
          spellcheck="false"
          autocapitalize="off"
          autocomplete="off"
          placeholder="e.g. sprint"
          disabled={busy || f.isEdit}
          bind:value={f.name}
        />
        <span class="hint">the immutable type identifier — cannot be changed after creation</span>
      </label>

      <label class="field">
        <span class="lbl">display name</span>
        <input type="text" spellcheck="false" disabled={busy} bind:value={f.displayName} />
      </label>

      <label class="field">
        <span class="lbl">description</span>
        <input type="text" spellcheck="false" disabled={busy} bind:value={f.description} />
      </label>

      <fieldset class="group">
        <legend>retrieval</legend>
        <label class="field">
          <span class="lbl">policy</span>
          <select disabled={busy} bind:value={f.retrievalPolicy}>
            {#each RETRIEVAL_POLICIES as p (p)}<option value={p}>{p}</option>{/each}
          </select>
        </label>
        <label class="field" class:muted={f.retrievalPolicy !== 'damped'}>
          <span class="lbl">damping factor</span>
          <input
            type="text"
            inputmode="decimal"
            placeholder="0–1, blank = default"
            disabled={busy}
            bind:value={f.dampingFactor}
          />
        </label>
      </fieldset>

      <fieldset class="group">
        <legend>guard</legend>
        <label class="check"><input type="checkbox" disabled={busy} bind:checked={f.guardCheck} /> duplicate check</label>
        <label class="check"><input type="checkbox" disabled={busy} bind:checked={f.guardCandidate} /> candidate scan</label>
        <label class="field">
          <span class="lbl">threshold duplicate</span>
          <input type="text" inputmode="decimal" placeholder="0–1, blank = default" disabled={busy} bind:value={f.thresholdDuplicate} />
        </label>
        <label class="field">
          <span class="lbl">threshold review</span>
          <input type="text" inputmode="decimal" placeholder="0–1, blank = default" disabled={busy} bind:value={f.thresholdReview} />
        </label>
      </fieldset>

      <fieldset class="group">
        <legend>indexing</legend>
        <label class="check"><input type="checkbox" disabled={busy} bind:checked={f.dreamLinkable} /> dream-linkable</label>
        <label class="check"><input type="checkbox" disabled={busy} bind:checked={f.digestInclude} /> include in digest</label>
        <label class="check"><input type="checkbox" disabled={busy} bind:checked={f.overviewInclude} /> include in overview</label>
      </fieldset>

      <fieldset class="group">
        <legend>structural parent</legend>
        <label class="field">
          <span class="lbl">mode</span>
          <select disabled={busy} bind:value={f.parentMode}>
            {#each PARENT_MODES as m (m)}<option value={m}>{m}</option>{/each}
          </select>
        </label>
      </fieldset>

      {#if hints.length > 0}
        <ul class="effects" aria-label="policy effects">
          {#each hints as h (h)}<li>{h}</li>{/each}
        </ul>
      {/if}

      {#if err}
        <p class="problem" class:field-error={err.kind === 'field'} role="alert">{err.message}</p>
      {/if}
    </div>

    <footer>
      <div class="actions">
        <button type="button" class="ghost" disabled={busy} onclick={close}>Cancel</button>
        <button type="submit" disabled={busy}>{busy ? 'saving…' : 'Save type'}</button>
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
  .note {
    margin: 0;
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text-dim);
    font-size: var(--fs-xs);
    line-height: var(--lh-body);
  }
  .field {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .field.muted {
    opacity: 0.6;
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
  .group {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    margin: 0;
    border: 1px solid var(--border);
    border-radius: var(--radius);
    padding: var(--space-2) var(--space-3) var(--space-3);
  }
  legend {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    color: var(--text-dim);
    padding: 0 var(--space-1);
  }
  .check {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    font-size: var(--fs-sm);
    color: var(--text-dim);
  }
  .effects {
    margin: 0;
    padding-left: var(--space-3);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    font-size: var(--fs-xs);
    color: var(--text-dim);
  }
  .hint {
    font-size: var(--fs-2xs);
    color: var(--text-faint);
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
  .problem.field-error {
    border-color: var(--warn);
    color: var(--warn);
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
