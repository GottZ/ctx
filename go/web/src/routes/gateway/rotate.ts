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
// Pure module: no DOM, no clock.

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

/** The fixed planned-change windows (design/05 §4.6: 1 h / 24 h / 7 d). */
const GRACE_PRESETS: readonly GraceOption[] = [
  { label: '1 hour', seconds: 3_600 },
  { label: '24 hours', seconds: 86_400 },
  { label: '7 days', seconds: MAX_ROTATION_GRACE_S },
]

/** The preselected choice: leak — revoke the predecessor now (grace 0, B-20). */
export function defaultRotateChoice(): RotateChoice {
  return { reason: 'leak', graceS: 0 }
}

function graceLabel(s: number): string {
  if (s % 86_400 === 0) return s === 86_400 ? '24 hours' : `${s / 86_400} days`
  if (s % 3_600 === 0) return s === 3_600 ? '1 hour' : `${s / 3_600} hours`
  if (s % 60 === 0) return `${s / 60} minutes`
  return `${s} seconds`
}

/**
 * The planned-change grace options and the preselected one: whoami
 * model_api.rotation_grace_s, clamped to MAX_ROTATION_GRACE_S; a server value
 * outside the presets joins the list (sorted) so the default stays the
 * server's; absent / not positive ⇒ the shortest preset (smallest exposure).
 */
export function graceOptions(rotationGraceS: number | null): { options: GraceOption[]; preselected: number } {
  const options = [...GRACE_PRESETS]
  if (rotationGraceS === null || !Number.isFinite(rotationGraceS) || rotationGraceS <= 0) return { options, preselected: GRACE_PRESETS[0]!.seconds }
  const pre = Math.min(Math.floor(rotationGraceS), MAX_ROTATION_GRACE_S)
  if (!options.some((o) => o.seconds === pre)) {
    options.push({ label: graceLabel(pre), seconds: pre })
    options.sort((a, b) => a.seconds - b.seconds)
  }
  return { options, preselected: pre }
}

/** The model-key-rotate payload for `choice` — grace always present (0 for a leak), clamped to [0, 7 d]. */
export function rotateSpec(consumerId: string, choice: RotateChoice): ModelKeyRotateSpec {
  const grace = choice.reason === 'leak' ? 0 : Math.max(0, Math.min(Math.floor(choice.graceS), MAX_ROTATION_GRACE_S))
  return { consumer_id: consumerId, grace }
}

/**
 * Does the row offer "Rotate key"? Granting (model_consumers.go): a
 * server-admin on any active consumer; a tenant-admin with mintModelKeys on an
 * active app consumer — never a ctx-peer below server-admin (V-01-b). An
 * inactive consumer cannot rotate (409).
 */
export function canRotate(c: ConsumerView, caps: EditCaps): boolean {
  if (!c.active) return false
  return caps.serverAdmin || (c.kind !== 'ctx-peer' && caps.mintModelKeys)
}

/**
 * The key that carries "End grace period now": only in status `rotating`, the
 * ONE live key the server marks `superseded` (the predecessor in its grace
 * window). Ambiguous answers yield null — never a guess that could revoke
 * the successor (B-20).
 */
export function endGraceTarget(c: ConsumerView): KeyView | null {
  if (c.status !== 'rotating') return null
  const superseded = c.keys.filter((k) => k.status === 'superseded' && k.revoked_at === null)
  return superseded.length === 1 ? superseded[0]! : null
}
