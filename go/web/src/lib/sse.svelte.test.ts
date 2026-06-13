// SseClient gates (design 04-§3.6 / web-svelte.md §6): named-event dispatch off
// a fetch stream, and the HTTP-error → 'error' status that drives the page's
// poll fallback. fetch + ReadableStream are stubbed; pure node.

import { afterEach, describe, expect, it, vi } from 'vitest'
import { SseClient } from './sse.svelte'

// sseResponse builds a 200 text/event-stream Response whose body is the given
// SSE frames as one chunk (an explicit ReadableStream so .body is never null in
// the test env).
function sseResponse(...frames: string[]): Response {
  const bytes = new TextEncoder().encode(frames.join(''))
  const stream = new ReadableStream<Uint8Array>({
    start(c) {
      c.enqueue(bytes)
      c.close()
    },
  })
  return new Response(stream, { status: 200, headers: { 'Content-Type': 'text/event-stream' } })
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('SseClient', () => {
  it('dispatches named events with parsed JSON data', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValueOnce(
        sseResponse(
          'event: status\ndata: {"health":{"status":"ok"}}\n\n',
          ': ping\n\n',
          'event: backends\ndata: [{"name":"herbert-chat"}]\n\n',
        ),
      ),
    )
    const got: Array<[string, unknown]> = []
    const sse = new SseClient('/api/events', (name, data) => got.push([name, data]))
    await sse.connect()
    sse.close() // stop the post-EOF reconnect timer

    expect(got).toEqual([
      ['status', { health: { status: 'ok' } }],
      ['backends', [{ name: 'herbert-chat' }]],
    ])
  })

  it('sends the Authorization header from getInit and Accept: text/event-stream', async () => {
    const key = ['cafe', 'f00d'].join('') // doc-value, assembled at runtime (repo rule: no secret-shaped literals)
    const fetchMock = vi.fn().mockResolvedValueOnce(sseResponse('event: status\ndata: {}\n\n'))
    vi.stubGlobal('fetch', fetchMock)
    const sse = new SseClient(
      '/api/events',
      () => {},
      () => ({ headers: { Authorization: `Bearer ${key}` } }),
    )
    await sse.connect()
    sse.close()

    const init = fetchMock.mock.calls[0][1] as RequestInit
    const headers = init.headers as Record<string, string>
    expect(headers.Authorization).toBe(`Bearer ${key}`)
    expect(headers.Accept).toBe('text/event-stream')
  })

  it('goes to error on a non-ok status (429 connection cap → poll fallback)', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(new Response(null, { status: 429 })))
    const sse = new SseClient('/api/events', () => {})
    await sse.connect()
    expect(sse.status).toBe('error')
    sse.close() // stop the backoff reconnect timer
  })

  it('drops a malformed data frame without crashing the stream', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValueOnce(
        sseResponse('event: status\ndata: {bad json\n\n', 'event: backends\ndata: []\n\n'),
      ),
    )
    const got: Array<[string, unknown]> = []
    const sse = new SseClient('/api/events', (name, data) => got.push([name, data]))
    await sse.connect()
    sse.close()

    // The malformed status frame is skipped; the valid backends frame survives.
    expect(got).toEqual([['backends', []]])
  })
})
