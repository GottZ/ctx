// Pure pref logic for ThemeToggle.svelte (design 02-theme.md §6 W-T4). The
// segment order, the icon-cycle / arrow-key step (cyclePref), the radiogroup
// keyboard map (nextOnKey) and the set() funnel (select) live in the
// component's module script (no DOM) so they unit-test in the node-only vitest
// env (vite.config test.environment), mirroring ConfirmDialog.svelte.test. The
// DOM wiring (roving tabindex, focus, aria-checked render) is the thin shell
// around them and is left to the playwright gate, not exercised here.

import { describe, expect, it } from 'vitest'
import type { ThemePref } from './theme.svelte'
import { THEME_OPTIONS, THEME_PREFS, cyclePref, nextOnKey, select } from './ThemeToggle.svelte'

describe('THEME_OPTIONS / THEME_PREFS', () => {
  it('exposes system → light → dark in that order (system first = default)', () => {
    expect(THEME_PREFS).toEqual(['system', 'light', 'dark'])
    expect(THEME_OPTIONS.map((o) => o.value)).toEqual([...THEME_PREFS])
  })

  it('gives every option a visible label and an accessible name', () => {
    for (const o of THEME_OPTIONS) {
      expect(o.label.length).toBeGreaterThan(0)
      expect(o.aria.length).toBeGreaterThan(0)
    }
  })
})

describe('cyclePref', () => {
  it('cycles forward, wrapping dark → system (icon-cycle click)', () => {
    expect(cyclePref('system')).toBe('light')
    expect(cyclePref('light')).toBe('dark')
    expect(cyclePref('dark')).toBe('system')
  })

  it('cycles backward, wrapping system → dark', () => {
    expect(cyclePref('system', -1)).toBe('dark')
    expect(cyclePref('dark', -1)).toBe('light')
    expect(cyclePref('light', -1)).toBe('system')
  })

  it('treats an unknown current as the first pref', () => {
    expect(cyclePref('bogus' as ThemePref)).toBe('light')
  })
})

describe('nextOnKey — radiogroup keyboard navigation', () => {
  it('moves to the next pref on ArrowRight / ArrowDown', () => {
    expect(nextOnKey('ArrowRight', 'system')).toBe('light')
    expect(nextOnKey('ArrowDown', 'light')).toBe('dark')
    expect(nextOnKey('ArrowRight', 'dark')).toBe('system') // wraps
  })

  it('moves to the previous pref on ArrowLeft / ArrowUp', () => {
    expect(nextOnKey('ArrowLeft', 'light')).toBe('system')
    expect(nextOnKey('ArrowUp', 'system')).toBe('dark') // wraps
  })

  it('jumps to the ends on Home / End', () => {
    expect(nextOnKey('Home', 'dark')).toBe('system')
    expect(nextOnKey('End', 'system')).toBe('dark')
  })

  it('returns null for non-navigation keys (Space/Enter fall through to click)', () => {
    expect(nextOnKey(' ', 'system')).toBeNull()
    expect(nextOnKey('Enter', 'light')).toBeNull()
    expect(nextOnKey('Tab', 'dark')).toBeNull()
  })
})

describe('select — the set() funnel', () => {
  it('forwards the chosen pref to controller.set (segment click / keyboard)', () => {
    const calls: ThemePref[] = []
    const fake = { set: (p: ThemePref) => calls.push(p) }

    select(fake, 'light')
    select(fake, 'dark')
    select(fake, 'system')

    expect(calls).toEqual(['light', 'dark', 'system'])
  })

  it('drives the icon-cycle: select(controller, cyclePref(pref)) sets the next pref', () => {
    const calls: ThemePref[] = []
    const fake = { set: (p: ThemePref) => calls.push(p) }

    select(fake, cyclePref('system'))
    expect(calls).toEqual(['light'])
  })
})
