// SettingsUi unit tests: collapse default + persistence round-trip, the
// search filter (fuzzy over key/description/value) with score ordering, and
// the corrupted-storage fallback.

import { describe, expect, it } from 'vitest'
import type { SettingView } from '../../lib/api/types'
import { SettingsUi, searchFields, type KVStorage } from './ui.svelte'

function memStorage(seed: Record<string, string> = {}): KVStorage & { data: Record<string, string> } {
  const data = { ...seed }
  return {
    data,
    getItem: (k) => (k in data ? data[k] : null),
    setItem: (k, v) => {
      data[k] = v
    },
  }
}

function view(key: string, value: unknown, description?: string): SettingView {
  return { key, type: 'string', mutability: 'hot', value, source: 'default', default: value, description }
}

describe('SettingsUi — collapse', () => {
  it('starts every group collapsed by default', () => {
    const ui = new SettingsUi(memStorage())
    expect(ui.isCollapsed('dream')).toBe(true)
  })

  it('toggle flips and persists; a new instance reads it back', () => {
    const storage = memStorage()
    const ui = new SettingsUi(storage)
    ui.toggle('dream')
    expect(ui.isCollapsed('dream')).toBe(false)
    const again = new SettingsUi(storage)
    expect(again.isCollapsed('dream')).toBe(false)
    expect(again.isCollapsed('chat')).toBe(true)
  })

  it('setAll expands and collapses the given prefixes', () => {
    const ui = new SettingsUi(memStorage())
    ui.setAll(['a', 'b'], false)
    expect(ui.isCollapsed('a')).toBe(false)
    expect(ui.isCollapsed('b')).toBe(false)
    ui.setAll(['a', 'b'], true)
    expect(ui.isCollapsed('a')).toBe(true)
  })

  it('expand opens a closed group and leaves an open one alone', () => {
    const ui = new SettingsUi(memStorage())
    ui.expand('dream')
    expect(ui.isCollapsed('dream')).toBe(false)
    ui.expand('dream')
    expect(ui.isCollapsed('dream')).toBe(false)
  })

  it('survives corrupted storage', () => {
    const ui = new SettingsUi(memStorage({ 'ctx.settings.collapsed': '{not json' }))
    expect(ui.isCollapsed('dream')).toBe(true)
  })

  it('survives a null storage (SSR)', () => {
    const ui = new SettingsUi(null)
    ui.toggle('dream')
    expect(ui.isCollapsed('dream')).toBe(false)
  })
})

describe('SettingsUi — search', () => {
  const settings = [
    view('dream.backoff_factor', 1.6, 'growth base of the exponential curve'),
    view('dream.backoff_min', '12h', 'cooldown floor at eval count 0'),
    view('chat.model', 'qwen3', 'model name requested from the chat backend'),
  ]

  it('is off for a blank query and preserves registry order', () => {
    const ui = new SettingsUi(memStorage())
    ui.query = '  '
    expect(ui.searching).toBe(false)
    const rows = ui.visibleSettings(settings)
    expect(rows.map((r) => r.setting.key)).toEqual(settings.map((s) => s.key))
    expect(rows.every((r) => r.result === null)).toBe(true)
  })

  it('filters to matches and sorts by score', () => {
    const ui = new SettingsUi(memStorage())
    ui.query = 'backoff'
    const rows = ui.visibleSettings(settings)
    expect(rows).toHaveLength(2)
    expect(rows.every((r) => r.setting.key.startsWith('dream.backoff'))).toBe(true)
  })

  it('matches descriptions and values, not only keys', () => {
    const ui = new SettingsUi(memStorage())
    ui.query = 'exponential'
    expect(ui.visibleSettings(settings).map((r) => r.setting.key)).toEqual(['dream.backoff_factor'])
    ui.query = 'qwen3'
    expect(ui.visibleSettings(settings).map((r) => r.setting.key)).toEqual(['chat.model'])
  })

  it('tolerates a typo (Levenshtein tier) end to end', () => {
    const ui = new SettingsUi(memStorage())
    ui.query = 'backpff'
    expect(ui.visibleSettings(settings)).toHaveLength(2)
  })
})

describe('searchFields', () => {
  it('masks sensitive values out of the corpus', () => {
    const s: SettingView = { ...view('chat.api_key', 'set'), sensitive: true }
    const value = searchFields(s).find((f) => f.id === 'value')
    expect(value?.text).toBe('')
  })
})
