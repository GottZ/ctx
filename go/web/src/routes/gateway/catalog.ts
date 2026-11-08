// Alias catalog /gateway/models — pure editor logic (design/05 §3.3 editor
// rules, §4.6 ModelsPage, §5 B-8/B-17/B-23; wave W05-13 — RC-D C5). Server =
// internal/handler/model_alias_manage.go (authoritative):
//   - a tenant-admin reads _global + its home scope and writes ONLY its home
//     scope; `_global` rows carry no write control below server-admin (the
//     row's `writable` AND the scope — two independent locks, inv05 D8);
//   - name and kind are IMMUTABLE after create (400 immutable_field, V-02-a):
//     the edit form shows them read-only and the patch has no field for them;
//   - `enabled` is always sent on create (the handler 422s without it); a pool
//     suggestion is a DRAFT: enabled:false;
//   - lowering the sensitivity (create: below credentials; update: below the
//     current value) is server-admin only and only after an explicit confirm
//     ("foreign prompts may then reach backends with trust < full") — the
//     confirmed request carries confirm_sensitivity_downgrade:true;
//   - a tenant alias may not sit below the _global alias of the same name
//     (V-02-c, 422 at `sensitivity`) — shown at the field.
//
// Pure module: no DOM, no clock.

import type { ApiError } from '../../lib/api'
import type { ModelAliasCreateSpec, ModelAliasKind, ModelAliasUpdateSpec, ModelAliasView } from '../../lib/api/types'

export type AliasSensitivity = 'credentials' | 'personal' | 'internal' | 'public'

/** The pool roles "Suggest from pool" drafts an alias for (design/05 §3.3) and the alias kind each one serves. */
export const POOL_ROLES: readonly { role: 'chat' | 'embed' | 'rerank'; kind: ModelAliasKind }[] = [
  { role: 'chat', kind: 'chat' },
  { role: 'embed', kind: 'embedding' },
  { role: 'rerank', kind: 'rerank' },
]

/** The fields the edit form shows read-only (immutable server-side, V-02-a). */
export const LOCKED_ON_EDIT: readonly string[] = ['name', 'kind']

/** The alias kinds (migration 162 CHECK), in the editor's order. */
export const ALIAS_KINDS: readonly ModelAliasKind[] = ['chat', 'embedding', 'rerank']

/** Strictest first — "lower" = a higher index. */
export const ALIAS_SENSITIVITIES: readonly AliasSensitivity[] = ['credentials', 'personal', 'internal', 'public']

function rank(s: string | null): number {
  const i = ALIAS_SENSITIVITIES.indexOf((s ?? 'credentials') as AliasSensitivity)
  return i < 0 ? 0 : i
}

/** Write controls of one catalog row for the caller. */
export interface RowControls {
  edit: boolean
  /** enable / disable toggle */
  toggle: boolean
}

/**
 * Two independent locks: the server's `writable` AND — below server-admin —
 * a scope other than `_global` (inv05 D8: a single flag is no lock).
 */
export function rowControls(row: ModelAliasView, serverAdmin: boolean): RowControls {
  const ok = row.writable && (serverAdmin || row.scope !== '_global')
  return { edit: ok, toggle: ok }
}

/** One sensitivity option of the alias editor. */
export interface AliasSensitivityOption {
  value: AliasSensitivity
  disabled: boolean
  note?: string
}

/**
 * Sensitivity options: a server-admin every value (lowering asks for a
 * confirm), a tenant-admin only values at or above `current` (create: at or
 * above credentials ⇒ credentials only); lower ones disabled "server-admin only".
 */
export function aliasSensitivityOptions(serverAdmin: boolean, current: AliasSensitivity | null): AliasSensitivityOption[] {
  const floor = rank(current)
  return ALIAS_SENSITIVITIES.map((value) =>
    serverAdmin || rank(value) <= floor ? { value, disabled: false } : { value, disabled: true, note: 'server-admin only' },
  )
}

/** Does moving `from` → `to` lower the floor (create: from = null ⇒ credentials)? */
export function needsDowngradeConfirm(from: AliasSensitivity | null, to: AliasSensitivity): boolean {
  return rank(to) > rank(from)
}

/** The editor's field values. */
export interface AliasForm {
  name: string
  kind: ModelAliasKind
  role: string
  /** '' = the caller's default scope (home / _global). */
  scope: string
  /** server-admin only: the tenant a tenant scope belongs to ('' / absent = none; the handler 422s a tenant scope without it). */
  tenantId?: string
  sensitivity: AliasSensitivity
  description: string
  enabled: boolean
  policy: Record<string, unknown>
}

/** The confirm text shown before a downgrade is sent. */
export const DOWNGRADE_WARNING = 'foreign prompts may then reach backends with trust below full'

/**
 * model-alias-create spec: name, kind, role, enabled and sensitivity ALWAYS;
 * scope / description / policy only when set; a downgrade below credentials
 * only `confirmed` (then with confirm_sensitivity_downgrade:true).
 */
export function buildAliasCreate(f: AliasForm, confirmed: boolean): { spec: ModelAliasCreateSpec } | { error: string } {
  const name = f.name.trim()
  if (name === '') return { error: 'name is required' }
  const role = f.role.trim()
  if (role === '') return { error: 'role is required' }
  const lowers = needsDowngradeConfirm(null, f.sensitivity)
  if (lowers && !confirmed) return { error: `lowering the sensitivity below credentials needs a confirmation — ${DOWNGRADE_WARNING}` }
  const spec: ModelAliasCreateSpec = { name, kind: f.kind, role, enabled: f.enabled, sensitivity: f.sensitivity }
  if (f.scope !== '') spec.scope = f.scope
  if (f.tenantId) spec.tenant_id = f.tenantId
  const description = f.description.trim()
  if (description !== '') spec.description = description
  if (Object.keys(f.policy).length > 0) spec.policy = { ...f.policy }
  if (lowers) spec.confirm_sensitivity_downgrade = true
  return { spec }
}

/** Key-order independent JSON of a policy object (flat dotted keys, scalar values). */
function canonical(p: Record<string, unknown>): string {
  return JSON.stringify(Object.keys(p).sort().map((k) => [k, p[k]]))
}

/**
 * model-alias-update patch: the changed MUTABLE fields only (role,
 * sensitivity, policy, description, enabled) — name / kind / scope never,
 * whatever the form holds; a downgrade only `confirmed`.
 */
export function buildAliasUpdate(row: ModelAliasView, f: AliasForm, confirmed: boolean): { patch: ModelAliasUpdateSpec } | { error: string } {
  const patch: ModelAliasUpdateSpec = {}
  const role = f.role.trim()
  if (role === '') return { error: 'role is required' }
  if (role !== row.role) patch.role = role
  if (f.sensitivity !== row.sensitivity) {
    const lowers = needsDowngradeConfirm((row.sensitivity as AliasSensitivity) ?? null, f.sensitivity)
    if (lowers && !confirmed) return { error: `lowering the sensitivity needs a confirmation — ${DOWNGRADE_WARNING}` }
    patch.sensitivity = f.sensitivity
    if (lowers) patch.confirm_sensitivity_downgrade = true
  }
  const description = f.description.trim()
  if (description !== row.description) patch.description = description
  if (f.enabled !== row.enabled) patch.enabled = f.enabled
  if (canonical(f.policy) !== canonical(row.policy ?? {})) patch.policy = { ...f.policy }
  return Object.keys(patch).length === 0 ? { error: 'nothing changed' } : { patch }
}

/**
 * The draft "Suggest from pool" creates for a role: named after the pool's
 * primary model, ALWAYS enabled:false (a suggestion is never live). No primary
 * ('' or null — the pool offers nothing) ⇒ null (fail-closed, E-02-13).
 */
export function suggestDraft(role: string, kind: ModelAliasKind, primaryModel: string | null): ModelAliasCreateSpec | null {
  const name = (primaryModel ?? '').trim()
  if (name === '') return null
  return { name, kind, role, enabled: false }
}

/** One policy key of the form: its wire name and its value type (modelalias/policy.go policyKeys). */
export interface PolicyKey {
  key: string
  type: 'int' | 'bool' | 'reasoning'
}

const POLICY_KEYS: readonly (PolicyKey & { kinds: readonly ModelAliasKind[] })[] = [
  { key: 'max_output_tokens', type: 'int', kinds: ['chat'] },
  { key: 'default_max_tokens', type: 'int', kinds: ['chat'] },
  { key: 'max_n', type: 'int', kinds: ['chat'] },
  { key: 'timeout_s', type: 'int', kinds: ['chat', 'embedding', 'rerank'] },
  { key: 'reasoning', type: 'reasoning', kinds: ['chat'] },
  { key: 'vision', type: 'bool', kinds: ['chat'] },
  { key: 'vision_max_bytes', type: 'int', kinds: ['chat'] },
  { key: 'context_length', type: 'int', kinds: ['chat'] },
  { key: 'tools.max_count', type: 'int', kinds: ['chat'] },
  { key: 'tools.max_bytes', type: 'int', kinds: ['chat'] },
  { key: 'response_format.max_bytes', type: 'int', kinds: ['chat'] },
  { key: 'logit_bias.max_entries', type: 'int', kinds: ['chat'] },
  { key: 'stop.max_entries', type: 'int', kinds: ['chat'] },
  { key: 'embedding.native_dims', type: 'int', kinds: ['embedding'] },
  { key: 'embedding.matryoshka_min', type: 'int', kinds: ['embedding'] },
  { key: 'embedding.max_inputs', type: 'int', kinds: ['embedding'] },
  { key: 'embedding.sub_batch', type: 'int', kinds: ['embedding'] },
  { key: 'embedding.max_input_runes', type: 'int', kinds: ['embedding'] },
  { key: 'rerank.max_documents', type: 'int', kinds: ['rerank'] },
  { key: 'rerank.sub_batch', type: 'int', kinds: ['rerank'] },
]

/** The policy keys (with type) the AliasPolicyForm offers for a kind. */
export function policyFields(kind: ModelAliasKind): readonly PolicyKey[] {
  return POLICY_KEYS.filter((p) => p.kinds.includes(kind)).map(({ key, type }) => ({ key, type }))
}

/** The policy key names for a kind (design/02 §3.2; mirror of modelalias.policyKeys). */
export function policyKeys(kind: ModelAliasKind): readonly string[] {
  return policyFields(kind).map((p) => p.key)
}

/** A 422 `fields` array mapped field → message (lands at the form field); anything else ⇒ {}. */
export function fieldErrors(err: ApiError): Record<string, string> {
  const fields = err.details?.fields
  if (!Array.isArray(fields)) return {}
  const out: Record<string, string> = {}
  for (const f of fields) {
    if (f !== null && typeof f === 'object' && typeof (f as { field?: unknown }).field === 'string') {
      const e = f as { field: string; message?: unknown }
      out[e.field] = typeof e.message === 'string' ? e.message : 'invalid'
    }
  }
  return out
}
