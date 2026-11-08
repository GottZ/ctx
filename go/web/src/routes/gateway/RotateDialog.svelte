<script lang="ts">
  // Rotate a consumer's key (design/05 §4.6 RotateDialog, §5 B-6/B-20; wave
  // W05-08 — RC-D C5). Required question "reason": "Leak / suspicion — revoke
  // the old key now" is PRESELECTED (rotate.defaultRotateChoice = grace 0: the
  // predecessor stops at once); "Planned change" offers the grace window
  // (rotate.graceOptions: 1 h / 24 h / 7 d, preselected = whoami
  // model_api.rotation_grace_s clamped to 7 d) during which both keys work.
  // Submit sends rotate.rotateSpec (grace always on the wire) through
  // model.rotate, then the SECOND reveal-once path: RevealOnceKey (unchanged)
  // snapshots the new plaintext at mount, the dialog drops its own copy right
  // after tick(), ack or Esc wipes it — never in the model, storage, URL or
  // console. A refusal (403 peer / no self-service, 409 inactive, 400 grace)
  // stays in the dialog as role="alert".
  import { onMount, tick } from 'svelte'
  import { toApiError } from '../../lib/api'
  import type { ConsumerView } from '../../lib/api/types'
  import Modal from '../../lib/ui/Modal.svelte'
  import RevealOnceKey from '../tenant/RevealOnceKey.svelte'
  import type { ConsumersModel } from './consumers.svelte'
  import { defaultRotateChoice, graceOptions, rotateSpec, type RotateReason } from './rotate'

  interface Props {
    model: ConsumersModel
    consumer: ConsumerView
    /** whoami model_api.rotation_grace_s (the planned-change default). */
    rotationGraceS: number | null
    onclose: () => void
  }
  let { model, consumer, rotationGraceS, onclose }: Props = $props()

  // svelte-ignore state_referenced_locally
  const grace = graceOptions(rotationGraceS)
  const preset = defaultRotateChoice()
  let reason = $state<RotateReason>(preset.reason)
  let graceValue = $state(String(grace.preselected))

  let busy = $state(false)
  let error = $state<string | null>(null)
  let revealing = $state(false)
  // Holds the plaintext only between rotate() and RevealOnceKey's mount.
  let revealKey = $state('')
  let newHint = $state('')
  let dialogEl: HTMLDialogElement | undefined = $state()
  let firstRadio = $state<HTMLInputElement>()

  onMount(() => firstRadio?.focus())

  async function submit(): Promise<void> {
    if (busy || revealing) return
    busy = true
    error = null
    try {
      const res = await model.rotate(rotateSpec(consumer.id, { reason, graceS: reason === 'leak' ? 0 : Number(graceValue) }))
      // the plaintext first: an answer without `key` must not lose it (review #10)
      revealKey = res.api_key
      newHint = res.key?.hint ?? ''
      revealing = true
      // RevealOnceKey snapshots the prop at mount; drop the dialog's copy right after.
      await tick()
      revealKey = ''
    } catch (err) {
      error = toApiError(err).message
    } finally {
      busy = false
    }
  }

  /** Reveal acknowledged (RevealOnceKey wiped its copy): close. */
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

<Modal bind:dialogEl width="36rem" dismissable={!busy} ariaLabelledby="consumer-rotate-title" onclose={handleClose}>
  <form
    method="dialog"
    onsubmit={(e) => {
      e.preventDefault()
      if (!revealing) void submit()
    }}
  >
    <header>
      <h2 id="consumer-rotate-title">Rotate key — {consumer.label}</h2>
      <button type="button" class="x" title="close" aria-label="close" disabled={busy} onclick={close}>×</button>
    </header>

    <div class="body">
      {#if revealing}
        <p class="created">
          New key{#if newHint} <code>{newHint}</code>{/if} for <strong>{consumer.label}</strong>
          {reason === 'leak' ? '— the old key is revoked.' : '— the old key keeps working until its grace period ends.'}
        </p>
        <RevealOnceKey apiKey={revealKey} onack={finish} />
      {:else}
        <fieldset class="reason">
          <legend>reason</legend>
          <label class="check">
            <input bind:this={firstRadio} type="radio" name="rotate-reason" value="leak" disabled={busy} bind:group={reason} />
            Leak / suspicion — revoke the old key now
          </label>
          <label class="check">
            <input type="radio" name="rotate-reason" value="planned" disabled={busy} bind:group={reason} />
            Planned change
          </label>
        </fieldset>
        {#if reason === 'planned'}
          <div class="field">
            <label class="lbl" for="rot-grace">grace period</label>
            <select id="rot-grace" aria-describedby="rot-grace-hint" bind:value={graceValue} disabled={busy}>
              {#each grace.options as o (o.seconds)}
                <option value={String(o.seconds)}>{o.label}</option>
              {/each}
            </select>
            <span class="hint" id="rot-grace-hint">both keys work until the grace period ends; the old key stops then.</span>
          </div>
        {:else}
          <p class="hint">the old key stops working the moment the new one is issued.</p>
        {/if}
        {#if error}
          <p class="problem" role="alert">{error}</p>
        {/if}
      {/if}
    </div>

    {#if !revealing}
      <footer>
        <div class="actions">
          <button type="button" class="ghost" disabled={busy} onclick={close}>Cancel</button>
          <button type="submit" disabled={busy}>{busy ? 'rotating…' : 'Rotate key'}</button>
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
  .field select {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-2);
    background: var(--surface-0);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
  }
  .hint {
    margin: 0;
    font-size: var(--fs-xs);
    color: var(--text-dim);
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
  .created {
    margin: 0;
    font-size: var(--fs-sm);
    color: var(--text);
  }
  .created code {
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
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
