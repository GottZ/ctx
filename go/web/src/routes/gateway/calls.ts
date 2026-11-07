// Last calls of a consumer (RC-D C6, wave W05-09, design/05 §4.3 W05-03 +
// §7 gate "tenant-admin: kein Link auf /api/llmlog/{id} — rot gegen Link").
// Pure and DOM-free.
//
//   callDetailAllowed(tier)   true ONLY for server-admin: below it the detail
//                             route answers a uniform 404 for every consumer
//                             row, so a link would be a dead end that also
//                             invites body probing.
//   callRows(entries, tier)   display rows: time, model, status text, tokens
//                             ("not recorded" for null, never 0), error as
//                             class + status ONLY (never `detail`), and
//                             `detailId` = the row id for server-admin, null
//                             for everyone else.
//
// RC-D S1: contract stubs — callDetailAllowed answers false (fail-closed),
// callRows answers [].

import type { ConsumerCallEntry } from '../../lib/api/types'
import type { Tier } from '../../lib/auth/capabilities'

/** One display row of the consumer's last calls. */
export interface CallRow {
  id: string
  at: string
  model: string
  pipeline: string
  /** 'ok' or '<class> · <status>' — never the raw error detail. */
  outcome: string
  durationText: string
  tokensText: string
  /** The row id for the server-admin detail view; null below server-admin. */
  detailId: string | null
}

/** Whether the caller may open a call's detail (/api/llmlog/{id}). */
export function callDetailAllowed(tier: Tier | null): boolean {
  void tier
  return false
}

/** The consumer's calls as display rows (body-free, detail link per tier). */
export function callRows(entries: readonly ConsumerCallEntry[], tier: Tier | null): CallRow[] {
  void entries
  void tier
  return []
}
