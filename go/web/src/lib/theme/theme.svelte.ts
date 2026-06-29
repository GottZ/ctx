// Theme controller (design 02-theme.md §4 / §6 W-T3): the runes-store side of
// the light/dark system. The render-blocking boot script (public/theme-boot.js)
// owns No-FOUC — it paints `data-theme` before first paint; this controller
// re-syncs that into a reactive store at boot, then drives every later change.
//
// Strategy (§4): localStorage holds the *preference* (system|light|dark); the
// controller resolves `system` to a concrete light|dark via matchMedia and
// ALWAYS writes the concrete value as `data-theme` on <html>. CSS therefore
// only ever sees `[data-theme="light"]` (or the attributeless :root dark
// default) — one source per theme, no media-query/source-order fragility.
//
// Mirrors the class-store pattern of lib/auth.svelte.ts (exported class for
// testing + a module singleton for the app).

export type ThemePref = 'system' | 'light' | 'dark'
export type ResolvedTheme = 'light' | 'dark'

/**
 * Custom-event name fired on `window` AFTER `data-theme` is (re)written. SINGLE
 * SOURCE OF TRUTH — the graph axis (graph-theme / G2) imports this to subscribe
 * (`window.addEventListener(THEME_CHANGE_EVENT, …)`) and re-color its canvas.
 * The dispatch order (attribute first, event second) guarantees a
 * getComputedStyle read in the handler already sees the new `--graph-*` tokens.
 */
export const THEME_CHANGE_EVENT = 'ctx:theme-change'

const STORAGE_KEY = 'ctx.theme'
const DARK_QUERY = '(prefers-color-scheme: dark)'

// Boot/SSR/no-matchMedia fallback is DARK — matches the attributeless :root
// default in tokens.css, so a missing OS signal never flips the app to light.
const FALLBACK_DARK = true

export class ThemeController {
  /** Persisted preference (system|light|dark). `system` tracks the OS. */
  pref = $state<ThemePref>('system')
  /** Concrete theme currently written to <html data-theme>. */
  resolved = $state<ResolvedTheme>('dark')

  // The system-preference media query. Held so the OS listener is attached
  // exactly once and `system` resolves cheaply. Lives for the page lifetime
  // (no teardown — this is a module singleton).
  private mql: MediaQueryList | null = null

  /**
   * Boot-time sync (main.ts, before mount): read the persisted preference,
   * attach the OS-theme listener, and re-apply. No-FOUC stays owned by the boot
   * script; this only mirrors the already-painted state into the runes store.
   */
  init(): void {
    this.pref = readStoredPref()
    this.attachMediaListener()
    this.apply()
  }

  /** User-initiated change (ThemeToggle / W-T4): persist + re-apply. */
  set(pref: ThemePref): void {
    this.pref = pref
    writeStoredPref(pref)
    this.apply()
  }

  // Resolve the preference to a concrete theme, consulting the OS for `system`.
  private resolveTheme(pref: ThemePref): ResolvedTheme {
    if (pref === 'light' || pref === 'dark') return pref
    return this.systemPrefersDark() ? 'dark' : 'light'
  }

  // Write the concrete theme to <html data-theme> and notify listeners. The
  // event MUST fire AFTER the attribute write (graph re-color reads tokens via
  // getComputedStyle in the handler).
  private apply(): void {
    const resolved = this.resolveTheme(this.pref)
    this.resolved = resolved
    if (typeof document !== 'undefined') {
      document.documentElement.setAttribute('data-theme', resolved)
    }
    dispatchThemeChange()
  }

  private systemPrefersDark(): boolean {
    if (this.mql) return this.mql.matches
    // No listener attached yet (set() before init()) — query on demand.
    try {
      return typeof globalThis.matchMedia === 'function'
        ? globalThis.matchMedia(DARK_QUERY).matches
        : FALLBACK_DARK
    } catch {
      return FALLBACK_DARK
    }
  }

  private attachMediaListener(): void {
    if (this.mql) return
    let mql: MediaQueryList | null = null
    try {
      mql = typeof globalThis.matchMedia === 'function' ? globalThis.matchMedia(DARK_QUERY) : null
    } catch {
      mql = null
    }
    if (!mql) return
    this.mql = mql
    // OS theme changed: re-resolve ONLY while tracking the system preference;
    // an explicit light/dark choice stays pinned.
    mql.addEventListener('change', () => {
      if (this.pref === 'system') this.apply()
    })
  }
}

function readStoredPref(): ThemePref {
  try {
    const raw = globalThis.localStorage?.getItem(STORAGE_KEY)
    if (raw === 'light' || raw === 'dark' || raw === 'system') return raw
  } catch {
    // localStorage unavailable (private mode / cookies blocked) → system.
  }
  return 'system'
}

function writeStoredPref(pref: ThemePref): void {
  try {
    globalThis.localStorage?.setItem(STORAGE_KEY, pref)
  } catch {
    // Best-effort persist; the in-memory pref still drives this tab.
  }
}

function dispatchThemeChange(): void {
  if (typeof window === 'undefined' || typeof window.dispatchEvent !== 'function') return
  window.dispatchEvent(new Event(THEME_CHANGE_EVENT))
}

export const themeController = new ThemeController()
