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
// RC-D S1: contract stub — fail-closed (no option, nothing is a lane). The
// tests in lane.test.ts are red against it; S2 implements.

import type { ExternalLane } from '../../lib/api/types'

/** One option of the lane select. */
export interface LaneOption {
  value: ExternalLane
  disabled: boolean
  /** Why the option is disabled ("server-admin only", "managed by the server-admin"). */
  note?: string
}

/** Every lane of the closed vocabulary, in display order (urgent first). S1 stub: empty. */
export const LANE_OPTIONS: readonly ExternalLane[] = []

/** True iff `v` is a lane of the closed vocabulary. S1 stub: nothing is. */
export function isExternalLane(_v: unknown): _v is ExternalLane {
  return false
}

/**
 * The options of the lane select for one consumer as the caller may set them.
 * `current` is the consumer's admission_lane as the server answered it (a bare
 * string — an unknown value is shown, never offered). S1 stub: no option.
 */
export function laneOptions(_serverAdmin: boolean, _current: string, _peer: boolean): LaneOption[] {
  return []
}
