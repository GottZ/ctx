// Editing state + save flow for the settings area (design 04-§3.3): drafts
// over the loaded catalog, per-field server errors/warnings, sequential PUTs
// per group save (one call per key — a 422 maps to exactly one field), and
// the DELETE-reset affordance. Plain $state class so vitest covers the flow
// without a DOM; the api functions are injectable for the same reason.

import { toApiError } from '../../lib/api'
import { deleteSetting, putSetting } from '../../lib/api/settings'
import type { SettingPutResponse, SettingView } from '../../lib/api/types'
import {
  crossFieldIssues,
  draftFor,
  groupByPrefix,
  isEditable,
  parseDraft,
  type CrossFieldIssue,
} from '../../lib/settings'

interface SettingsApi {
  put: typeof putSetting
  del: typeof deleteSetting
}

export class SettingsModel {
  settings = $state<SettingView[]>([])
  drafts = $state<Record<string, string>>({})
  /** Per-field failure: client parse error or the server's 4xx message. */
  errors = $state<Record<string, string>>({})
  /** PUT-response advisories (the F2 cross-field warnings[]). */
  warnings = $state<Record<string, string[]>>({})
  saving = $state(false)

  readonly groups = $derived(groupByPrefix(this.settings))
  /** Live mirror of the config cross-field rules over the drafted values. */
  readonly issues: CrossFieldIssue[] = $derived(crossFieldIssues((key) => this.#effective(key)))

  #api: SettingsApi

  constructor(api: SettingsApi = { put: putSetting, del: deleteSetting }) {
    this.#api = api
  }

  /** Adopt a freshly loaded catalog; all edit state restarts from it. */
  load(settings: SettingView[]): void {
    this.settings = settings
    const drafts: Record<string, string> = {}
    for (const s of settings) drafts[s.key] = draftFor(s)
    this.drafts = drafts
    this.errors = {}
    this.warnings = {}
  }

  byKey(key: string): SettingView | undefined {
    return this.settings.find((s) => s.key === key)
  }

  isDirty(key: string): boolean {
    const s = this.byKey(key)
    if (s === undefined || !isEditable(s)) return false
    return this.drafts[key] !== draftFor(s)
  }

  dirtyKeys(prefix: string): string[] {
    return this.settings
      .filter((s) => s.key.split('.', 1)[0] === prefix && this.isDirty(s.key))
      .map((s) => s.key)
  }

  /** Cross-field issues attributed to one field. */
  issuesFor(key: string): CrossFieldIssue[] {
    return this.issues.filter((issue) => issue.key === key)
  }

  /**
   * Save one category card: sequential PUTs of the dirty keys. A failed key
   * keeps its error inline and the loop continues — the remaining fields are
   * independent writes, not a transaction.
   */
  async saveGroup(prefix: string): Promise<void> {
    this.saving = true
    try {
      for (const key of this.dirtyKeys(prefix)) {
        const s = this.byKey(key)
        if (s === undefined) continue
        delete this.errors[key]
        delete this.warnings[key]
        const parsed = parseDraft(s, this.drafts[key])
        if (!parsed.ok) {
          this.errors[key] = parsed.error
          continue
        }
        try {
          this.#applyPut(s, await this.#api.put(key, parsed.value))
        } catch (err) {
          this.errors[key] = toApiError(err).message
        }
      }
    } finally {
      this.saving = false
    }
  }

  /** DELETE the override — revert to env/default. */
  async reset(key: string): Promise<void> {
    const s = this.byKey(key)
    if (s === undefined) return
    delete this.errors[key]
    delete this.warnings[key]
    try {
      const res = await this.#api.del(key)
      s.value = res.value
      s.source = res.source
      this.drafts[key] = draftFor(s)
    } catch (err) {
      this.errors[key] = toApiError(err).message
    }
  }

  /** Drop the local edit of one field (back to the loaded value). */
  revertDraft(key: string): void {
    const s = this.byKey(key)
    if (s === undefined) return
    this.drafts[key] = draftFor(s)
    delete this.errors[key]
  }

  #applyPut(s: SettingView, res: SettingPutResponse): void {
    s.value = res.value
    s.source = res.source
    this.drafts[s.key] = draftFor(s)
    if (res.warnings.length > 0) this.warnings[s.key] = res.warnings
  }

  /** Would-be effective value: the parsed draft where dirty, else current. */
  #effective(key: string): unknown {
    const s = this.byKey(key)
    if (s === undefined) return undefined
    if (this.isDirty(key)) {
      const parsed = parseDraft(s, this.drafts[key])
      if (parsed.ok) return parsed.value
    }
    return s.value
  }
}
