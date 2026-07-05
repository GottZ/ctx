// Hand-maintained wire types for the web-chat surface (design 06 §3.3/§3.5).
// No OpenAPI spec — the Go handler is the source of truth. One source comment
// per type, mirroring lib/api/types.ts.

import type { ApiError } from '../api'

export type Sensitivity = 'public' | 'internal' | 'personal' | 'credentials'

// Source: go/internal/handler/chat.go (chatSessionView) — metadata only, no
// read_scopes/created_by (internal).
export interface ChatSessionView {
  id: string
  title: string
  scope: string
  max_sensitivity: Sensitivity
  created_at: string
  updated_at: string
}

// Source: chatSessionListItem — view + the message_count navigation anchor.
export interface ChatSessionListItem extends ChatSessionView {
  message_count: number
}

// Source: GET /api/chat/sessions (HandleListSessions).
export interface ChatSessionListResponse {
  success: true
  sessions: ChatSessionListItem[]
}

// Source: llm.ToolCall — persisted assistant tool call. `arguments` is a JSON
// STRING (OpenAI wire form), parsed lazily by the UI.
export interface PersistedToolCall {
  id: string
  type: string
  function: { name: string; arguments: string }
}

// Source: chatMessageView — one message in the detail response. tool_calls is
// raw JSON (the handler embeds it via json.RawMessage, not base64).
export interface ChatMessage {
  seq: number
  role: 'user' | 'assistant' | 'tool'
  content: string
  sensitivity: Sensitivity
  tool_calls?: PersistedToolCall[]
  tool_call_id?: string
  tool_name?: string
  backend?: string
  model?: string
  prompt_tokens?: number
  completion_tokens?: number
  duration_ms?: number
  metadata?: Record<string, unknown>
  created_at: string
}

// Source: GET /api/chat/sessions/{id} (HandleGetSession).
export interface ChatSessionDetailResponse {
  success: true
  session: ChatSessionView
  messages: ChatMessage[]
}

// Source: POST /api/chat/stream request body (streamRequest).
export interface StreamRequest {
  session_id?: string
  message: string
  sensitivity?: Sensitivity
  tools_enabled?: boolean
  max_tokens?: number
}

// ── SSE event payloads (design 06 §3.5). The event name is the SSE `event:`
// field; these are the `data:` JSON shapes. Unknown event types are ignored
// (forward-compat for §3.8 pending_action).

export interface SessionEvent {
  session_id: string
  user_seq: number
}
export interface BackendEvent {
  backend: string
  model: string
  trust: string
  tools_active: boolean
  fallback: boolean
}
export interface DeltaEvent {
  text: string
}
export interface ToolCallStartEvent {
  iteration: number
  name: string
}
export interface ToolCallEvent {
  iteration: number
  id: string
  name: string
  arguments: Record<string, unknown>
}
export interface ToolResultBlock {
  id: string
  title: string
  category: string
  score?: number
}
// Source: chat.StagedWrite (tools.go, D-W6b) — the ConfirmCard payload of a
// staged ctx_store write. Rides on tool_result events AND inside persisted
// tool message content (render.ts reconstructs it after a session reload).
export interface StagedWriteInfo {
  payload_hash: string
  op: string // 'store' | 'update' (D-W6c)
  scope: string
  category: string
  title: string
  sensitivity: string
  content_preview: string
  content_chars: number
  expires_at: string | null
  // update form only (op 'update', D-W6c): target block + the authoritative
  // field list the staged update writes.
  target_id?: string
  update_fields?: string[]
}

export interface ToolResultEvent {
  iteration: number
  id: string
  name: string
  ok: boolean
  duration_ms: number
  chars: number
  truncated: boolean
  summary: string
  blocks: ToolResultBlock[] | null
  staged?: StagedWriteInfo | null
}

// Source: POST /api/confirm (handler/confirm.go, D-W6b; op since D-W6c).
export interface ConfirmResponse {
  success: true
  op?: string // 'store' | 'update'
  block: { id: string; title: string; category: string; scope: string }
}
export interface UsageEvent {
  iteration: number
  prompt_tokens: number
  completion_tokens: number
  draft_accept: number
}
export interface DoneEvent {
  finish_reason: 'stop' | 'tool_limit' | 'budget' | 'length' | 'canceled'
  assistant_seq: number
  iterations: number
  total_ms: number
}
export interface ErrorEvent {
  code: string
  backend?: string
  retryable: boolean
}

// What the stream reader hands to the store. A `fail` is a pre-stream JSON
// failure (4xx, surfaced as ApiError) so the UI distinguishes "never started"
// (retryable as a fresh turn) from a mid-stream `error` event.
export type StreamCallback = (name: string, data: unknown) => void
export type StreamFailure = ApiError
