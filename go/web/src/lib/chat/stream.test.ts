// streamTurn tests (design 06 §3.5): the SSE response is parsed into named
// events, and a pre-stream JSON failure surfaces as ApiError (so the UI shows a
// fresh-turn retry rather than a mid-stream error). fetch is stubbed with a
// ReadableStream body, mirroring sse.svelte.test.ts.

import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from '../api'
import { parseRetryAfter, streamTurn } from './stream'

function sseResponse(...frames: string[]): Response {
  const enc = new TextEncoder()
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const f of frames) controller.enqueue(enc.encode(f))
      controller.close()
    },
  })
  return new Response(stream, { status: 200, headers: { 'Content-Type': 'text/event-stream' } })
}

afterEach(() => vi.unstubAllGlobals())

describe('streamTurn', () => {
  it('dispatches named SSE events in order', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        sseResponse(
          'event: session\ndata: {"session_id":"s1","user_seq":1}\n\n',
          'event: delta\ndata: {"text":"hi"}\n\n',
          'event: done\ndata: {"finish_reason":"stop","assistant_seq":2,"iterations":1,"total_ms":100}\n\n',
        ),
      ),
    )
    const events: Array<[string, unknown]> = []
    await streamTurn({ message: 'hi' }, 'key', (n, d) => events.push([n, d]), new AbortController().signal)

    expect(events.map((e) => e[0])).toEqual(['session', 'delta', 'done'])
    expect(events[0][1]).toEqual({ session_id: 's1', user_seq: 1 })
    expect(events[1][1]).toEqual({ text: 'hi' })
  })

  it('throws ApiError on a pre-stream failure (e.g. 409 busy)', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ success: false, error: 'a turn is already running for this session' }), {
          status: 409,
          headers: { 'Content-Type': 'application/json' },
        }),
      ),
    )
    await expect(
      streamTurn({ message: 'hi' }, 'key', () => {}, new AbortController().signal),
    ).rejects.toMatchObject({ status: 409, code: 'conflict' })
  })

  it('sends the bearer key and the message body', async () => {
    const fetchMock = vi.fn().mockResolvedValue(sseResponse('event: done\ndata: {"finish_reason":"stop"}\n\n'))
    vi.stubGlobal('fetch', fetchMock)
    await streamTurn({ message: 'hello', session_id: 's9' }, 'my-key', () => {}, new AbortController().signal)

    const [url, init] = fetchMock.mock.calls[0]
    expect(url).toBe('/api/chat/stream')
    expect(init.method).toBe('POST')
    expect((init.headers as Record<string, string>).Authorization).toBe('Bearer my-key')
    expect(JSON.parse(init.body as string)).toEqual({ message: 'hello', session_id: 's9' })
  })

  it('returns quietly when aborted before the response', async () => {
    const ctrl = new AbortController()
    ctrl.abort()
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(Object.assign(new Error('aborted'), { name: 'AbortError' })))
    await expect(streamTurn({ message: 'x' }, 'k', () => {}, ctrl.signal)).resolves.toBeUndefined()
  })

  it('guards: ApiError is the thrown type', () => {
    expect(new ApiError(409, 'conflict', 'busy')).toBeInstanceOf(ApiError)
  })

  it('attaches the Retry-After hint to a pre-stream 429 (MW8 B1)', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ success: false, error: 'server busy — retry shortly' }), {
          status: 429,
          headers: { 'Content-Type': 'application/json', 'Retry-After': '7' },
        }),
      ),
    )
    await expect(streamTurn({ message: 'x' }, 'k', () => {}, new AbortController().signal)).rejects.toMatchObject({
      status: 429,
      code: 'rate_limited',
      details: { retry_after: 7 },
    })
  })

  it('omits the hint on a 429 without a Retry-After header (honest absence)', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        new Response(JSON.stringify({ success: false, error: 'a turn is already running for this scope' }), {
          status: 429,
          headers: { 'Content-Type': 'application/json' },
        }),
      ),
    )
    await expect(streamTurn({ message: 'x' }, 'k', () => {}, new AbortController().signal)).rejects.toMatchObject({
      status: 429,
      code: 'rate_limited',
      details: null,
    })
  })
})

describe('parseRetryAfter', () => {
  const res = (headers: Record<string, string>): Response => new Response(null, { status: 429, headers })

  it('parses a positive integer delta-seconds', () => {
    expect(parseRetryAfter(res({ 'Retry-After': '12' }))).toBe(12)
  })

  it('ceils a fractional value to whole seconds', () => {
    expect(parseRetryAfter(res({ 'Retry-After': '3.2' }))).toBe(4)
  })

  it('returns null for an absent header', () => {
    expect(parseRetryAfter(res({}))).toBeNull()
  })

  it('returns null for the HTTP-date form (not emitted by ctxd) and non-positive values', () => {
    expect(parseRetryAfter(res({ 'Retry-After': 'Wed, 21 Oct 2026 07:28:00 GMT' }))).toBeNull()
    expect(parseRetryAfter(res({ 'Retry-After': '0' }))).toBeNull()
    expect(parseRetryAfter(res({ 'Retry-After': '-3' }))).toBeNull()
  })
})
