// Last calls of a consumer (RC-D C6, wave W05-09, design/05 §4.3 W05-03 +
// §7 gate "tenant-admin: kein Link auf /api/llmlog/{id} — rot gegen Link").
// Pure and DOM-free.
//
//   callDetailAllowed(tier)   true ONLY for server-admin: below it the detail
//                             route answers a uniform 404 for every consumer
//                             row (C2 W06-17), so a link would be a dead end
//                             that also invites body probing.
//   callRows(entries, tier)   display rows: time, model, outcome, duration and
//                             tokens ("not recorded" for null, never 0), the
//                             error as its CLASS only (never `detail`: the
//                             capped raw text can carry a provider body with
//                             prompt fragments, design/05 §4.3), and
//                             `detailId` = the row id for server-admin, null
//                             for everyone else.

import type { ConsumerCallEntry } from '../../lib/api/types'
import type { Tier } from '../../lib/auth/capabilities'
import { fmtMs } from '../status/dispatch'

/** What a missing value reads as (never "0"). */
const NOT_RECORDED = 'not recorded'

/** One display row of the consumer's last calls. */
export interface CallRow {
  id: string
  at: string
  model: string
  pipeline: string
  /** 'ok' or the error class — never the raw error detail. */
  outcome: string
  durationText: string
  tokensText: string
  /** The row id for the server-admin detail view; null below server-admin. */
  detailId: string | null
}

/** Whether the caller may open a call's detail (/api/llmlog/{id}). */
export function callDetailAllowed(tier: Tier | null): boolean {
  return tier === 'server-admin'
}

function tokensText(e: ConsumerCallEntry): string {
  if (e.prompt_tokens === null || e.completion_tokens === null) return NOT_RECORDED
  return `${e.prompt_tokens.toLocaleString('en-US')} → ${e.completion_tokens.toLocaleString('en-US')}`
}

/** The consumer's calls as display rows (body-free, detail link per tier). */
export function callRows(entries: readonly ConsumerCallEntry[], tier: Tier | null): CallRow[] {
  const detail = callDetailAllowed(tier)
  return entries.map((e) => ({
    id: e.id,
    at: e.created_at,
    model: e.model,
    pipeline: e.pipeline,
    outcome: e.error === null ? 'ok' : e.error.class,
    durationText: e.duration_ms === null ? NOT_RECORDED : fmtMs(e.duration_ms),
    tokensText: tokensText(e),
    detailId: detail ? e.id : null,
  }))
}
