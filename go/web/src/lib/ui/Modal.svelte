<script lang="ts">
  // Shared modal dialog primitive (design 05 sec.7 Q9). ONE place owns the native
  // dialog+showModal shell that six call sites duplicated (KeyCreateDialog,
  // BlockDialog, BackendDialog, BlockDetail's delete gate, TenantCreateDialog,
  // ConfirmDialog): the browser top-layer, the focus-trap and the Esc->cancel
  // handling all come from showModal() -- this primitive is the single caller, so
  // a regression to a non-modal .show() (no trap, no Esc-close) surfaces once in
  // the Q9 e2e rot-probe instead of six times. The backdrop scrim reads the
  // --backdrop token (Q3 consolidated the value; Q9 consolidates the structure).
  //
  // Behaviour is a faithful superset of the six originals, parametrised so each
  // keeps its exact dismissal contract (verhaltensneutral). Esc and backdrop are
  // DECOUPLED because the create dialogs guarded them differently (Esc suppressed
  // only while busy, backdrop suppressed while busy OR on the reveal-once view):
  //   - dismissable=false suppresses Esc (the busy guard).
  //   - backdropClose is the FULL click-outside-to-dismiss condition the caller
  //     computes (Confirm: !busy ; Tenant/Key: !busy && created===null ; the form
  //     dialogs: never). Independent of dismissable by design.
  //
  // The dialog ELEMENT is exposed as a $bindable prop so callers keep their
  // imperative dialogEl.close() on a successful commit -- the least-diff seam, and
  // it types cleanly as HTMLDialogElement (Svelte has no committed precedent for
  // component-instance bind:this in this repo).
  //
  // The box chrome (border, radius, surface, max-height) lived identically in all
  // six style blocks -- it lives here now; width is the only per-caller variable,
  // set inline as pure geometry (inline-gate-legal: no colour/typo property in the
  // attribute channel, design 05 sec.4.4 -- the same lane FloatingWindow's geometry
  // uses). NO --z token: a modal dialog renders in the browser top layer, ABOVE
  // the whole --z-* scale (tokens.css:141 says as much) -- z-index is inert here.
  // NO persistence: the primitive adds no localStorage/sessionStorage (design 05
  // UI-State-Invariante -- only ctx.theme + ctx.nav-rail persist, neither here).
  import { onMount, type Snippet } from 'svelte'

  let {
    /** Intrinsic width e.g. 32rem; capped as min(width, calc(100vw - 2rem)). */
    width = '32rem',
    /** Esc guard: when false the native Esc-cancel is suppressed (busy guard). */
    dismissable = true,
    /** Full click-outside-to-dismiss condition (caller-computed). Default false. */
    backdropClose = false,
    ariaLabelledby,
    ariaDescribedby,
    /** Native close event: Esc, backdrop-click and dialogEl.close(). */
    onclose,
    /** The dialog element, exposed so the caller can close() it imperatively. */
    dialogEl = $bindable(),
    children,
  }: {
    width?: string
    dismissable?: boolean
    backdropClose?: boolean
    ariaLabelledby?: string
    ariaDescribedby?: string
    onclose?: () => void
    dialogEl?: HTMLDialogElement
    children: Snippet
  } = $props()

  // showModal (NOT show): the modal top-layer gives the focus-trap + the
  // Esc-cancel event the whole primitive rests on.
  onMount(() => dialogEl?.showModal())

  // Viewport-capped width, the identical rule the six dialogs held inline. Built
  // as a string so it is a single geometry style:width directive (no colour/typo
  // property in the attribute channel -> inline-gate-legal, design 05 sec.4.4).
  const widthCss = $derived(`min(${width}, calc(100vw - 2rem))`)
</script>

<dialog
  bind:this={dialogEl}
  class="modal"
  style:width={widthCss}
  aria-labelledby={ariaLabelledby}
  aria-describedby={ariaDescribedby}
  {onclose}
  oncancel={(e) => {
    if (!dismissable) e.preventDefault()
  }}
  onclick={(e) => {
    if (backdropClose && e.target === dialogEl) dialogEl?.close()
  }}
>
  {@render children()}
</dialog>

<style>
  .modal {
    max-height: calc(100dvh - 4rem);
    padding: 0;
    border: 1px solid var(--border-strong);
    border-radius: var(--radius);
    background: var(--surface-1);
    color: var(--text);
  }
  .modal::backdrop {
    background: var(--backdrop);
  }
</style>
