// Recordings view models (issue #575) with an injected api, like
// consumer.svelte.test.ts: the consumers overview, the keyset list with its
// filters and the scan-limit continuation, the detail with its blocks, the
// window reader (64 KiB windows, one streaming TextDecoder so a character
// split across windows stays whole, hex for bytes that are not UTF-8), and
// the translation (chunk 0..n-1 in turn, Stop / leaving / a new translation
// abort the running one, every error code a named state). Nothing of a
// recording or a translation reaches browser storage.

import { readFileSync } from 'node:fs'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from '../../lib/api'
import type {
  RecordingBlock,
  RecordingConsumersResponse,
  RecordingGetResponse,
  RecordingListResponse,
  RecordingMeta,
  RecordingReadResponse,
  RecordingTranslateResponse,
} from '../../lib/api/types'
import { listRecordings, type RecordingListParams, type RecordingReadParams, type RecordingTranslateParams } from '../../lib/api/recordings'
import {
  BlockModel,
  EMPTY_RECORDING_FILTER,
  RecordingConsumersModel,
  RecordingDetailModel,
  RecordingListModel,
  TranslationModel,
  WindowReader,
} from './recordings.svelte'

const RID = '01900000-0000-7000-8000-0000000000r1'
const CID = '01900000-0000-7000-8000-00000000c0a6'

afterEach(() => {
  vi.unstubAllGlobals()
})

function meta(id: string, over: Partial<RecordingMeta> = {}): RecordingMeta {
  return {
    id,
    consumer_id: CID,
    created_at: '2026-06-29T11:00:00Z',
    finished_at: '2026-06-29T11:00:01Z',
    process_id: 'p1',
    model_key_id: 'k1',
    path: '/v1/chat/completions',
    op: 'chat',
    client_request_id: null,
    model: 'chat',
    stream: false,
    declared_bytes: 120,
    request_bytes: 120,
    request_complete: true,
    state: 'done',
    status: 200,
    code: null,
    response_bytes: 300,
    request_cleared: false,
    response_null: false,
    ...over,
  }
}

function block(address: string, over: Partial<RecordingBlock> = {}): RecordingBlock {
  return {
    address,
    side: 'request',
    kind: 'text',
    role: 'user',
    bytes: 5,
    utf8: true,
    translatable: true,
    inline: true,
    preview: 'hello',
    preview_truncated: false,
    preview_omitted: false,
    ...over,
  }
}

function page(ids: string[], next: string | null, over: Partial<RecordingListResponse> = {}): RecordingListResponse {
  return { success: true, recordings: ids.map((i) => meta(i)), next_cursor: next, scanned: ids.length + 1, scan_limit_reached: false, ...over }
}

function b64(bytes: Uint8Array): string {
  return Buffer.from(bytes).toString('base64')
}

function window(bytes: Uint8Array, offset: number, total: number, utf8 = true): RecordingReadResponse {
  return { success: true, address: 'a', offset, limit: bytes.length, total, eof: offset + bytes.length >= total, utf8, data: b64(bytes) }
}

function tr(chunk: number, chunks: number, text: string): RecordingTranslateResponse {
  return { success: true, id: RID, address: 'request/messages/0', chunk, chunks, language: 'de', text, model: 'qwen38-27b', generated: true }
}

function envelope(status: number, code: string, extra: Record<string, unknown> = {}): ApiError {
  return new ApiError(status, 'x', `text ${code}`, 'rq', { success: false, error: `text ${code}`, code, ...extra })
}

// ---------------------------------------------------------------------------

describe('RecordingConsumersModel', () => {
  const ANSWER: RecordingConsumersResponse = {
    success: true,
    process_id: 'p1',
    since: '2026-06-29T10:00:00Z',
    consumers: [
      {
        consumer_id: CID,
        label: 'rec-app',
        tenant_id: 't',
        tenant_slug: 'acme',
        record_bodies: true,
        active: true,
        deleted: false,
        last_recorded_at: '2026-06-29T11:00:00Z',
        process: { admitted: 3, in_flight: 1, recorded: 2, unrecordable: 0, unfinished: 0, stream_answers_unrecorded: 0 },
      },
    ],
  }

  it('loads the consumers with this process’s counts', async () => {
    const consumers = vi.fn().mockResolvedValue(ANSWER)
    const m = new RecordingConsumersModel({ consumers })
    await m.load()
    expect(consumers).toHaveBeenCalledTimes(1)
    expect(m.status).toBe('ready')
    expect(m.consumers.map((c) => c.label)).toEqual(['rec-app'])
    expect(m.consumers[0]?.process?.recorded).toBe(2)
    expect(m.processId).toBe('p1')
    expect(m.since).toBe('2026-06-29T10:00:00Z')
  })

  it('a failing read is the error state', async () => {
    const m = new RecordingConsumersModel({ consumers: vi.fn().mockRejectedValue(new ApiError(500, 'server', 'boom')) })
    await m.load()
    expect(m.status).toBe('error')
    expect(m.loadError?.status).toBe(500)
    expect(m.consumers).toEqual([])
  })
})

// ---------------------------------------------------------------------------

describe('listRecordings request payload (the wire)', () => {
  function stub(): ReturnType<typeof vi.fn> {
    const f = vi.fn().mockResolvedValue(new Response(JSON.stringify(page([], null)), { status: 200, headers: { 'Content-Type': 'application/json' } }))
    vi.stubGlobal('fetch', f)
    return f
  }
  function body(f: ReturnType<typeof vi.fn>): Record<string, unknown> {
    return JSON.parse(String((f.mock.calls[0]?.[1] as RequestInit).body)) as Record<string, unknown>
  }

  it('only consumer_id without a filter — no empty fields on the wire', async () => {
    const f = stub()
    await listRecordings({ consumerId: CID })
    expect(body(f)).toEqual({ action: 'model-recording-list', data: { consumer_id: CID } })
  })

  it('every filter field by its wire name', async () => {
    const f = stub()
    await listRecordings({ consumerId: CID, cursor: 'cur', since: 'S', until: 'U', ops: ['chat', 'none'], state: 'open', status: '4xx', limit: 7 })
    expect(body(f)).toEqual({
      action: 'model-recording-list',
      data: { consumer_id: CID, cursor: 'cur', since: 'S', until: 'U', op: ['chat', 'none'], state: 'open', status: '4xx', limit: 7 },
    })
  })
})

describe('RecordingListModel', () => {
  it('first page without a cursor, the filter as parameters, newest first as served', async () => {
    const list = vi.fn().mockResolvedValue(page(['r3', 'r2'], 'c1'))
    const m = new RecordingListModel({ list })
    await m.load(CID, {
      since: '2026-06-29 10:00',
      until: '',
      ops: ['chat', 'embed'],
      state: 'done',
      status: '2xx',
    })
    const p = list.mock.calls[0]?.[0] as RecordingListParams
    expect(p.consumerId).toBe(CID)
    expect(p.cursor).toBeUndefined()
    expect(p.since).toBe(new Date(2026, 5, 29, 10, 0).toISOString())
    expect(p.until).toBeUndefined()
    expect(p.ops).toEqual(['chat', 'embed'])
    expect(p.state).toBe('done')
    expect(p.status).toBe('2xx')
    expect(m.recordings.map((r) => r.id)).toEqual(['r3', 'r2'])
    expect(m.nextCursor).toBe('c1')
    expect(m.status).toBe('ready')
  })

  it('an empty filter sends none of its fields', async () => {
    const list = vi.fn().mockResolvedValue(page([], null))
    const m = new RecordingListModel({ list })
    await m.load(CID, EMPTY_RECORDING_FILTER)
    const p = list.mock.calls[0]?.[0] as RecordingListParams
    expect(p).toEqual({ consumerId: CID })
  })

  it('Load more appends the next page with the opaque cursor, the filter unchanged', async () => {
    const list = vi.fn().mockResolvedValueOnce(page(['r3', 'r2'], 'c1')).mockResolvedValueOnce(page(['r1'], null))
    const m = new RecordingListModel({ list })
    await m.load(CID, { ...EMPTY_RECORDING_FILTER, ops: ['rerank'] })
    await m.loadMore()
    const p2 = list.mock.calls[1]?.[0] as RecordingListParams
    expect(p2.cursor).toBe('c1')
    expect(p2.ops).toEqual(['rerank'])
    expect(m.recordings.map((r) => r.id)).toEqual(['r3', 'r2', 'r1'])
    expect(m.nextCursor).toBeNull()
    await m.loadMore()
    expect(list, 'at the end of the line Load more sends nothing').toHaveBeenCalledTimes(2)
  })

  it('scan limit reached: the rows searched are named and Continue goes on from the walked row', async () => {
    const list = vi
      .fn()
      .mockResolvedValueOnce(page([], 'walk1', { scanned: 2000, scan_limit_reached: true }))
      .mockResolvedValueOnce(page(['r9'], 'walk2', { scanned: 2000, scan_limit_reached: true }))
      .mockResolvedValueOnce(page(['r8'], null, { scanned: 120, scan_limit_reached: false }))
    const m = new RecordingListModel({ list })
    await m.load(CID, { ...EMPTY_RECORDING_FILTER, status: '5xx' })
    expect(m.recordings).toEqual([])
    expect(m.scanLimitReached).toBe(true)
    expect(m.scanned).toBe(2000)
    expect(m.nextCursor).toBe('walk1')
    await m.loadMore()
    expect((list.mock.calls[1]?.[0] as RecordingListParams).cursor).toBe('walk1')
    expect((list.mock.calls[1]?.[0] as RecordingListParams).status).toBe('5xx')
    expect(m.recordings.map((r) => r.id)).toEqual(['r9'])
    expect(m.scanned, 'rows searched add up since the filter was applied').toBe(4000)
    await m.loadMore()
    expect(m.recordings.map((r) => r.id)).toEqual(['r9', 'r8'])
    expect(m.scanLimitReached).toBe(false)
    expect(m.scanned).toBe(4120)
  })

  it('a new filter starts over: no cursor, the list and the count reset', async () => {
    const list = vi
      .fn()
      .mockResolvedValueOnce(page(['r3'], 'c1', { scanned: 2000, scan_limit_reached: true }))
      .mockResolvedValueOnce(page(['r1'], null))
    const m = new RecordingListModel({ list })
    await m.load(CID, EMPTY_RECORDING_FILTER)
    await m.setFilter({ ...EMPTY_RECORDING_FILTER, state: 'open' })
    const p2 = list.mock.calls[1]?.[0] as RecordingListParams
    expect(p2.cursor).toBeUndefined()
    expect(p2.state).toBe('open')
    expect(m.recordings.map((r) => r.id)).toEqual(['r1'])
    expect(m.scanned).toBe(2)
    expect(m.scanLimitReached).toBe(false)
  })

  it('an unreadable time bound is a filter error and sends nothing', async () => {
    const list = vi.fn().mockResolvedValue(page([], null))
    const m = new RecordingListModel({ list })
    await m.load(CID, { ...EMPTY_RECORDING_FILTER, until: 'not a time' })
    expect(list).not.toHaveBeenCalled()
    expect(m.filterError).toMatch(/until/)
  })

  it('a stale answer of the previous filter never lands', async () => {
    let releaseFirst: (v: RecordingListResponse) => void = () => {}
    const list = vi
      .fn()
      .mockImplementationOnce(() => new Promise<RecordingListResponse>((r) => (releaseFirst = r)))
      .mockResolvedValueOnce(page(['new'], null))
    const m = new RecordingListModel({ list })
    const first = m.load(CID, EMPTY_RECORDING_FILTER)
    await m.setFilter({ ...EMPTY_RECORDING_FILTER, state: 'done' })
    releaseFirst(page(['old'], 'x'))
    await first
    expect(m.recordings.map((r) => r.id)).toEqual(['new'])
    expect(m.nextCursor).toBeNull()
  })

  it('a failing Load more keeps the rows and names the error', async () => {
    const list = vi.fn().mockResolvedValueOnce(page(['r3'], 'c1')).mockRejectedValueOnce(new ApiError(500, 'server', 'down'))
    const m = new RecordingListModel({ list })
    await m.load(CID, EMPTY_RECORDING_FILTER)
    await m.loadMore()
    expect(m.recordings.map((r) => r.id)).toEqual(['r3'])
    expect(m.moreError?.message).toBe('down')
    expect(m.nextCursor).toBe('c1')
  })
})

// ---------------------------------------------------------------------------

describe('RecordingDetailModel', () => {
  const GET: RecordingGetResponse = {
    success: true,
    recording: meta(RID),
    request: { address: 'request', state: 'present', format: 'json', bytes: 120 },
    response: { address: 'response', state: 'present', format: 'not_utf8', bytes: 4 },
    blocks: [
      block('request/messages/0'),
      block('request/params', { kind: 'json', role: undefined, translatable: false }),
      block('response', { side: 'response', kind: 'raw', role: undefined, utf8: false, translatable: false, preview: '', preview_omitted: true, bytes: 4 }),
    ],
    block_offset: 0,
    blocks_total: 3,
  }

  /** A page of a recording with `total` blocks: request messages, the last `responses` of them answer blocks. */
  function pageOf(offset: number, size: number, total: number, responses: number): RecordingGetResponse {
    const blocks = Array.from({ length: Math.max(0, Math.min(size, total - offset)) }, (_, k) => {
      const i = offset + k
      return i < total - responses
        ? block(`request/messages/${i}`)
        : block(`response/choices/${i - (total - responses)}/content`, { side: 'response', role: 'assistant' })
    })
    return { ...GET, blocks, block_offset: offset, blocks_total: total }
  }

  it('loads the recording, its sides and one view per block; only translatable blocks translate', async () => {
    const get = vi.fn().mockResolvedValue(GET)
    const m = new RecordingDetailModel({ get })
    await m.load(RID)
    expect(get.mock.calls[0]?.[0]).toBe(RID)
    expect(get.mock.calls[0]?.[1]?.blockOffset, 'the first page names no offset').toBeUndefined()
    expect(m.blocksTotal).toBe(3)
    expect(m.hasMoreBlocks).toBe(false)
    expect(m.status).toBe('ready')
    expect(m.recording?.id).toBe(RID)
    expect(m.request?.format).toBe('json')
    expect(m.views.map((v) => v.block.address)).toEqual(['request/messages/0', 'request/params', 'response'])
    expect(m.views.map((v) => v.translation !== null)).toEqual([true, false, false])
    expect(m.requestBlocks.map((v) => v.block.address)).toEqual(['request/messages/0', 'request/params'])
    expect(m.responseBlocks.map((v) => v.block.address)).toEqual(['response'])
    expect(m.sideReader('response').hexView, 'a body that is not UTF-8 reads as hex').toBe(true)
    expect(m.sideReader('request').hexView).toBe(false)
  })

  it('the translation language defaults to the browser language when the server has it, else English', async () => {
    const de = new RecordingDetailModel({ get: vi.fn().mockResolvedValue(GET) }, 'de-AT')
    expect(de.language).toBe('de')
    await de.load(RID)
    expect(de.views[0]?.translation?.language, 'the blocks start in the page’s language').toBe('de')
    expect(new RecordingDetailModel({}, 'tlh').language).toBe('en')
    expect(new RecordingDetailModel({}, undefined).language).toBe('en')
  })

  it('more blocks than one page: Load more blocks appends the next page by block_offset, grouped by side, until the end', async () => {
    const get = vi.fn().mockImplementation(async (_id: string, opts?: { blockOffset?: number }) => pageOf(opts?.blockOffset ?? 0, 500, 1200, 50))
    const m = new RecordingDetailModel({ get })
    await m.load(RID)
    expect(m.views).toHaveLength(500)
    expect(m.blocksTotal).toBe(1200)
    expect(m.hasMoreBlocks).toBe(true)
    expect(m.responseBlocks, 'the answer’s blocks lie past the first page').toHaveLength(0)
    await m.loadMoreBlocks()
    expect(get.mock.calls[1]?.[1]?.blockOffset).toBe(500)
    expect(m.views).toHaveLength(1000)
    expect(m.hasMoreBlocks).toBe(true)
    await m.loadMoreBlocks()
    expect(get.mock.calls[2]?.[1]?.blockOffset).toBe(1000)
    expect(m.views).toHaveLength(1200)
    expect(m.hasMoreBlocks, 'at the end the button goes').toBe(false)
    expect(m.requestBlocks).toHaveLength(1150)
    expect(m.responseBlocks.map((v) => v.block.address).slice(0, 2)).toEqual(['response/choices/0/content', 'response/choices/1/content'])
    expect(new Set(m.views.map((v) => v.block.address)).size, 'no block twice across pages').toBe(1200)
    await m.loadMoreBlocks()
    expect(get, 'nothing is read past the last block').toHaveBeenCalledTimes(3)
  })

  it('a later page refreshes the recording: one that finished in between reads done, and the raw reader of a side that changed follows it', async () => {
    const first: RecordingGetResponse = {
      ...pageOf(0, 500, 600, 0),
      recording: meta(RID, { state: 'open', status: null, finished_at: null, response_bytes: 0, response_null: true }),
      response: { address: 'response', state: 'pending', bytes: 0 },
    }
    const second: RecordingGetResponse = { ...pageOf(500, 500, 600, 0), recording: meta(RID) }
    const get = vi.fn().mockResolvedValueOnce(first).mockResolvedValueOnce(second)
    const m = new RecordingDetailModel({ get })
    await m.load(RID)
    expect(m.recording?.state).toBe('open')
    expect(m.response?.state).toBe('pending')
    const pending = m.sideReader('response')
    const request = m.sideReader('request')
    await m.loadMoreBlocks()
    expect(m.views).toHaveLength(600)
    expect(m.recording?.state, 'the page’s metadata, not the first one’s').toBe('done')
    expect(m.recording?.status).toBe(200)
    expect(m.response?.state).toBe('present')
    expect(m.sideReader('response'), 'a side whose state changed gets a fresh reader').not.toBe(pending)
    expect(m.sideReader('response').hexView, 'the fresh reader reads the answer as it now is (not UTF-8)').toBe(true)
    expect(m.sideReader('request'), 'an unchanged side keeps its reader and what it loaded').toBe(request)
  })

  it('a failing next page keeps the loaded blocks and names the error', async () => {
    const get = vi.fn().mockResolvedValueOnce(pageOf(0, 500, 600, 0)).mockRejectedValueOnce(envelope(503, 'audit_unavailable'))
    const m = new RecordingDetailModel({ get })
    await m.load(RID)
    await m.loadMoreBlocks()
    expect(m.views).toHaveLength(500)
    expect(m.moreBlocksError?.code).toBe('audit_unavailable')
    expect(m.hasMoreBlocks).toBe(true)
  })

  it('leaving the page (dispose) aborts a running detail read; its late answer never lands', async () => {
    let signal: AbortSignal | undefined
    let release: (v: RecordingGetResponse) => void = () => {}
    const get = vi.fn().mockImplementation((_id: string, opts?: { signal?: AbortSignal }) => {
      signal = opts?.signal
      return new Promise<RecordingGetResponse>((r) => (release = r))
    })
    const m = new RecordingDetailModel({ get })
    const run = m.load(RID)
    expect(signal, 'the read carries a signal').toBeDefined()
    m.dispose()
    expect(signal?.aborted).toBe(true)
    release(GET)
    await run
    expect(m.views).toEqual([])
    expect(m.recording).toBeNull()
  })

  it('leaving the page aborts a running next-page read as well', async () => {
    const signals: (AbortSignal | undefined)[] = []
    const get = vi
      .fn()
      .mockImplementationOnce(async () => pageOf(0, 500, 900, 0))
      .mockImplementationOnce((_id: string, opts?: { signal?: AbortSignal }) => {
        signals.push(opts?.signal)
        return new Promise(() => {})
      })
    const m = new RecordingDetailModel({ get })
    await m.load(RID)
    void m.loadMoreBlocks()
    await vi.waitFor(() => expect(get).toHaveBeenCalledTimes(2))
    m.dispose()
    expect(signals[0]?.aborted).toBe(true)
  })

  it('an unknown id is notFound', async () => {
    const m = new RecordingDetailModel({ get: vi.fn().mockRejectedValue(envelope(404, 'not_found')) })
    await m.load('nope')
    expect(m.status).toBe('error')
    expect(m.notFound).toBe(true)
    expect(m.views).toEqual([])
  })

  it('an audit that cannot be written is its named error, nothing shown', async () => {
    const m = new RecordingDetailModel({ get: vi.fn().mockRejectedValue(envelope(503, 'audit_unavailable')) })
    await m.load(RID)
    expect(m.status).toBe('error')
    expect(m.error?.code).toBe('audit_unavailable')
    expect(m.error?.text).toMatch(/could not be audited/)
    expect(m.recording).toBeNull()
  })
})

// ---------------------------------------------------------------------------

describe('WindowReader — 64 KiB windows, one streaming decoder', () => {
  it('reads from 0 in 64 KiB windows until eof and joins a character split across two windows', async () => {
    // "a€b": € is E2 82 AC — the first window ends after E2 82
    const all = new TextEncoder().encode('a€b')
    const read = vi
      .fn()
      .mockImplementationOnce(async (p: RecordingReadParams) => window(all.slice(0, 3), p.offset, all.length))
      .mockImplementationOnce(async (p: RecordingReadParams) => window(all.slice(3), p.offset, all.length))
    const r = new WindowReader({ read }, RID, 'request/messages/0', { utf8: true })
    await r.more()
    expect(read.mock.calls[0]?.[0]).toEqual({ id: RID, address: 'request/messages/0', offset: 0, limit: 65536 })
    expect(r.text, 'the half character waits for its rest').toBe('a')
    expect(r.loaded).toBe(3)
    expect(r.total).toBe(5)
    expect(r.eof).toBe(false)
    await r.more()
    expect(read.mock.calls[1]?.[0]).toEqual({ id: RID, address: 'request/messages/0', offset: 3, limit: 65536 })
    expect(r.text).toBe('a€b')
    expect(r.text).not.toContain('�')
    expect(r.eof).toBe(true)
    await r.more()
    expect(read, 'nothing is read past eof').toHaveBeenCalledTimes(2)
  })

  it('a block that is not UTF-8 is shown as a lossless hex dump', async () => {
    const bytes = Uint8Array.from([0xff, 0xfe, 0x00, 0x41])
    const read = vi.fn().mockResolvedValue(window(bytes, 0, 4, false))
    const r = new WindowReader({ read }, RID, 'response', { utf8: false })
    await r.more()
    expect(r.hexView).toBe(true)
    expect(r.hexText()).toBe('00000000  ff fe 00 41')
  })

  it('a window the server refuses keeps what was loaded and names the error', async () => {
    const read = vi.fn().mockRejectedValue(envelope(409, 'body_cleared'))
    const r = new WindowReader({ read }, RID, 'request', { utf8: null })
    await r.more()
    expect(r.status).toBe('error')
    expect(r.error?.code).toBe('body_cleared')
    expect(r.loaded).toBe(0)
  })

  it('dispose aborts a running read', async () => {
    let signal: AbortSignal | undefined
    const read = vi.fn().mockImplementation((_p: RecordingReadParams, s?: AbortSignal) => {
      signal = s
      return new Promise(() => {})
    })
    const r = new WindowReader({ read }, RID, 'request', { utf8: null })
    void r.more()
    await Promise.resolve()
    r.dispose()
    expect(signal?.aborted).toBe(true)
  })
})

// ---------------------------------------------------------------------------

describe('TranslationModel', () => {
  it('asks chunk 0, then 1..chunks-1 in turn, appending — never two at once', async () => {
    let inFlight = 0
    let maxInFlight = 0
    const translate = vi.fn().mockImplementation(async (p: RecordingTranslateParams) => {
      inFlight++
      maxInFlight = Math.max(maxInFlight, inFlight)
      await Promise.resolve()
      inFlight--
      return tr(p.chunk, 3, `part ${p.chunk}`)
    })
    const t = new TranslationModel({ translate }, RID, 'request/messages/0', 'de')
    await t.start()
    expect(translate.mock.calls.map((c) => (c[0] as RecordingTranslateParams).chunk)).toEqual([0, 1, 2])
    expect(translate.mock.calls[0]?.[0]).toEqual({ id: RID, address: 'request/messages/0', language: 'de', chunk: 0 })
    expect(maxInFlight).toBe(1)
    expect(t.parts).toEqual(['part 0', 'part 1', 'part 2'])
    expect(t.chunks).toBe(3)
    expect(t.done).toBe(3)
    expect(t.status).toBe('done')
    expect(t.model).toBe('qwen38-27b')
  })

  it('the language select decides the language of the next translation', async () => {
    const translate = vi.fn().mockResolvedValue(tr(0, 1, 'x'))
    const t = new TranslationModel({ translate }, RID, 'a', 'en')
    t.language = 'ja'
    await t.start()
    expect((translate.mock.calls[0]?.[0] as RecordingTranslateParams).language).toBe('ja')
  })

  it('Stop aborts the running chunk and asks for no further one', async () => {
    const signals: AbortSignal[] = []
    let release: (v: RecordingTranslateResponse) => void = () => {}
    const translate = vi
      .fn()
      .mockImplementationOnce(async (_p: RecordingTranslateParams, s?: AbortSignal) => {
        if (s) signals.push(s)
        return tr(0, 3, 'part 0')
      })
      .mockImplementationOnce((_p: RecordingTranslateParams, s?: AbortSignal) => {
        if (s) signals.push(s)
        return new Promise<RecordingTranslateResponse>((r) => (release = r))
      })
    const t = new TranslationModel({ translate }, RID, 'a', 'de')
    const run = t.start()
    await vi.waitFor(() => expect(translate).toHaveBeenCalledTimes(2))
    t.stop()
    expect(signals[1]?.aborted).toBe(true)
    release(tr(1, 3, 'late part'))
    await run
    expect(translate).toHaveBeenCalledTimes(2)
    expect(t.status).toBe('stopped')
    expect(t.parts, 'what came before Stop stays, nothing after').toEqual(['part 0'])
  })

  it('leaving the page (dispose) aborts the running chunk', async () => {
    let signal: AbortSignal | undefined
    const translate = vi.fn().mockImplementation((_p: RecordingTranslateParams, s?: AbortSignal) => {
      signal = s
      return new Promise(() => {})
    })
    const t = new TranslationModel({ translate }, RID, 'a', 'de')
    void t.start()
    await vi.waitFor(() => expect(translate).toHaveBeenCalledTimes(1))
    t.dispose()
    expect(signal?.aborted).toBe(true)
  })

  it('a new translation of the same block aborts the running one; its parts never mix in', async () => {
    const signals: AbortSignal[] = []
    let releaseOld: (v: RecordingTranslateResponse) => void = () => {}
    const translate = vi
      .fn()
      .mockImplementationOnce((_p: RecordingTranslateParams, s?: AbortSignal) => {
        if (s) signals.push(s)
        return new Promise<RecordingTranslateResponse>((r) => (releaseOld = r))
      })
      .mockImplementation(async (p: RecordingTranslateParams, s?: AbortSignal) => {
        if (s) signals.push(s)
        return tr(p.chunk, 1, `new ${p.language}`)
      })
    const t = new TranslationModel({ translate }, RID, 'a', 'de')
    const old = t.start()
    await vi.waitFor(() => expect(translate).toHaveBeenCalledTimes(1))
    t.language = 'fr'
    await t.start()
    expect(signals[0]?.aborted, 'the first run is aborted').toBe(true)
    releaseOld(tr(0, 1, 'old de'))
    await old
    expect(t.parts).toEqual(['new fr'])
    expect(t.status).toBe('done')
  })

  it.each([
    [503, 'no_eligible_backend', /No local backend may receive recorded content/],
    [503, 'window_too_small', /context window/i],
    [413, 'block_too_large', /256000/],
    [422, 'not_translatable', /no text to translate/i],
    [429, 'translate_busy', /a translation of yours is running/i],
    [502, 'translate_failed', /model did not answer/i],
    [503, 'canceled', /canceled/i],
    [503, 'audit_unavailable', /could not be audited/i],
  ])('%i %s ⇒ its named error state', async (status, code, text) => {
    const translate = vi.fn().mockRejectedValue(envelope(status, code, { limit_runes: 256000 }))
    const t = new TranslationModel({ translate }, RID, 'a', 'de')
    await t.start()
    expect(t.status).toBe('error')
    expect(t.error?.code).toBe(code)
    expect(t.error?.text).toMatch(text)
  })

  it('an error in a later chunk keeps the parts before it', async () => {
    const translate = vi.fn().mockResolvedValueOnce(tr(0, 2, 'part 0')).mockRejectedValueOnce(envelope(502, 'translate_failed'))
    const t = new TranslationModel({ translate }, RID, 'a', 'de')
    await t.start()
    expect(t.parts).toEqual(['part 0'])
    expect(t.status).toBe('error')
    expect(t.error?.code).toBe('translate_failed')
  })
})

describe('BlockModel', () => {
  it('a translatable block carries a translation in the default language, others none', () => {
    const api = { translate: vi.fn(), read: vi.fn() }
    expect(new BlockModel(api, RID, block('request/messages/0'), 'de').translation?.language).toBe('de')
    expect(new BlockModel(api, RID, block('request/params', { translatable: false }), 'de').translation).toBeNull()
  })

  it('the original is the preview until windows are loaded, then the decoded bytes', async () => {
    const all = new TextEncoder().encode('hello world')
    const read = vi.fn().mockResolvedValue(window(all, 0, all.length))
    const m = new BlockModel({ read, translate: vi.fn() }, RID, block('request/messages/0', { preview: 'hello', preview_truncated: true, bytes: all.length }), 'en')
    expect(m.originalText).toBe('hello')
    await m.reader.more()
    expect(m.originalText).toBe('hello world')
  })
})

// ---------------------------------------------------------------------------

describe('nothing of a recording or a translation is stored in the browser', () => {
  const FILES = [
    'recordings.svelte.ts',
    'recording-data.ts',
    'RecordingsPage.svelte',
    'RecordingListPage.svelte',
    'RecordingDetailPage.svelte',
    'RecordingBlock.svelte',
    '../../lib/api/recordings.ts',
    // the render path of a translation
    '../../lib/markdown/Markdown.svelte',
    '../../lib/markdown/markdown.ts',
    '../../lib/markdown/camo.ts',
  ]
  const STORAGE = /\b(localStorage|sessionStorage|indexedDB|IDBFactory|caches|CacheStorage|document\.cookie|cookieStore)\b/

  it('the modules name no storage API (static scan)', () => {
    expect('sessionStorage.setItem').toMatch(STORAGE)
    expect('window.localStorage').toMatch(STORAGE)
    for (const f of FILES) {
      const src = readFileSync(new URL(f, import.meta.url), 'utf8')
      expect(src, f).not.toMatch(STORAGE)
    }
  })

  it('a full read and translation touch no storage (runtime trap)', async () => {
    const touched: string[] = []
    const trap = (name: string) =>
      new Proxy(
        {},
        {
          get(_t, p) {
            touched.push(`${name}.${String(p)}`)
            return () => null
          },
        },
      )
    vi.stubGlobal('localStorage', trap('localStorage'))
    vi.stubGlobal('sessionStorage', trap('sessionStorage'))
    vi.stubGlobal('indexedDB', trap('indexedDB'))
    const all = new TextEncoder().encode('secret')
    const api = {
      get: vi.fn().mockResolvedValue({
        success: true,
        recording: meta(RID),
        request: { address: 'request', state: 'present', format: 'json', bytes: 6 },
        response: { address: 'response', state: 'none', bytes: 0 },
        blocks: [block('request/messages/0', { preview: 'sec', preview_truncated: true, bytes: 6 })],
        block_offset: 0,
        blocks_total: 1,
      }),
      read: vi.fn().mockResolvedValue(window(all, 0, all.length)),
      translate: vi.fn().mockResolvedValue(tr(0, 1, 'geheim')),
    }
    const d = new RecordingDetailModel(api)
    await d.load(RID)
    const v = d.views[0]
    if (v === undefined) throw new Error('no view')
    await v.reader.more()
    await v.translation?.start()
    await d.sideReader('request').more()
    d.dispose()
    expect(v.translation?.parts).toEqual(['geheim'])
    expect(touched).toEqual([])
  })
})
