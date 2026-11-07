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
// RC-D S1: contract stub — fail-closed (no write control, no option, no spec,
// always confirm). catalog.test.ts is red against it; S2 implements.

import type { ApiError } from '../../lib/api'
import type { ModelAliasCreateSpec, ModelAliasKind, ModelAliasUpdateSpec, ModelAliasView } from '../../lib/api/types'

export type AliasSensitivity = 'credentials' | 'personal' | 'internal' | 'public'

/** The pool roles "Suggest from pool" drafts an alias for (design/05 §3.3): chat, embed, rerank. S1 stub: none. */
export const POOL_ROLES: readonly { role: string; kind: ModelAliasKind }[] = []

/** The fields the edit form shows read-only (immutable server-side). S1 stub: none. */
export const LOCKED_ON_EDIT: readonly string[] = []

/** Write controls of one catalog row for the caller. */
export interface RowControls {
  edit: boolean
  /** enable / disable toggle */
  toggle: boolean
}

/** S1 stub: no control on any row. */
export function rowControls(_row: ModelAliasView, _serverAdmin: boolean): RowControls {
  return { edit: false, toggle: false }
}

/** One sensitivity option of the alias editor. */
export interface AliasSensitivityOption {
  value: AliasSensitivity
  disabled: boolean
  note?: string
}

/**
 * Sensitivity options: server-admin every value (lowering asks for a confirm),
 * tenant-admin only values ≥ `current` (create: ≥ credentials ⇒ credentials
 * only; lower ones disabled "server-admin only"). S1 stub: none.
 */
export function aliasSensitivityOptions(_serverAdmin: boolean, _current: AliasSensitivity | null): AliasSensitivityOption[] {
  return []
}

/**
 * Does moving `from` → `to` lower the floor (towards public) and therefore
 * need the confirm dialog? create: from = null ⇒ compared with credentials.
 * S1 stub: always (fail-closed).
 */
export function needsDowngradeConfirm(_from: AliasSensitivity | null, _to: AliasSensitivity): boolean {
  return true
}

/** The editor's field values. */
export interface AliasForm {
  name: string
  kind: ModelAliasKind
  role: string
  /** '' = the caller's default scope (home / _global). */
  scope: string
  sensitivity: AliasSensitivity
  description: string
  enabled: boolean
  policy: Record<string, unknown>
}

/** model-alias-create spec (enabled ALWAYS set; confirm flag only when `confirmed`). S1 stub: error. */
export function buildAliasCreate(_f: AliasForm, _confirmed: boolean): { spec: ModelAliasCreateSpec } | { error: string } {
  return { error: 'the alias catalog is not wired yet (RC-D S1 contract stub)' }
}

/** model-alias-update patch: changed fields among role/sensitivity/policy/description/enabled only. S1 stub: error. */
export function buildAliasUpdate(_row: ModelAliasView, _f: AliasForm, _confirmed: boolean): { patch: ModelAliasUpdateSpec } | { error: string } {
  return { error: 'the alias catalog is not wired yet (RC-D S1 contract stub)' }
}

/** The draft "Suggest from pool" creates for a role: name = the pool's primary model, enabled:false. S1 stub: null. */
export function suggestDraft(_role: string, _kind: ModelAliasKind, _primaryModel: string | null): ModelAliasCreateSpec | null {
  return null
}

/** The policy keys the AliasPolicyForm offers for a kind (design/02 §3.2). S1 stub: none. */
export function policyKeys(_kind: ModelAliasKind): readonly string[] {
  return []
}

/** A 422 `fields` array mapped field → message (lands at the form field). S1 stub: nothing. */
export function fieldErrors(_err: ApiError): Record<string, string> {
  return {}
}
