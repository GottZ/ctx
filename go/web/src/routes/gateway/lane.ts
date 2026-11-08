// Admission lane of a model consumer (design/05 §4.5 row "Lane", §4.8, §5 B-4,
// V-03-a; wave W05-11 — RC-D C5). The vocabulary is CODE: the closed TS type
// ExternalLane ('urgent' | 'bulk', the server CHECK of A03-W6 / K-03-4 and the
// validator of model-consumer-update), NEVER derived from the dispatch class
// list (DispatchBucket.class carries interactive / external / external_bulk —
// queue names of the dispatcher, not lanes a consumer can be put on).
//
// Field tier (§4.5, V-01-a mirror — the server stays the authority):
//   server-admin            urgent + bulk selectable
//   tenant-admin, app       may only LOWER: on a `bulk` consumer `urgent` is
//                           disabled ("server-admin only"); on an `urgent`
//                           consumer both stay selectable (urgent → bulk lowers)
//   tenant-admin, ctx-peer  locked (peers are managed by the server-admin)
//

import type { ExternalLane } from '../../lib/api/types'

/** One option of the lane select. */
export interface LaneOption {
  value: ExternalLane
  disabled: boolean
  /** Why the option is disabled ("server-admin only", "managed by the server-admin"). */
  note?: string
}

/**
 * Exhaustiveness anchor: a lane added to (or removed from) ExternalLane breaks
 * this object at compile time, so LANE_OPTIONS can never drift from the type.
 */
const LANE_SET: Record<ExternalLane, true> = { urgent: true, bulk: true }

/** Every lane of the closed vocabulary, in display order (urgent first). */
export const LANE_OPTIONS: readonly ExternalLane[] = (['urgent', 'bulk'] as const).filter((l) => LANE_SET[l])

/** True iff `v` is a lane of the closed vocabulary (exact string, no trimming, no case folding). */
export function isExternalLane(v: unknown): v is ExternalLane {
  return typeof v === 'string' && Object.prototype.hasOwnProperty.call(LANE_SET, v)
}

/**
 * The options of the lane select for one consumer as the caller may set them.
 * `current` is the consumer's admission_lane as the server answered it (a bare
 * string). Raising = picking `urgent` while the current lane is not `urgent`
 * (the handler's serverAdminOnly rule) — an unknown current value therefore
 * only allows `bulk`.
 */
export function laneOptions(serverAdmin: boolean, current: string, peer: boolean): LaneOption[] {
  return LANE_OPTIONS.map((value): LaneOption => {
    if (serverAdmin) return { value, disabled: false }
    if (peer) return { value, disabled: true, note: 'managed by the server-admin' }
    if (value === 'urgent' && current !== 'urgent') return { value, disabled: true, note: 'server-admin only' }
    return { value, disabled: false }
  })
}
