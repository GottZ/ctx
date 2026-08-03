// Pure helpers for the pool editor (design 04-§3.5). Trust ranks + elevation
// detection mirror backends/trust.go for UX only — the server stays
// authoritative (it answers 400 without confirm_trust_elevation). The enum
// option lists, model_map row<->object conversion, the update diff (so a patch
// touches only changed keys, honoring the server's raw-key-presence semantics)
// and the client-side secret-usage join all live here so vitest covers them
// without a DOM.

import { toApiError } from './api'
import type { BackendListItem, BackendSpec, BackendView, ModelSpec, SecretMeta, SettingView } from './api/types'

export interface BackendFieldError {
  field: string
  message: string
}

/** Per-field validation errors carried on a 422 (ValidateBackend → fields[]);
 *  empty for any other failure. Lets the dialog show errors next to fields. */
export function fieldErrors(err: unknown): BackendFieldError[] {
  const f = toApiError(err).details?.fields
  if (!Array.isArray(f)) return []
  return f.filter(
    (x): x is BackendFieldError =>
      typeof x === 'object' && x !== null && typeof (x as { field?: unknown }).field === 'string',
  )
}

// Trust levels, most→least trusted, with the user's semantics (design §3.5).
// rank mirrors backends/trust.go; an elevation (rank rose) needs confirmation.
export interface TrustLevel {
  value: string
  rank: number
  tip: string
}
export const TRUST_LEVELS: TrustLevel[] = [
  { value: 'full-trust', rank: 3, tip: 'lokale Modelle + Cloud ohne Datenspeicherung — darf Credentials sehen' },
  { value: 'no-credentials', rank: 2, tip: 'keine Passwörter/Credentials verlassen diesen Backend' },
  { value: 'non-personal', rank: 1, tip: 'keine persönlichen Daten — nur internes Material' },
  {
    value: 'public',
    rank: 0,
    tip: 'darf auch öffentliche Cloud sehen (z. B. DeepSeek/China) — nur public Content wie Prompt-Benchmarks',
  },
]

/** Display order = the server's Chain order (pool.go): priority DESC, name ASC.
 *  Returns a new array; the input is untouched. */
export function sortBackends(backends: BackendListItem[]): BackendListItem[] {
  return [...backends].sort((a, b) => b.priority - a.priority || a.name.localeCompare(b.name))
}

const TRUST_RANK = new Map(TRUST_LEVELS.map((t) => [t.value, t.rank]))

/** Rank of a trust level; -1 for unknown (admits nothing) — mirrors trust.go. */
export function trustRank(trust: string): number {
  return TRUST_RANK.get(trust) ?? -1
}

/** True when next is more trusted than prev — the server demands confirm then. */
export function isTrustElevation(prev: string, next: string): boolean {
  return trustRank(next) > trustRank(prev)
}

// Wire enums (verified against backends/validate.go, NOT the design doc which
// undercounted locality). locality is also auto-derived from base_url on create
// and cross-checked against the host, so '' (auto) is the sane default.
export const PROTOCOLS = ['openai', 'ollama', 'rerank'] as const
export const PROVIDER_CLASSES = ['generic', 'llamacpp', 'openrouter'] as const
export const LOCALITIES = ['local', 'lan', 'external'] as const

// Core roles (backend.go); free-text proxy roles are allowed but warn server-side.
export const CORE_ROLES = [
  'synthesis',
  'translate',
  'embed',
  'rerank',
  'dream',
  'digest',
  'chat',
  'dream-embed',
  'classify',
] as const

// model_map row form for the line editor. params (rare) are preserved verbatim
// across edits but not surfaced as editable fields.
export interface ModelMapRow {
  role: string
  model: string
  params?: Record<string, unknown>
}

export function modelMapToRows(mm: Record<string, ModelSpec>): ModelMapRow[] {
  return Object.entries(mm).map(([role, spec]) => ({ role, model: spec.model, params: spec.params }))
}

/** Rows → wire object form {role:{model,params?}}, dropping blank rows. */
export function rowsToModelMap(rows: ModelMapRow[]): Record<string, ModelSpec> {
  const out: Record<string, ModelSpec> = {}
  for (const r of rows) {
    const role = r.role.trim()
    const model = r.model.trim()
    if (role === '' || model === '') continue
    out[role] = r.params ? { model, params: r.params } : { model }
  }
  return out
}

function sameSet(a: string[], b: string[]): boolean {
  if (a.length !== b.length) return false
  const sa = [...a].sort()
  const sb = [...b].sort()
  return sa.every((v, i) => v === sb[i])
}

function sameModelMap(a: Record<string, ModelSpec>, b: Record<string, ModelSpec>): boolean {
  const ak = Object.keys(a)
  if (ak.length !== Object.keys(b).length) return false
  return ak.every(
    (k) =>
      b[k] !== undefined &&
      a[k].model === b[k].model &&
      JSON.stringify(a[k].params ?? null) === JSON.stringify(b[k].params ?? null),
  )
}

/** The editable surface of the backend dialog (name immutable; priority/enabled
 *  are table-level single-field patches, not part of this draft). */
export interface BackendDraft {
  base_url: string
  protocol: string
  provider_class: string
  api_key_ref: string
  locality: string // '' = auto-derive (omitted on create)
  num_ctx: number
  trust: string
  roles: string[]
  model_map: Record<string, ModelSpec>
  // disable-profile membership (092, U01-W6): the profile names this backend
  // belongs to. The dialog checkbox section edits it; it rides on the same
  // create/update patch (raw-key presence — see createSpec/backendDiff).
  disable_profiles: string[]
  // metadata additions earned in-dialog (today: the embed-equivalence
  // metadata_patch). Absent = untouched; the dialog never edits metadata
  // directly, it only merges server-produced patches in.
  metadata?: Record<string, unknown>
}

export function draftFromBackend(b: BackendView): BackendDraft {
  return {
    base_url: b.base_url,
    protocol: b.protocol,
    provider_class: b.provider_class,
    api_key_ref: b.api_key_ref,
    locality: b.locality,
    num_ctx: b.num_ctx,
    trust: b.trust,
    roles: [...b.roles],
    model_map: { ...b.model_map },
    disable_profiles: [...(b.disable_profiles ?? [])],
  }
}

/** A create spec: name + every meaningful draft field. locality '' is dropped
 *  so the server auto-derives it from base_url. */
export function createSpec(name: string, d: BackendDraft): BackendSpec {
  const spec: BackendSpec = {
    name,
    base_url: d.base_url,
    protocol: d.protocol,
    provider_class: d.provider_class,
    trust: d.trust,
    num_ctx: d.num_ctx,
    roles: d.roles,
    model_map: d.model_map,
  }
  if (d.api_key_ref) spec.api_key_ref = d.api_key_ref
  if (d.locality) spec.locality = d.locality
  // Membership is always sent on create so the join is set from the start; []
  // is the harmless no-op (clears nothing on a fresh backend).
  spec.disable_profiles = d.disable_profiles
  if (d.metadata) spec.metadata = d.metadata
  return spec
}

/** An update patch carrying ONLY changed fields — relies on the server keeping
 *  omitted keys (raw-key presence). An explicit roles:[]/model_map:{} clears. */
export function backendDiff(original: BackendView, d: BackendDraft): BackendSpec {
  const spec: BackendSpec = {}
  if (d.base_url !== original.base_url) spec.base_url = d.base_url
  if (d.protocol !== original.protocol) spec.protocol = d.protocol
  if (d.provider_class !== original.provider_class) spec.provider_class = d.provider_class
  if (d.api_key_ref !== original.api_key_ref) spec.api_key_ref = d.api_key_ref
  if (d.locality !== original.locality) spec.locality = d.locality
  if (d.num_ctx !== original.num_ctx) spec.num_ctx = d.num_ctx
  if (d.trust !== original.trust) spec.trust = d.trust
  if (!sameSet(d.roles, original.roles)) spec.roles = d.roles
  if (!sameModelMap(d.model_map, original.model_map)) spec.model_map = d.model_map
  // Only patch membership when it changed — an absent key leaves the join
  // untouched server-side (§4.3); an explicit [] clears all memberships.
  if (!sameSet(d.disable_profiles, original.disable_profiles ?? [])) spec.disable_profiles = d.disable_profiles
  // metadata REPLACES server-side (applySpec) — merge over the stored object
  // so an in-dialog patch never drops score_domain & friends.
  if (d.metadata) spec.metadata = { ...original.metadata, ...d.metadata }
  return spec
}

/** True when an update patch has no field changes (skip the round-trip). */
export function isEmptySpec(spec: BackendSpec): boolean {
  return Object.keys(spec).length === 0
}

// ---- secret usage join (design §3.5: derived "fehlt" status) ----------------

export interface SecretUsage {
  /** secret name → backend names referencing it via api_key_ref (client-side;
   *  the server's referenced_by covers settings keys only). */
  backendsBySecret: Map<string, string[]>
  /** refs pointing at a non-existent secret — the derived "fehlt" status. */
  dangling: { source: 'backend' | 'setting'; ref: string; secret: string }[]
}

export function secretUsage(
  secrets: SecretMeta[],
  backends: BackendListItem[],
  settings: SettingView[],
): SecretUsage {
  const known = new Set(secrets.map((s) => s.name))
  const backendsBySecret = new Map<string, string[]>()
  const dangling: SecretUsage['dangling'] = []

  for (const b of backends) {
    const ref = b.api_key_ref
    if (!ref) continue
    if (known.has(ref)) {
      const list = backendsBySecret.get(ref) ?? []
      list.push(b.name)
      backendsBySecret.set(ref, list)
    } else {
      dangling.push({ source: 'backend', ref: b.name, secret: ref })
    }
  }

  // sensitive db-sourced settings carry the secret_ref name as their value
  // (handler/settings.go masking rule); env-sourced render "(set via env)".
  for (const s of settings) {
    if (!s.sensitive || s.source !== 'db' || typeof s.value !== 'string') continue
    if (!known.has(s.value)) dangling.push({ source: 'setting', ref: s.key, secret: s.value })
  }

  return { backendsBySecret, dangling }
}
