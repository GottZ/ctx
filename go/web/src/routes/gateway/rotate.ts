// Key rotation of a model consumer — pure logic of the RotateDialog and the
// KeyChain "End grace period now" control (design/05 §4.6 RotateDialog, key row
// "superseded", §5 B-20; wave W05-08 — RC-D C5). Server: model-key-rotate
// {consumer_id, grace (s), expires_at?} ⇒ {key, api_key}; model-key-revoke
// {key_id} ends the grace window of exactly ONE key (the predecessor).
//
//   Reason (required question)                         grace
//   "Leak / suspicion — revoke the old key now"        0          ← PRESELECTED
//   "Planned change" + 1 h / 24 h / 7 d                3600 / 86400 / 604800
//                                                      (default rotation_grace_s,
//                                                      clamped to ≤ 7 d)
//
// In status `rotating` (two live keys) only the PREDECESSOR (the older live
// key, status `superseded`) carries "End grace period now", and that control
// revokes the predecessor's OWN key_id — never the consumer id, never the
// successor (B-20: a variant that revokes every live key would lock out the
// successor too).
//
// RC-D S1: contract stub — fail-closed (no reason preselected, no option, no
// target, no rotate control). rotate.test.ts is red against it.

import type { ConsumerView, KeyView, ModelKeyRotateSpec } from '../../lib/api/types'
import type { EditCaps } from './policy'

export type RotateReason = 'leak' | 'planned'

/** Upper bound of the grace window (store.MaxRotationGrace = 7 d). */
export const MAX_ROTATION_GRACE_S = 604_800

/** The dialog's choice. */
export interface RotateChoice {
  reason: RotateReason
  /** grace in seconds — 0 for a leak. */
  graceS: number
}

/** One planned-change grace option. */
export interface GraceOption {
  label: string
  seconds: number
}

/** The preselected choice: leak — revoke now (grace 0). S1 stub: nothing chosen (null). */
export function defaultRotateChoice(): RotateChoice | null {
  return null
}

/**
 * The planned-change grace options (1 h / 24 h / 7 d) and which one is
 * preselected for a planned change: whoami model_api.rotation_grace_s,
 * clamped to MAX_ROTATION_GRACE_S. S1 stub: none.
 */
export function graceOptions(_rotationGraceS: number | null): { options: GraceOption[]; preselected: number | null } {
  return { options: [], preselected: null }
}

/** The model-key-rotate payload for `choice` — grace always sent. S1 stub: null (nothing to send). */
export function rotateSpec(_consumerId: string, _choice: RotateChoice): ModelKeyRotateSpec | null {
  return null
}

/** Does the row offer "Rotate key"? server-admin; tenant-admin with mintModelKeys on an active app consumer. S1 stub: never. */
export function canRotate(_c: ConsumerView, _caps: EditCaps): boolean {
  return false
}

/**
 * The key that carries "End grace period now": in status `rotating` the
 * predecessor (older live key, status superseded) — else null. Its `id` is
 * what model-key-revoke receives. S1 stub: null.
 */
export function endGraceTarget(_c: ConsumerView): KeyView | null {
  return null
}
