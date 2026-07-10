// Chat turn streamer (design 06 §3.5). POST /api/chat/stream with the message
// body and read the SSE response — fetch + ReadableStream + eventsource-parser,
// the same wire mechanics as SseClient (sse.svelte.ts) with one deliberate
// difference: NO reconnect. A turn is a one-shot; a reconnect would re-POST and
// re-run the whole turn (double tool execution, double answer). Abort is final.
//
// Pre-stream failures (the server answers JSON 4xx/404/409/429/503 before the
// first event) surface as ApiError so the caller can show a fresh-turn retry;
// once the stream starts, failures arrive as `error` SSE events instead.

import { createParser, type EventSourceMessage } from 'eventsource-parser'
import { ApiError, codeFor } from '../api'
import type { StreamCallback, StreamRequest } from './types'

/**
 * Parse an RFC 7231 `Retry-After` as integer delta-seconds (the dispatcher
 * sends a clamped 1..30, or omits the header — design/03 §3.3 / dispatch_reject.go).
 * Returns null for an absent / non-numeric / non-positive value: an HONEST
 * absence the UI renders as the generic "system busy" state rather than a
 * fabricated countdown (mirrors the server's "no snapshot ⇒ no value" rule).
 * The HTTP-date form is not emitted by ctxd, so it is treated as absent too.
 */
export function parseRetryAfter(res: Response): number | null {
  const raw = res.headers.get('Retry-After')
  if (raw === null) return null
  const secs = Number(raw.trim())
  if (!Number.isFinite(secs) || secs <= 0) return null
  return Math.ceil(secs)
}

/**
 * Run one chat turn. Resolves when the stream ends (a done/error event or EOF);
 * the AbortSignal cancels the in-flight request (the server persists the
 * partial with a canceled marker and frees the slot). Throws ApiError only for
 * a PRE-STREAM failure; mid-stream failures are delivered as `error` events.
 */
export async function streamTurn(
  req: StreamRequest,
  csrfToken: string | null,
  onEvent: StreamCallback,
  signal: AbortSignal,
): Promise<void> {
  // Auth fährt als Session-Cookie mit (same-origin); der POST braucht den
  // CSRF-Synchronizer als Header (design 05 §4.4, OAuth R4).
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    Accept: 'text/event-stream',
  }
  if (csrfToken) headers['X-CSRF-Token'] = csrfToken

  let res: Response
  try {
    res = await fetch('/api/chat/stream', { method: 'POST', headers, body: JSON.stringify(req), signal })
  } catch (cause) {
    if (signal.aborted) return // user aborted before the response — not an error
    const detail = cause instanceof Error ? cause.message : String(cause)
    throw new ApiError(0, 'network', `ctxd unreachable: ${detail}`)
  }

  const requestId = res.headers.get('X-Request-ID')
  // Pre-stream failure: the endpoint answered a JSON error, not a stream.
  if (!res.ok || !res.body) {
    const body = (await res.json().catch(() => null)) as { error?: string } | null
    const message = body?.error ?? `chat stream failed (HTTP ${res.status})`
    // A 429 is a capacity signal (dispatcher rejection / per-scope turn cap):
    // carry the Retry-After hint (when present) so the caller can drive a
    // countdown + jittered auto-retry instead of a hard "turn failed".
    const retryAfter = res.status === 429 ? parseRetryAfter(res) : null
    const details = retryAfter !== null ? { retry_after: retryAfter } : null
    throw new ApiError(res.status, codeFor(res.status), message, requestId, details)
  }

  const parser = createParser({
    onEvent: (ev: EventSourceMessage) => {
      let data: unknown = null
      try {
        data = ev.data ? JSON.parse(ev.data) : null
      } catch {
        return // a malformed frame is dropped, never crashes the stream
      }
      onEvent(ev.event ?? 'message', data)
    },
  })

  const reader = res.body.pipeThrough(new TextDecoderStream()).getReader()
  for (;;) {
    let chunk: ReadableStreamReadResult<string>
    try {
      chunk = await reader.read()
    } catch {
      return // aborted mid-stream — the store already holds the partial
    }
    if (chunk.done) break
    parser.feed(chunk.value)
  }
}
