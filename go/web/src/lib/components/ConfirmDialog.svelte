<script lang="ts" module>
  // Generic confirm primitive (design 04-§3 / §4 wave A2b). Generalises the
  // BackendDialog.svelte:84 confirmStep two-click pattern into a standalone,
  // reusable confirm dialog on top of the shared Modal shell (design 05 §7 Q9 —
  // the native <dialog>+showModal wiring now lives in lib/ui/Modal.svelte).
  // Adds an optional "type the slug" prune gate for irreversible Full-Prune
  // actions (e.g. tenant-delete, §6.4). Consumed by A3/A5/A6 and the Blocks axis.
  //
  // The confirm-step + slug-match logic lives here in the module script (pure,
  // no DOM) so it is unit-testable in the node-only vitest env (design
  // 04-§5.5) — the component body below wires it to the dialog.

  /** Slug-prune gate: the typed value must equal the required slug exactly.
   *  Surrounding whitespace is trimmed (slugs never contain spaces, so a
   *  stray pasted space should not block an otherwise-correct entry). */
  export function slugMatches(required: string, typed: string): boolean {
    return typed.trim() === required
  }

  /** Whether the final confirm action is permitted. No requireTyped (undefined
   *  or empty) → always allowed; with requireTyped → only on an exact slug. */
  export function canConfirm(requireTyped: string | undefined, typed: string): boolean {
    if (!requireTyped) return true
    return slugMatches(requireTyped, typed)
  }

  export type ConfirmDecision = 'arm' | 'commit' | 'blocked'

  /** What a click on the primary button should do:
   *  - benign (no danger, no slug): commit on the first click.
   *  - danger or slug-gated: the FIRST click arms (reveals the final confirm
   *    stage — this is the "two-click" safety); once armed, commit iff the
   *    slug gate passes, otherwise blocked. */
  export function decideConfirm(opts: {
    danger?: boolean
    requireTyped?: string
    armed: boolean
    typed: string
  }): ConfirmDecision {
    const gated = Boolean(opts.danger) || Boolean(opts.requireTyped)
    if (!opts.armed) return gated ? 'arm' : 'commit'
    return canConfirm(opts.requireTyped, opts.typed) ? 'commit' : 'blocked'
  }
</script>

<script lang="ts">
  import { type Snippet } from 'svelte'
  import { toApiError } from '../api'
  import Modal from '../ui/Modal.svelte'

  let {
    title,
    message = '',
    confirmLabel = 'Confirm',
    danger = false,
    requireTyped,
    onconfirm,
    oncancel,
    children,
  }: {
    title: string
    /** Blast-radius / body copy, announced via role=status. Use {children} for
     *  rich content (e.g. a per-scope count list) when plain text is not enough. */
    message?: string
    confirmLabel?: string
    /** Danger-styles the confirm button and forces the two-click arm step. */
    danger?: boolean
    /** When set, the confirm button stays disabled until this exact value is
     *  typed (Full-Prune slug gate, §6.4). Implies a danger confirm. */
    requireTyped?: string
    /** Fired on confirm. May be async; a rejection keeps the dialog open and
     *  surfaces the error (role=alert) so the user can retry. */
    onconfirm?: () => void | Promise<void>
    /** Fired when the dialog is dismissed (Cancel / Esc / backdrop), never on
     *  a successful confirm. */
    oncancel?: () => void
    children?: Snippet
  } = $props()

  let armed = $state(false)
  let typed = $state('')
  let busy = $state(false)
  let error = $state<string | null>(null)

  let dialogEl: HTMLDialogElement | undefined = $state()
  // $state: the focus $effect reads these reactively when `armed` flips, so the
  // ref must be reactive — else focus never moves into the armed/slug field.
  let typedInputEl = $state<HTMLInputElement>()
  let confirmBtnEl = $state<HTMLButtonElement>()

  // Distinguishes a committed close from an Esc/backdrop/Cancel dismissal so
  // the native `close` event only reports oncancel for the latter.
  let committed = false

  const gated = $derived(danger || Boolean(requireTyped))
  const ready = $derived(canConfirm(requireTyped, typed))

  // Stable, instance-unique ids so two dialogs never collide on aria targets.
  const uid = `confirm-${Math.random().toString(36).slice(2, 9)}`
  const titleId = `${uid}-t`
  const msgId = `${uid}-m`

  // On entering the armed (confirm / slug) stage move focus to the tip-field
  // (if any), else to the danger button — a11y per design 04-§3 A2b. The
  // effect runs after the DOM is committed, so the bound refs exist.
  $effect(() => {
    if (!armed) return
    if (requireTyped) typedInputEl?.focus()
    else confirmBtnEl?.focus()
  })

  async function primary(): Promise<void> {
    if (busy) return
    const decision = decideConfirm({ danger, requireTyped, armed, typed })
    if (decision === 'arm') {
      armed = true
      return
    }
    if (decision === 'blocked') return
    await commit()
  }

  async function commit(): Promise<void> {
    if (busy) return
    busy = true
    error = null
    try {
      await onconfirm?.()
      committed = true
      dialogEl?.close()
    } catch (err) {
      // Stay open, keep the typed value, let the user retry the confirm.
      error = toApiError(err).message
    } finally {
      busy = false
    }
  }

  function close(): void {
    if (busy) return
    dialogEl?.close()
  }

  function handleClose(): void {
    if (!committed) oncancel?.()
  }
</script>

<Modal
  bind:dialogEl
  width="32rem"
  dismissable={!busy}
  backdropClose={!busy}
  ariaLabelledby={titleId}
  ariaDescribedby={msgId}
  onclose={handleClose}
>
  <form
    method="dialog"
    onsubmit={(e) => {
      e.preventDefault()
      void primary()
    }}
  >
    <header>
      <h2 id={titleId}>{title}</h2>
      <button type="button" class="x" title="close" disabled={busy} onclick={close}>×</button>
    </header>

    <div class="body">
      <div id={msgId} class="blast" role="status">
        {#if message}<p class="msg" class:warn={gated}>{message}</p>{/if}
        {#if children}{@render children()}{/if}
      </div>

      {#if armed && requireTyped}
        <label class="field">
          <span class="lbl">type <code>{requireTyped}</code> to confirm</span>
          <input
            bind:this={typedInputEl}
            type="text"
            spellcheck="false"
            autocomplete="off"
            disabled={busy}
            bind:value={typed}
          />
        </label>
      {:else if armed}
        <p class="confirm-text">This action cannot be undone. Confirm?</p>
      {/if}

      {#if error}
        <p class="msg error" role="alert">{error}</p>
      {/if}
    </div>

    <footer>
      <div class="actions">
        {#if armed}
          <button type="button" class="ghost" disabled={busy} onclick={() => (armed = false)}>Back</button>
        {:else}
          <button type="button" class="ghost" disabled={busy} onclick={close}>Cancel</button>
        {/if}
        <button
          bind:this={confirmBtnEl}
          type="submit"
          class:danger={gated}
          disabled={busy || (armed && !ready)}
        >
          {busy ? 'working…' : confirmLabel}
        </button>
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
  .blast {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .msg {
    margin: 0;
    font-size: var(--fs-sm);
    line-height: var(--lh-body);
    color: var(--text);
  }
  .msg.warn {
    color: var(--warn);
  }
  .msg.error {
    color: var(--danger);
    border: 1px solid var(--danger);
    background: var(--danger-dim);
    border-radius: var(--radius);
    padding: var(--space-1) var(--space-2);
    font-size: var(--fs-xs);
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
  .field input {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-2);
    background: var(--surface-0);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
  }
  .field input:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  code {
    font-family: var(--font-mono);
    color: var(--accent);
  }
  .confirm-text {
    margin: 0;
    font-size: var(--fs-sm);
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
  button {
    font-family: var(--font-ui);
    font-size: var(--fs-sm);
    padding: var(--space-1) var(--space-3);
    background: var(--surface-2);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    color: var(--text);
    cursor: pointer;
  }
  button:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  button.ghost {
    background: transparent;
  }
  button.danger {
    border-color: var(--danger);
    color: var(--danger);
  }
</style>
