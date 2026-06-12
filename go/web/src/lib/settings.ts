// Pure settings-UI logic (design 04-§3.3, against the as-built F2 contract):
// widget dispatch from the registry type, prefix grouping, draft parsing and
// the client-side mirror of the known config cross-field rules. Everything
// here is preview/UX — the server-side candidate build stays authoritative.

import type { SettingView } from './api/types'

/** The five rendering strategies the generic UI knows. */
export type Widget = 'switch' | 'number' | 'select' | 'text' | 'readonly'

/**
 * Registry type → widget (config/describe.go typeNames). An unknown type —
 * a future registry addition this bundle predates — renders read-only
 * instead of guessing an input shape.
 */
export function widgetFor(type: string): Widget {
  switch (type) {
    case 'bool':
      return 'switch'
    case 'int':
    case 'float':
      return 'number'
    case 'protocol':
    case 'think':
    case 'sensitivity':
      return 'select'
    case 'string':
    case 'seconds':
    case 'hours':
    case 'timezone':
    case 'scopes':
    case 'scope_floor':
      return 'text'
    default:
      return 'readonly'
  }
}

/**
 * Options for select-typed keys — a hardcoded mirror of the Go enums
 * (backends.Protocol, backends.ThinkMode); the server 422s anything else.
 */
export function selectOptions(type: string): string[] {
  switch (type) {
    case 'protocol':
      return ['ollama', 'openai']
    case 'think':
      return ['true', 'false']
    case 'sensitivity':
      // Mirror of backends.Sensitivity; LOWERING a guarded pool.default_*
      // key 422s without the confirm flag (F3 §3.5) — the error surfaces
      // on the field, the confirmed path is the CLI (--confirm-sensitivity-downgrade).
      return ['credentials', 'personal', 'internal', 'public']
    default:
      return []
  }
}

/** Free-text hint per registry type, rendered as the input placeholder. */
export function typeHint(type: string): string | null {
  switch (type) {
    case 'seconds':
      return 'Go duration, e.g. 20s or 7m0s'
    case 'hours':
      return 'hours or suffix form, e.g. 12h or 45d'
    case 'scopes':
      return 'comma-separated, e.g. private,shared'
    case 'timezone':
      return 'IANA name, e.g. Europe/Berlin'
    case 'scope_floor':
      return 'JSON map, e.g. {"friend-scope":"personal"}'
    default:
      return null
  }
}

/**
 * Whether the override layer can serve this key (handler/settings.go
 * mutabilityBlock): hot and coupled:embed-cache PUT fine, restart and
 * coupled answer 409 — those render read-only up front, and so does any
 * unknown future mutability class.
 */
export function isEditable(s: SettingView): boolean {
  return s.mutability === 'hot' || s.mutability === 'coupled:embed-cache'
}

/** Inline note explaining the mutability class, mirroring the server texts. */
export function mutabilityNote(s: SettingView): string | null {
  switch (s.mutability) {
    case 'hot':
      return null
    case 'coupled:embed-cache':
      return 'writing flushes the embed cache — query vectors re-embed against the new backend'
    case 'coupled':
      return `coupled to the embedding vector space; changing it requires a re-embed migration — set ${envHint(s)} and restart`
    case 'restart':
      return `restart-only; set ${envHint(s)} and restart`
    default:
      return `unknown mutability "${s.mutability}" — read-only in this UI version`
  }
}

function envHint(s: SettingView): string {
  return s.env_var ? s.env_var : 'it via env (no env var exists for this key)'
}

/** One category card: keys sharing the prefix before the first dot. */
export interface SettingsGroup {
  prefix: string
  settings: SettingView[]
}

/**
 * Group by key prefix, preserving registry order for both the groups and
 * the keys inside them (GET /api/settings renders registry order, §3.3:
 * prefix derivation instead of a second category source that could drift).
 */
export function groupByPrefix(settings: SettingView[]): SettingsGroup[] {
  const groups: SettingsGroup[] = []
  const byPrefix = new Map<string, SettingsGroup>()
  for (const s of settings) {
    const prefix = s.key.split('.', 1)[0]
    let group = byPrefix.get(prefix)
    if (group === undefined) {
      group = { prefix, settings: [] }
      byPrefix.set(prefix, group)
      groups.push(group)
    }
    group.settings.push(s)
  }
  return groups
}

/**
 * Initial draft text for a setting. Sensitive values are masked server-side
 * and a PUT takes a secret NAME, so their drafts always start empty; a
 * non-conforming rendering (e.g. dream_embed.num_ctx's "(inherit embed)"
 * string on an int key) also starts empty — the rendered value shows as
 * placeholder instead, so the draft never round-trips a display artifact.
 */
export function draftFor(s: SettingView): string {
  if (s.sensitive) return ''
  const v = s.value
  switch (widgetFor(s.type)) {
    case 'switch':
      return typeof v === 'boolean' ? String(v) : ''
    case 'number':
      return typeof v === 'number' ? String(v) : ''
    default:
      break
  }
  if (Array.isArray(v)) return v.join(',')
  if (v === null || v === undefined || typeof v === 'object') return ''
  return String(v)
}

/** Human form of an effective value (placeholder / reset affordance). */
export function formatValue(v: unknown): string {
  if (Array.isArray(v)) return v.join(',')
  if (v === null || v === undefined || v === '') return '(empty)'
  return String(v)
}

export type ParsedDraft = { ok: true; value: string | number | boolean } | { ok: false; error: string }

/**
 * Draft text → PUT scalar. Mirrors the server's first-line checks
 * (settings.ScalarValue rejects empty, the handler rejects non-bool bool)
 * so the obvious failures never leave the browser.
 */
export function parseDraft(s: SettingView, draft: string): ParsedDraft {
  const t = draft.trim()
  if (t === '') {
    return { ok: false, error: 'empty value — use reset to remove the override instead' }
  }
  switch (s.type) {
    case 'bool':
      if (t !== 'true' && t !== 'false') return { ok: false, error: 'must be true or false' }
      return { ok: true, value: t === 'true' }
    case 'int': {
      const n = Number(t)
      if (!Number.isInteger(n)) return { ok: false, error: 'must be an integer' }
      return { ok: true, value: n }
    }
    case 'float': {
      const n = Number(t)
      if (!Number.isFinite(n)) return { ok: false, error: 'must be a number' }
      return { ok: true, value: n }
    }
    default:
      return { ok: true, value: t }
  }
}

export interface CrossFieldIssue {
  /** The field the issue renders at (matches the Go Issue.Field). */
  key: string
  severity: 'error' | 'warn'
  message: string
}

/**
 * Client mirror of the three known cross-field rules (V1/V2/V3 in
 * go/internal/config/validate.go) over the would-be effective values —
 * inline preview before the PUT round-trip; the candidate build decides.
 */
export function crossFieldIssues(effective: (key: string) => unknown): CrossFieldIssue[] {
  const issues: CrossFieldIssue[] = []
  const num = (key: string): number | null => {
    const v = effective(key)
    return typeof v === 'number' ? v : null
  }

  // V2 — inverted thresholds make low_confidence unreachable (server: 422).
  const score = num('query.score_threshold')
  const confident = num('query.confident_threshold')
  if (score !== null && confident !== null && score > confident) {
    issues.push({
      key: 'query.score_threshold',
      severity: 'error',
      message: `score_threshold ${score} > confident_threshold ${confident} makes low_confidence unreachable`,
    })
  }

  // V1 — explicit dream/chat num_ctx split on the same host: two runner
  // instances of one model (VRAM OOM under ollama → error there).
  const dreamCtx = num('dream.num_ctx')
  const chatCtx = num('chat.num_ctx')
  if (
    dreamCtx !== null &&
    chatCtx !== null &&
    dreamCtx > 0 &&
    chatCtx > 0 &&
    dreamCtx !== chatCtx &&
    effective('dream.host') === effective('chat.host')
  ) {
    issues.push({
      key: 'dream.num_ctx',
      severity: effective('dream.protocol') === 'ollama' ? 'error' : 'warn',
      message: `dream num_ctx ${dreamCtx} != chat num_ctx ${chatCtx} on the same host — two runner instances of one model (VRAM OOM under ollama)`,
    })
  }

  // V3 — Wave-3 empiricism: cross-encoder as final arbiter destroys
  // graph-injected neighbors (R@10 0.715→0.571).
  const blend = num('rerank.blend_weight')
  if (blend === 1 && effective('graph.enabled') === true) {
    issues.push({
      key: 'rerank.blend_weight',
      severity: 'warn',
      message:
        'blend_weight 1.0 with graph expansion enabled: pure cross-encoder order overrides graph-injected neighbors (Wave-3: destructive) — consider 0.5',
    })
  }

  return issues
}
