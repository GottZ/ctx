// Guard review-queue manage client (needs_review pipeline W4). The /guard
// route works the write guard's flagged blocks through the scope-gated
// POST /api/manage guard-list / guard-resolve actions. guard-resolve carries
// either one id (single wire shape, unchanged since v1) or data.ids[] (the W1
// batch contract: every id accounted for — resolved XOR skipped+reason).

import { apiFetch } from '../api'

export interface GuardListItem {
  id: string
  title: string
  category: string
  scope: string
  type_name: string
  guard_status: string
  similarity: string | null
  matched_id: string | null
  matched_title: string | null
  checked_at: string | null
  updated_at: string
}

export interface GuardListResponse {
  success: boolean
  count: number
  blocks: GuardListItem[]
}

export interface GuardSkip {
  id: string
  reason: 'invalid_id' | 'not_found' | 'already_archived' | 'not_flagged'
}

export interface GuardResolveBatchResponse {
  success: boolean
  resolution: 'archive' | 'keep'
  resolved_count: number
  skipped_count: number
  skipped: GuardSkip[]
}

export interface GuardListParams {
  status?: string
  category?: string
  types?: string[]
  limit?: number
}

export function guardList(params: GuardListParams = {}): Promise<GuardListResponse> {
  return apiFetch<GuardListResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'guard-list', ...params }),
  })
}

/** Resolve one or many flagged blocks. Always uses the batch wire shape so the
 * caller gets the uniform resolved/skipped accounting (a single id is just a
 * batch of one). */
export function guardResolve(ids: string[], resolution: 'archive' | 'keep'): Promise<GuardResolveBatchResponse> {
  return apiFetch<GuardResolveBatchResponse>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action: 'guard-resolve', data: { ids, resolution } }),
  })
}
