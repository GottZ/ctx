// Policy edit of a model consumer — pure field-tier logic of the
// ConsumerEditDialog (design/05 §4.5 Feld-Tier-UX, §4.2 row "Policy ändern";
// wave W05-07 — RC-D C5). The UI MIRRORS the server rules of
// internal/handler/model_consumers.go (field tiers, consumerPolicy.allowed);
// the server stays the authority (a refusal is a 403 at the dialog).
//
//   field / caller            server-admin   tenant-admin, app          tenant-admin, ctx-peer
//   edit control on the row   yes            yes                        none (managed by server-admin)
//   prompt_sensitivity        every value    ≥ current; lower disabled  —
//                                            "server-admin only"
//   models widen / narrow     yes / yes      mintModelKeys / yes        —
//   all_models false → true   yes            mintModelKeys              —
//   tenant_scope set/change   yes            mintModelKeys (→ null: yes) —
//   allowed_cidrs             free           narrow only; "any source"  —
//                                            and prefixes outside the
//                                            stored list need mintModelKeys
//   lane                      lane.ts        lane.ts (only lower)       —
//
// Sensitivity order (strictest first): credentials > personal > internal >
// public — "lower" = towards public.
//
// RC-D S1: contract stub — fail-closed (no edit control, every option
// disabled, no patch). policy.test.ts is red against it; S2 implements.

import type { PromptSensitivity } from '../../lib/api/modelapi'
import type { ConsumerUpdateSpec, ConsumerView, ExternalLane } from '../../lib/api/types'

/** What the caller may do (session.caps). */
export interface EditCaps {
  serverAdmin: boolean
  mintModelKeys: boolean
}

/** One sensitivity option of the edit dialog. */
export interface SensitivityOption {
  value: PromptSensitivity
  disabled: boolean
  /** "server-admin only" on a disabled lower value. */
  note?: string
}

/** The field tiers of one consumer for one caller. */
export interface EditTiers {
  /** No edit control at all (ctx-peer row below server-admin). */
  locked: boolean
  sensitivity: SensitivityOption[]
  /** Unchecked model aliases may be checked (widening). */
  mayWidenModels: boolean
  /** "all offered incl. future" may be switched on. */
  mayEnableAllModels: boolean
  /** tenant_scope may be set or changed (null stays allowed: narrowing). */
  mayChangeScope: boolean
  /** The allowlist may be dropped (null = any source) or get prefixes outside the stored list. */
  mayWidenCidrs: boolean
}

/** Does the row carry an "Edit policy" control for this caller? S1 stub: never. */
export function canEditPolicy(_c: ConsumerView, _caps: EditCaps): boolean {
  return false
}

/** The field tiers of `c` for `caps`. S1 stub: locked, nothing allowed. */
export function editTiers(_c: ConsumerView, _caps: EditCaps): EditTiers {
  return { locked: true, sensitivity: [], mayWidenModels: false, mayEnableAllModels: false, mayChangeScope: false, mayWidenCidrs: false }
}

/**
 * True iff `next` widens the stored allowlist `stored` (mirror of the
 * handler's cidrsNarrow, negated): null over a stored list widens, a prefix not
 * contained in some stored prefix widens; a list over a stored null narrows.
 * S1 stub: everything widens (fail-closed for the tier gate).
 */
export function cidrsWiden(_stored: readonly string[] | null, _next: readonly string[] | null): boolean {
  return true
}

/** The dialog's field values. */
export interface EditFormInput {
  label: string
  description: string
  /** null = only _global. */
  scope: string | null
  allModels: boolean
  /** Alias ids. */
  models: readonly string[]
  sensitivity: PromptSensitivity
  lane: ExternalLane
  /** Raw allowlist text; '' = any source. */
  cidrsText: string
}

/** The form's start values for `c` (what the dialog opens with). S1 stub: empty form. */
export function editFormFrom(_c: ConsumerView): EditFormInput {
  return { label: '', description: '', scope: null, allModels: false, models: [], sensitivity: 'credentials', lane: 'bulk', cidrsText: '' }
}

/**
 * The model-consumer-update patch: ONLY the fields that differ from `c`
 * (models compared as sets, sorted ids on the wire; the CIDR text parsed like
 * the create dialog), or the first client-side error — including a tier
 * violation the tiers forbid ("server-admin only"), so a forbidden change
 * never reaches the wire. No change ⇒ error "nothing changed".
 * S1 stub: always the error "not wired".
 */
export function buildUpdatePatch(_c: ConsumerView, _f: EditFormInput, _tiers: EditTiers): { patch: ConsumerUpdateSpec } | { error: string } {
  return { error: 'policy editing is not wired yet (RC-D S1 contract stub)' }
}
