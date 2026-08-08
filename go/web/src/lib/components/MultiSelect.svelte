<script lang="ts" module>
  /** Trigger summary: 'all' when every option is on, else 'on/total'. */
  export function summarize(on: number, total: number): string {
    return total > 0 && on >= total ? 'all' : `${on}/${total}`
  }
</script>

<script lang="ts">
  import type { Snippet } from 'svelte'

  // Compact multi-select: a trigger button (label + selection summary) opening
  // a checkbox popover. Controlled like the graph FilterPanel it was built
  // for — checked state lives with the caller, every interaction emits a
  // callback. The popover deliberately stays OPEN across toggles (multi-select
  // needs consecutive clicks); outside-pointerdown and Escape close it.
  let {
    label,
    options,
    checked,
    ontoggle,
    onall,
    onnone,
    ononly,
    option,
  }: {
    label: string
    options: string[]
    checked: (opt: string) => boolean
    ontoggle: (opt: string) => void
    /** Quick action "all" — omit to hide the button. */
    onall?: () => void
    /** Quick action "none" — omit to hide the button (an allowlist whose
     *  empty state means "all" has no representable none). */
    onnone?: () => void
    /** Per-row "only" isolation — omit to hide the buttons. */
    ononly?: (opt: string) => void
    /** Row label content (swatches etc.); defaults to the option text. */
    option?: Snippet<[string]>
  } = $props()

  let open = $state(false)
  let rootEl = $state<HTMLElement>()
  let triggerEl = $state<HTMLButtonElement>()

  const onCount = $derived(options.filter((o) => checked(o)).length)
  const filtered = $derived(onCount < options.length)

  function onDocPointerdown(e: PointerEvent): void {
    if (open && rootEl && !rootEl.contains(e.target as Node)) open = false
  }

  // Escape closes and returns focus to the trigger. stopPropagation keeps the
  // window-close Escape (FloatingWindow container) out of this interaction.
  function onKeydown(e: KeyboardEvent): void {
    if (e.key !== 'Escape' || !open) return
    e.stopPropagation()
    open = false
    triggerEl?.focus()
  }
</script>

<svelte:document onpointerdown={onDocPointerdown} />

<!-- svelte-ignore a11y_no_static_element_interactions — keydown delegation
     wrapper (same pattern as SearchBox): the interactive elements inside are
     native buttons/checkboxes. -->
<div class="ms" bind:this={rootEl} onkeydown={onKeydown}>
  <button
    type="button"
    class="trigger"
    class:filtered
    aria-expanded={open}
    bind:this={triggerEl}
    onclick={() => (open = !open)}
  >
    <span class="lbl">{label}</span>
    <span class="sum">{summarize(onCount, options.length)}</span>
    <span class="caret" aria-hidden="true">▾</span>
  </button>

  {#if open}
    <div class="pop" role="group" aria-label={label}>
      {#if onall || onnone}
        <div class="quick">
          {#if onall}<button type="button" onclick={onall}>all</button>{/if}
          {#if onnone}<button type="button" onclick={onnone}>none</button>{/if}
        </div>
      {/if}
      {#each options as opt (opt)}
        <div class="row">
          <label class="check">
            <input type="checkbox" checked={checked(opt)} onchange={() => ontoggle(opt)} />
            {#if option}{@render option(opt)}{:else}{opt}{/if}
          </label>
          {#if ononly}
            <button type="button" class="only" aria-label="only {opt}" onclick={() => ononly(opt)}>only</button>
          {/if}
        </div>
      {/each}
    </div>
  {/if}
</div>

<style>
  .ms {
    position: relative;
    display: inline-flex;
  }

  /* Trigger mirrors the graph chrome buttons (.back in GraphPage): bordered
     surface-2, full --text for contrast; the mono-uppercase label keeps the
     former fieldset-legend look. */
  .trigger {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    min-height: 24px; /* SC 2.5.8 target-size — the focus-stage axe matrix enforces it */
    border: 1px solid var(--border);
    border-radius: var(--radius);
    background: var(--surface-2);
    color: var(--text);
    cursor: pointer;
    font-size: var(--fs-xs);
    padding: 0.15rem 0.55rem;
  }
  .trigger:hover {
    border-color: var(--text-dim);
  }
  .trigger.filtered {
    border-color: var(--accent);
  }
  .lbl {
    font-family: var(--font-mono);
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
  }
  .sum {
    font-family: var(--font-mono);
  }
  .trigger.filtered .sum {
    color: var(--accent);
  }
  .caret {
    font-size: var(--fs-2xs);
  }

  .pop {
    position: absolute;
    top: calc(100% + var(--space-1));
    left: 0;
    z-index: var(--z-popover);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    min-width: max-content;
    max-height: 20rem;
    overflow-y: auto;
    border: 1px solid var(--border);
    border-radius: var(--radius);
    background: var(--surface-1);
    box-shadow: var(--shadow-1);
    padding: var(--space-2);
  }

  .quick {
    display: flex;
    gap: var(--space-2);
    padding-bottom: var(--space-1);
    border-bottom: 1px solid var(--border);
  }
  .quick button {
    min-height: 24px;
    font-size: var(--fs-xs);
    padding: 0 var(--space-2);
  }

  .row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .row .check {
    flex: 1;
  }
  /* The "only" isolation stays permanently visible — a hover-only reveal
     (opacity 0) would leave a focusable-but-invisible control, exactly the
     kind of state the focus-stage axe matrix exists to catch. */
  .only {
    min-height: 24px;
    font-size: var(--fs-2xs);
    padding: 0 var(--space-1);
  }

  .check {
    display: inline-flex;
    align-items: center;
    gap: 0.3rem;
    min-height: 24px; /* target-size floor for the checkbox rows */
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    color: var(--text-dim);
    cursor: pointer;
  }
  .check input {
    accent-color: var(--accent);
    margin: 0;
  }
</style>
