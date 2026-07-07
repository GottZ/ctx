<script lang="ts">
  // Role-blackout Klartext-Zwischenschritt (092, design/01 §5.1/§5.5/§4.6).
  // Shared by ProfilesCard (U01-W6) and the StatusPage toggles (U01-W7): a
  // profile activation that would take a role fully dark must be confirmed via a
  // DOM alertdialog (never native confirm()), with full focus management — the
  // fail-closed a11y contract for keyboard/screenreader users. Extracted into
  // ONE component so the confirm markup + focus-trap + styles live in a single
  // shared chunk instead of duplicated per page (K4d budget: keeps the always-
  // loaded StatusPage chunk lean). AM-7: the copy says „Eject"/Profil-Label,
  // never „gaming".
  let {
    label,
    roles,
    embedDegraded = false,
    trigger,
    onconfirm,
    oncancel,
  }: {
    /** Display label of the profile being activated (shown in the lead line). */
    label: string
    /** Roles that activation takes fully dark (embed handled separately). */
    roles: string[]
    /** Whether embedding degrades (a plain sentence, not a raw role name). */
    embedDegraded?: boolean
    /** The switch/checkbox that opened the confirm — focus returns here on close. */
    trigger: HTMLElement | null
    /** „Trotzdem aktivieren" — the caller writes the activation with confirm. */
    onconfirm: () => void
    /** Cancel / Escape — the caller tears the dialog down (unmount → focus back). */
    oncancel: () => void
  } = $props()

  /**
   * Focus-managed alertdialog trap (§5.5): moves focus onto the confirm button
   * (the LAST button), traps Tab between the two actions, Escape cancels, and on
   * teardown returns focus to the trigger that opened it. use:action so the
   * wiring lives with the node's lifetime — no manual mount/unmount bookkeeping.
   */
  function blackoutTrap(node: HTMLElement, triggerEl: HTMLElement | null) {
    const buttons = (): HTMLButtonElement[] => Array.from(node.querySelectorAll('button'))
    buttons().at(-1)?.focus()
    function onKey(e: KeyboardEvent): void {
      if (e.key === 'Escape') {
        e.preventDefault()
        oncancel()
        return
      }
      if (e.key !== 'Tab') return
      const b = buttons()
      if (b.length === 0) return
      const first = b[0]
      const last = b[b.length - 1]
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault()
        last.focus()
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault()
        first.focus()
      }
    }
    node.addEventListener('keydown', onKey)
    return {
      destroy(): void {
        node.removeEventListener('keydown', onKey)
        triggerEl?.focus()
      },
    }
  }
</script>

<div
  class="blackout"
  role="alertdialog"
  aria-live="assertive"
  aria-label="Rollen-Blackout bestätigen"
  use:blackoutTrap={trigger}
>
  <p class="bo-lead">„{label}" aktivieren nimmt diese Rollen vollständig vom Netz:</p>
  {#if roles.length > 0}
    <ul class="bo-roles">{#each roles as r (r)}<li>{r}</li>{/each}</ul>
  {/if}
  {#if embedDegraded}
    <p class="bo-embed">Embedding fällt aus — neue Blöcke bleiben nur per Volltext auffindbar (kein Datenverlust).</p>
  {/if}
  <div class="bo-actions">
    <button type="button" class="ghost" onclick={oncancel}>abbrechen</button>
    <button type="button" class="danger" onclick={onconfirm}>Trotzdem aktivieren</button>
  </div>
</div>

<style>
  .blackout {
    margin-top: var(--space-1);
    border: 1px solid var(--danger);
    border-radius: var(--radius);
    background: var(--danger-dim);
    padding: var(--space-2);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .bo-lead {
    margin: 0;
    font-size: var(--fs-xs);
    color: var(--danger);
  }
  .bo-roles {
    margin: 0;
    padding-left: var(--space-3);
    font-family: var(--font-mono);
    font-size: var(--fs-xs);
    color: var(--danger);
  }
  .bo-embed {
    margin: 0;
    font-size: var(--fs-2xs);
    color: var(--warn);
  }
  .bo-actions {
    display: flex;
    justify-content: flex-end;
    gap: var(--space-2);
    margin-top: var(--space-1);
  }
  .ghost {
    font-size: var(--fs-2xs);
    padding: 0 var(--space-1);
    background: transparent;
  }
  button.danger {
    border-color: var(--danger);
    color: var(--danger);
    font-size: var(--fs-2xs);
    padding: 0 var(--space-1);
  }
</style>
