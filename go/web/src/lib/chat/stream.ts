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
 * Run one chat turn. Resolves when the stream ends (a done/error event or EOF);
 * the AbortSignal cancels the in-flight request (the server persists the
 * partial with a canceled marker and frees the slot). Throws ApiError only for
 * a PRE-STREAM failure; mid-stream failures are delivered as `error` events.
 */
export async function streamTurn(
  req: StreamRequest,
  key: string | null,
  onEvent: StreamCallback,
  signal: AbortSignal,
): Promise<void> {
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    Accept: 'text/event-stream',
  }
  if (key) headers.Authorization = `Bearer ${key}`

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
    throw new ApiError(res.status, codeFor(res.status), message, requestId)
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
