<script lang="ts" module>
  // Theme switcher (design 02-theme.md §6 W-T4, §9 "3-Segment-Radiogroup"):
  // the user-facing control for the light/dark system. Drives the module
  // singleton `themeController` (theme.svelte.ts) — read `pref`, call `set()`.
  // No-FOUC + persistence + matchMedia-resolve all live in the controller; this
  // component is only the input surface.
  //
  // Two render modes, keyed off the NavRail `expanded` prop (mirrors NavRail's
  // own expanded/icon footer buttons): expanded → a 3-segment radiogroup
  // (explicit/discoverable, §9); collapsed icon rail → a single icon-cycle
  // button (system → light → dark → system), the §9 narrow-screen fallback.
  //
  // The pure pref logic (segment order, cycle step, keyboard → next-pref, the
  // set() funnel) lives here in the module script — no DOM — so it is
  // unit-testable in the node-only vitest env (vite.config test.environment),
  // mirroring ConfirmDialog.svelte. The body below is the thin DOM shell.

  import type { ThemePref } from './theme.svelte'

  export interface ThemeOption {
    value: ThemePref
    /** Short visible label inside the expanded segment. */
    label: string
    /** Accessible name for the radio / cycle button (more descriptive). */
    aria: string
  }

  // Segment order — also the icon-cycle order. `system` first: it is the
  // first-time default (§9), then light → dark.
  export const THEME_OPTIONS: readonly ThemeOption[] = [
    { value: 'system', label: 'System', aria: 'System theme (follow OS)' },
    { value: 'light', label: 'Light', aria: 'Light theme' },
    { value: 'dark', label: 'Dark', aria: 'Dark theme' },
  ]

  export const THEME_PREFS: readonly ThemePref[] = THEME_OPTIONS.map((o) => o.value)

  /** The pref `step` positions away, wrapping. Powers both the icon-cycle
   *  button (step +1 per click) and the radiogroup arrow keys (±1). */
  export function cyclePref(current: ThemePref, step = 1): ThemePref {
    const n = THEME_PREFS.length
    const i = THEME_PREFS.indexOf(current)
    const base = i < 0 ? 0 : i
    return THEME_PREFS[(((base + step) % n) + n) % n]
  }

  /** Radiogroup keyboard navigation: the pref a key should move to, or null
   *  when the key is not a navigation key (Space/Enter fall through to the
   *  native button click). Right/Down → next, Left/Up → prev, Home/End → ends. */
  export function nextOnKey(key: string, current: ThemePref): ThemePref | null {
    switch (key) {
      case 'ArrowRight':
      case 'ArrowDown':
        return cyclePref(current, 1)
      case 'ArrowLeft':
      case 'ArrowUp':
        return cyclePref(current, -1)
      case 'Home':
        return THEME_PREFS[0]
      case 'End':
        return THEME_PREFS[THEME_PREFS.length - 1]
      default:
        return null
    }
  }

  /** The set() funnel both the click and the keyboard go through. Kept here
   *  (not inlined) so the "selecting a segment calls set(pref)" contract is
   *  unit-testable against a fake controller without a DOM. */
  export interface ThemeSetter {
    set(pref: ThemePref): void
  }
  export function select(controller: ThemeSetter, pref: ThemePref): void {
    controller.set(pref)
  }
</script>

<script lang="ts">
  import { themeController } from './theme.svelte'

  let {
    expanded = true,
  }: {
    /** Labelled 3-segment radiogroup when true; icon-cycle button when false
     *  (collapsed icon rail). Forwarded from NavRail. */
    expanded?: boolean
  } = $props()

  // Radiogroup container ref — used only imperatively to move focus to the
  // newly-selected radio on arrow keys (roving tabindex). The radios live in
  // DOM/THEME_PREFS order, so the index maps straight across.
  let group = $state<HTMLDivElement>()

  const current = $derived(
    THEME_OPTIONS.find((o) => o.value === themeController.pref) ?? THEME_OPTIONS[0]
  )

  function onKeydown(e: KeyboardEvent): void {
    const next = nextOnKey(e.key, themeController.pref)
    if (next === null) return
    e.preventDefault()
    select(themeController, next)
    const radios = group?.querySelectorAll<HTMLButtonElement>('[role="radio"]')
    radios?.[THEME_PREFS.indexOf(next)]?.focus()
  }
</script>

{#snippet glyph(pref: ThemePref)}
  <svg
    class="glyph"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    stroke-width="1.6"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
  >
    {#if pref === 'system'}
      <rect x="3" y="4.5" width="18" height="12" rx="1.5" /><path d="M8 20h8M12 16.5V20" />
    {:else if pref === 'light'}
      <circle cx="12" cy="12" r="4" /><path
        d="M12 2.5v2M12 19.5v2M2.5 12h2M19.5 12h2M5 5l1.4 1.4M17.6 17.6L19 19M19 5l-1.4 1.4M6.4 17.6L5 19"
      />
    {:else}
      <path d="M20 14.5A8 8 0 1 1 9.5 4a6.5 6.5 0 0 0 10.5 10.5z" />
    {/if}
  </svg>
{/snippet}

{#if expanded}
  <div bind:this={group} class="theme-toggle seg" role="radiogroup" aria-label="Theme">
    {#each THEME_OPTIONS as opt (opt.value)}
      {@const checked = themeController.pref === opt.value}
      <button
        type="button"
        role="radio"
        class="opt"
        class:checked
        aria-checked={checked}
        aria-label={opt.aria}
        tabindex={checked ? 0 : -1}
        title={opt.label}
        onclick={() => select(themeController, opt.value)}
        onkeydown={onKeydown}
      >
        {@render glyph(opt.value)}
        <span class="opt-label">{opt.label}</span>
      </button>
    {/each}
  </div>
{:else}
  <button
    type="button"
    class="theme-toggle cycle"
    aria-label={`Theme: ${current.label} — activate to switch`}
    title={`Theme: ${current.label}`}
    onclick={() => select(themeController, cyclePref(themeController.pref))}
  >
    {@render glyph(current.value)}
  </button>
{/if}

<style>
  .theme-toggle {
    width: 100%;
    box-sizing: border-box;
  }

  .seg {
    display: flex;
    gap: var(--space-1);
  }
  .opt {
    flex: 1 1 0;
    min-width: 0;
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 3px;
    padding: var(--space-1);
    background: transparent;
    border: 1px solid var(--border);
    border-radius: var(--radius);
    color: var(--text-dim);
    font-family: var(--font-mono);
    cursor: pointer;
    transition: color var(--dur-1) var(--ease), background var(--dur-1) var(--ease), border-color var(--dur-1) var(--ease);
  }
  .opt:hover {
    color: var(--text);
    background: var(--surface-2);
  }
  .opt.checked {
    color: var(--accent);
    background: var(--accent-dim);
    border-color: var(--accent);
  }
  .opt-label {
    font-size: var(--label-size);
    letter-spacing: var(--label-tracking);
    text-transform: uppercase;
    line-height: var(--lh-solid);
  }

  .cycle {
    display: flex;
    align-items: center;
    justify-content: center;
    padding: var(--space-2) 0;
    background: transparent;
    border: 1px solid transparent;
    border-radius: var(--radius);
    color: var(--text-dim);
    cursor: pointer;
    transition: color var(--dur-1) var(--ease), background var(--dur-1) var(--ease);
  }
  .cycle:hover {
    color: var(--text);
    background: var(--surface-2);
  }

  .glyph {
    width: 1.05rem;
    height: 1.05rem;
    flex: none;
  }
</style>
