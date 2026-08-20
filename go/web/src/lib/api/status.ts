// Status dashboard client (design 04-§3.2/§3.6, W6). GET /api/status +
// /api/llmlog are admin-only reads; the dream/gaming toggles reuse the existing
// POST /api/manage actions (admin-gated server-side). Public /health is the
// non-admin degradation path (no auth, name-free).

import { apiFetch } from '../api'
import type {
  DreamBackoffRestampResponse,
  DreamStatsResponse,
  HealthStatus,
  LLMLogDetailResponse,
  LLMLogResponse,
  StatusResponse,
} from './types'

export function fetchStatus(): Promise<StatusResponse> {
  return apiFetch<StatusResponse>('/api/status')
}

export interface LLMLogQuery {
  limit?: number
  pipeline?: string
  errorsOnly?: boolean
}

export function fetchLLMLog(q: LLMLogQuery = {}): Promise<LLMLogResponse> {
  const p = new URLSearchParams()
  if (q.limit !== undefined) p.set('limit', String(q.limit))
  if (q.pipeline) p.set('pipeline', q.pipeline)
  if (q.errorsOnly) p.set('errors_only', 'true')
  const qs = p.toString()
  return apiFetch<LLMLogResponse>(`/api/llmlog${qs ? `?${qs}` : ''}`)
}

/**
 * Gated per-id prompt/reply body fetch (D1b) behind a history-row click. The
 * bodies live ONLY here (the list is body-free); an unknown or foreign id is a
 * 404 ApiError (no existence oracle), a sealed/evicted row returns 200 with
 * null bodies + a body_state reason. The caller must NOT cache the returned
 * bodies — they are the sensitive shadow-corpus this endpoint gates.
 */
export function fetchLLMLogDetail(id: string): Promise<LLMLogDetailResponse> {
  return apiFetch<LLMLogDetailResponse>(`/api/llmlog/${encodeURIComponent(id)}`)
}

export type DreamMode = 'on' | 'throttled' | 'off'

// dream-mode mutation answer (U01-W7): carries as_of so the DreamTile splices it
// into the held status against the asOfFloor instead of a stale reload (§4.5-4).
export interface DreamModeResponse {
  success: true
  mode: DreamMode
  interval: number
  as_of: string
}

/**
 * Fetch the dream back-off distribution (dream-stats manage action — the same
 * source `ctx dream stats` renders, so CLI and web stay in parity). Server-side
 * this runs an O(n) GROUP BY over context_blocks (R12 names the 1M+ follow-up),
 * so callers throttle: DreamBackoffModel refetches only when last_cycle_at
 * moves and at most once per its min interval.
 */
export function fetchDreamStats(): Promise<DreamStatsResponse> {
  return apiFetch<DreamStatsResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'dream-stats' }),
  })
}

/**
 * Re-evaluate the back-off pipeline under the CURRENT policy (manage action;
 * admin-gated server-side): every non-transient cooldown stamp is recomputed
 * as dream_checked_at + curve(eval_count, last_inert). The settings curve
 * editor calls this right after a Save of dream.backoff_* keys so the policy
 * change governs the whole corpus immediately, not only future cycles.
 */
export function restampDreamBackoff(): Promise<DreamBackoffRestampResponse> {
  return apiFetch<DreamBackoffRestampResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'dream-backoff-restamp' }),
  })
}

/** Toggle the dream scheduler mode (manage action; admin-gated server-side). */
export function setDreamMode(mode: DreamMode): Promise<DreamModeResponse> {
  return apiFetch<DreamModeResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'dream-mode', data: { mode } }),
  })
}

/**
 * Public /health for the non-admin degraded tile. Bypasses apiFetch (no auth,
 * and we want the body even on the 503 unhealthy path).
 */
export async function fetchPublicHealth(): Promise<HealthStatus> {
  const res = await fetch('/health', { headers: { Accept: 'application/json' } })
  return (await res.json()) as HealthStatus
}
