// Pins the theme controller (design 02-theme.md §4/§6 W-T3): pref→resolved
// mapping, localStorage persistence, the OS-media listener firing ONLY while
// pref==='system', and THEME_CHANGE_EVENT dispatching AFTER the data-theme
// write. Env is `node` (vite.config) — document/matchMedia/localStorage/window
// are stubbed per test, mirroring graph-theme.test.ts / breakpoints.test.ts.

import { afterEach, describe, expect, it, vi } from 'vitest'
import { THEME_CHANGE_EVENT, ThemeController } from './theme.svelte'

afterEach(() => vi.unstubAllGlobals())

/** Stub <html> with a minimal getAttribute/setAttribute element. */
function stubDocument(): { getAttribute: (k: string) => string | null } {
  const attrs: Record<string, string> = {}
  const el = {
    setAttribute: (k: string, v: string) => {
      attrs[k] = v
    },
    getAttribute: (k: string) => (k in attrs ? attrs[k] : null),
  }
  vi.stubGlobal('document', { documentElement: el })
  return el
}

/** Stub matchMedia returning one mutable MQL; `setDark`/`fire` drive OS changes. */
function stubMatchMedia(dark: boolean) {
  const listeners = new Set<() => void>()
  const mql = {
    matches: dark,
    media: '(prefers-color-scheme: dark)',
    addEventListener: (_: string, h: () => void) => listeners.add(h),
    removeEventListener: (_: string, h: () => void) => listeners.delete(h),
  }
  vi.stubGlobal('matchMedia', () => mql)
  return {
    setDark: (v: boolean) => {
      mql.matches = v
    },
    fire: () => listeners.forEach((h) => h()),
  }
}

/** Stub localStorage backed by an in-memory record (returned for assertions). */
function stubLocalStorage(initial: Record<string, string> = {}): Record<string, string> {
  const data: Record<string, string> = { ...initial }
  vi.stubGlobal('localStorage', {
    getItem: (k: string) => (k in data ? data[k] : null),
    setItem: (k: string, v: string) => {
      data[k] = v
    },
    removeItem: (k: string) => {
      delete data[k]
    },
  })
  return data
}

/** Stub window as a real EventTarget so dispatch/listen round-trips. */
function stubWindow(): EventTarget {
  const bus = new EventTarget()
  vi.stubGlobal('window', bus)
  return bus
}

describe('THEME_CHANGE_EVENT', () => {
  it('is the canonical single-source event name', () => {
    expect(THEME_CHANGE_EVENT).toBe('ctx:theme-change')
  })
})

describe('ThemeController.init / set — pref → resolved', () => {
  it('defaults to system and resolves via matchMedia (OS dark → dark)', () => {
    const el = stubDocument()
    stubMatchMedia(true)
    stubLocalStorage()
    stubWindow()

    const c = new ThemeController()
    c.init()

    expect(c.pref).toBe('system')
    expect(c.resolved).toBe('dark')
    expect(el.getAttribute('data-theme')).toBe('dark')
  })

  it('system follows the OS to light when matchMedia reports light', () => {
    const el = stubDocument()
    stubMatchMedia(false)
    stubLocalStorage()
    stubWindow()

    const c = new ThemeController()
    c.init()

    expect(c.resolved).toBe('light')
    expect(el.getAttribute('data-theme')).toBe('light')
  })

  it('explicit light/dark resolve literally and write data-theme', () => {
    const el = stubDocument()
    stubMatchMedia(true) // OS dark — must be ignored when pinned
    stubLocalStorage()
    stubWindow()

    const c = new ThemeController()
    c.init()

    c.set('light')
    expect(c.resolved).toBe('light')
    expect(el.getAttribute('data-theme')).toBe('light')

    c.set('dark')
    expect(c.resolved).toBe('dark')
    expect(el.getAttribute('data-theme')).toBe('dark')

    c.set('system') // back to OS (dark)
    expect(c.resolved).toBe('dark')
  })
})

describe('ThemeController — localStorage persistence', () => {
  it('reads the persisted pref on init', () => {
    const el = stubDocument()
    stubMatchMedia(true)
    stubLocalStorage({ 'ctx.theme': 'light' })
    stubWindow()

    const c = new ThemeController()
    c.init()

    expect(c.pref).toBe('light')
    expect(c.resolved).toBe('light')
    expect(el.getAttribute('data-theme')).toBe('light')
  })

  it('persists the chosen pref under ctx.theme on set()', () => {
    stubDocument()
    stubMatchMedia(true)
    const store = stubLocalStorage()
    stubWindow()

    const c = new ThemeController()
    c.init()

    c.set('light')
    expect(store['ctx.theme']).toBe('light')
    c.set('dark')
    expect(store['ctx.theme']).toBe('dark')
  })

  it('falls back to system and stays in-memory when localStorage throws', () => {
    stubDocument()
    stubMatchMedia(true)
    stubWindow()
    vi.stubGlobal('localStorage', {
      getItem: () => {
        throw new Error('SecurityError: localStorage blocked')
      },
      setItem: () => {
        throw new Error('SecurityError: localStorage blocked')
      },
      removeItem: () => {},
    })

    const c = new ThemeController()
    expect(() => c.init()).not.toThrow()
    expect(c.pref).toBe('system')

    expect(() => c.set('light')).not.toThrow()
    expect(c.pref).toBe('light') // in-memory pref still updates
    expect(c.resolved).toBe('light')
  })
})

describe('ThemeController — matchMedia change', () => {
  it('follows OS changes only while pref is system', () => {
    const el = stubDocument()
    const mm = stubMatchMedia(true) // OS dark
    stubLocalStorage()
    stubWindow()

    const c = new ThemeController()
    c.init()
    expect(c.resolved).toBe('dark')

    // OS flips to light, pref still system → follow.
    mm.setDark(false)
    mm.fire()
    expect(c.resolved).toBe('light')
    expect(el.getAttribute('data-theme')).toBe('light')

    // User pins dark; later OS flips must NOT change the resolved theme.
    c.set('dark')
    expect(c.resolved).toBe('dark')
    mm.setDark(false) // OS = light
    mm.fire()
    expect(c.resolved).toBe('dark')
    expect(el.getAttribute('data-theme')).toBe('dark')
  })
})

describe('ThemeController — THEME_CHANGE_EVENT', () => {
  it('fires AFTER the data-theme attribute is written', () => {
    const el = stubDocument()
    stubMatchMedia(true)
    stubLocalStorage({ 'ctx.theme': 'light' })
    const bus = stubWindow()

    let attrAtDispatch: string | null = '<not-fired>'
    bus.addEventListener(THEME_CHANGE_EVENT, () => {
      attrAtDispatch = el.getAttribute('data-theme')
    })

    const c = new ThemeController()
    c.init()

    // The handler observed the already-written 'light' → event is post-write.
    expect(attrAtDispatch).toBe('light')
    expect(el.getAttribute('data-theme')).toBe('light')
  })

  it('re-fires on each set() and on a system-driven OS change', () => {
    stubDocument()
    const mm = stubMatchMedia(true)
    stubLocalStorage()
    const bus = stubWindow()

    let fired = 0
    bus.addEventListener(THEME_CHANGE_EVENT, () => {
      fired += 1
    })

    const c = new ThemeController()
    c.init() // 1 — initial apply
    c.set('light') // 2
    mm.setDark(false)
    mm.fire() // pref is 'light' (pinned) → NO re-apply, NO event
    expect(fired).toBe(2)

    c.set('system') // 3
    mm.fire() // pref is 'system' → re-apply → event
    expect(fired).toBe(4)
  })
})
