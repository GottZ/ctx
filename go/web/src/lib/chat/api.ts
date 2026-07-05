// Web-chat session API (design 06 §3.3). The JSON routes (list / detail /
// delete) go through apiFetch; the streaming turn (POST /api/chat/stream) is a
// separate path in stream.ts — apiFetch parses JSON, the stream is SSE.

import { apiFetch } from '../api'
import type { ChatSessionDetailResponse, ChatSessionListResponse, ConfirmResponse } from './types'

/** GET /api/chat/sessions — home-scope sessions, newest first (metadata only). */
export function listSessions(limit = 50): Promise<ChatSessionListResponse> {
  return apiFetch<ChatSessionListResponse>(`/api/chat/sessions?limit=${limit}`)
}

/**
 * GET /api/chat/sessions/{id} — one session + its messages (full tool-result
 * contents). 404 (ApiError, code 'not_found') on a foreign/unknown session.
 * Pagination is additive: afterSeq/limit default to "all".
 */
export function getSession(id: string, afterSeq = 0, limit = 0): Promise<ChatSessionDetailResponse> {
  const q = new URLSearchParams()
  if (afterSeq > 0) q.set('after_seq', String(afterSeq))
  if (limit > 0) q.set('limit', String(limit))
  const qs = q.toString()
  return apiFetch<ChatSessionDetailResponse>(`/api/chat/sessions/${encodeURIComponent(id)}${qs ? `?${qs}` : ''}`)
}

/** DELETE /api/chat/sessions/{id} — hard delete (messages cascade). */
export function deleteSession(id: string): Promise<{ success: true }> {
  return apiFetch<{ success: true }>(`/api/chat/sessions/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

/**
 * POST /api/confirm — execute a staged write (F6-C6 D-W6b). Auth is the
 * Bearer header apiFetch always sets (no cookie path exists, D1-m4). Misses
 * (expired / consumed / foreign / unknown) are a generic 404 ApiError.
 */
export function confirmWrite(payloadHash: string): Promise<ConfirmResponse> {
  return apiFetch<ConfirmResponse>('/api/confirm', {
    method: 'POST',
    body: JSON.stringify({ payload_hash: payloadHash }),
  })
}
