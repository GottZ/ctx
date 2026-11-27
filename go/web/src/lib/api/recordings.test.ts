// recordings.ts binding gate (#575): every action hits POST /api/manage with
// exactly its action and the payload fields of the contract (docs/api.md
// "Recordings of a recording consumer"), translate carries no text field, and
// the AbortSignal of read and translate reaches fetch itself — Stop and
// leaving the page cancel the request, so the server cancels the model call
// and the person's translation brake frees at once. Pure node, fetch stubbed.

import { afterEach, describe, expect, it, vi } from 'vitest'
import { getRecording, readRecording, translateRecording } from './recordings'

function stubFetch(body: unknown): ReturnType<typeof vi.fn> {
  const mock = vi.fn().mockResolvedValue(
    new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } }),
  )
  vi.stubGlobal('fetch', mock)
  return mock
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('recordings client — the abort signal reaches fetch', () => {
  it('translate: POST /api/manage with the block address, never text, and the signal', async () => {
    const mock = stubFetch({ success: true, text: 'x', chunk: 0, chunks: 1 })
    const ctrl = new AbortController()
    await translateRecording({ id: 'r1', address: 'request/messages/1', language: 'de', chunk: 0 }, ctrl.signal)
    expect(mock).toHaveBeenCalledTimes(1)
    const [path, init] = mock.mock.calls[0] as [string, RequestInit]
    expect(path).toBe('/api/manage')
    expect(init.method).toBe('POST')
    expect(init.signal, 'the signal is fetch’s own, so an abort cancels the request').toBe(ctrl.signal)
    const sent = JSON.parse(String(init.body)) as { action: string; data: Record<string, unknown> }
    expect(sent.action).toBe('model-recording-translate')
    expect(Object.keys(sent.data).sort()).toEqual(['address', 'chunk', 'id', 'language'])
  })

  it('get: the first page is the id alone, a later page names block_offset (and block_limit when given), the signal reaches fetch', async () => {
    const answer = { success: true, recording: {}, request: {}, response: {}, blocks: [], block_offset: 0, blocks_total: 0 }
    const mock = stubFetch(answer)
    const ctrl = new AbortController()
    await getRecording('r1', { signal: ctrl.signal })
    let [path, init] = mock.mock.calls[0] as [string, RequestInit]
    expect(path).toBe('/api/manage')
    expect(init.signal, 'leaving the detail page cancels the read').toBe(ctrl.signal)
    expect(JSON.parse(String(init.body))).toEqual({ action: 'model-recording-get', data: { id: 'r1' } })
    mock.mockResolvedValue(new Response(JSON.stringify(answer), { status: 200, headers: { 'Content-Type': 'application/json' } }))
    await getRecording('r1', { blockOffset: 500, blockLimit: 100 })
    ;[, init] = mock.mock.calls[1] as [string, RequestInit]
    expect(JSON.parse(String(init.body))).toEqual({ action: 'model-recording-get', data: { id: 'r1', block_offset: 500, block_limit: 100 } })
    expect(init.signal).toBeUndefined()
  })

  it('read: the signal reaches fetch', async () => {
    const mock = stubFetch({ success: true, data: '', total: 0, offset: 0, limit: 0, eof: true })
    const ctrl = new AbortController()
    await readRecording({ id: 'r1', address: 'request', offset: 0, limit: 10 }, ctrl.signal)
    const [, init] = mock.mock.calls[0] as [string, RequestInit]
    expect(init.signal).toBe(ctrl.signal)
  })
})
