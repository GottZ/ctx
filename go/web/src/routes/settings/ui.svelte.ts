// Presentation state of the settings area, split from the save-flow model
// (model.svelte.ts owns drafts/errors/PUTs; this owns what the user SEES):
// per-group collapse with localStorage persistence, and the fuzzy search
// query with its per-setting match results. Plain $state class so vitest
// covers collapse/search logic DOM-free (storage injectable like the
// ThemeController's).

import { fuzzyMatch, type FuzzyResult } from '../../lib/fuzzy'
import { formatValue } from '../../lib/settings'
import type { SettingView } from '../../lib/api/types'

const STORAGE_KEY = 'ctx.settings.collapsed'

/** Minimal storage surface (localStorage-shaped) for testability. */
export interface KVStorage {
  getItem(key: string): string | null
  setItem(key: string, value: string): void
}

function defaultStorage(): KVStorage | null {
  try {
    return typeof localStorage === 'undefined' ? null : localStorage
  } catch {
    return null // SSR / privacy mode — collapse state just won't persist
  }
}

/** Search fields per setting: key dominates, description carries prose hits,
 *  value/env/type catch "what is set to 8080"-style queries. */
export function searchFields(s: SettingView): Array<{ id: string; text: string; weight: number }> {
  return [
    { id: 'key', text: s.key, weight: 1.0 },
    { id: 'description', text: s.description ?? '', weight: 0.7 },
    { id: 'value', text: s.sensitive ? '' : formatValue(s.value), weight: 0.6 },
    { id: 'env', text: s.env_var ?? '', weight: 0.5 },
    { id: 'type', text: s.type, weight: 0.3 },
  ]
}

export class SettingsUi {
  /** Explicit per-prefix collapse choices; unlisted prefixes use the default. */
  collapsed = $state<Record<string, boolean>>({})
  /** Live fuzzy query ('' = search off, plain category view). */
  query = $state('')

  #storage: KVStorage | null

  constructor(storage: KVStorage | null = defaultStorage()) {
    this.#storage = storage
    this.collapsed = this.#readStored()
  }

  /** Groups start collapsed — 30+ categories scan better as a closed index. */
  isCollapsed(prefix: string): boolean {
    return this.collapsed[prefix] ?? true
  }

  toggle(prefix: string): void {
    this.collapsed = { ...this.collapsed, [prefix]: !this.isCollapsed(prefix) }
    this.#persist()
  }

  setAll(prefixes: string[], collapsed: boolean): void {
    const next: Record<string, boolean> = { ...this.collapsed }
    for (const p of prefixes) next[p] = collapsed
    this.collapsed = next
    this.#persist()
  }

  /** Jump-nav click: expand the group (a jump to a closed card is a dead end). */
  expand(prefix: string): void {
    if (this.isCollapsed(prefix)) this.toggle(prefix)
  }

  get searching(): boolean {
    return this.query.trim().length > 0
  }

  /** Fuzzy result for one setting under the current query; null = filtered. */
  match(s: SettingView): FuzzyResult | null {
    return fuzzyMatch(this.query, searchFields(s))
  }

  /**
   * The settings of one group under the current query, best score first.
   * Search off → registry order untouched. Search on → only matches, so an
   * empty return means the whole group hides.
   */
  visibleSettings(settings: SettingView[]): Array<{ setting: SettingView; result: FuzzyResult | null }> {
    if (!this.searching) return settings.map((setting) => ({ setting, result: null }))
    const out: Array<{ setting: SettingView; result: FuzzyResult }> = []
    for (const setting of settings) {
      const result = this.match(setting)
      if (result !== null) out.push({ setting, result })
    }
    out.sort((a, b) => b.result.score - a.result.score)
    return out
  }

  #readStored(): Record<string, boolean> {
    try {
      const raw = this.#storage?.getItem(STORAGE_KEY)
      if (raw === null || raw === undefined) return {}
      const parsed: unknown = JSON.parse(raw)
      if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) return {}
      const out: Record<string, boolean> = {}
      for (const [k, v] of Object.entries(parsed)) {
        if (typeof v === 'boolean') out[k] = v
      }
      return out
    } catch {
      return {}
    }
  }

  #persist(): void {
    try {
      this.#storage?.setItem(STORAGE_KEY, JSON.stringify(this.collapsed))
    } catch {
      // quota/privacy failures lose persistence, never the session state
    }
  }
}
