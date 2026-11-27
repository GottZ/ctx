// Recordings view state (issue #575): the server-admin's reading of the
// requests a recording consumer sent. Plain $state classes with an injectable
// api (consumer.svelte.ts pattern) so vitest covers them in node.
//
//   RecordingConsumersModel  model-recording-consumers (overview)
//   RecordingListModel       model-recording-list — keyset pages newest first,
//                            the applied filter, Load more / Continue after a
//                            walk that stopped at the server's scan bound
//   RecordingDetailModel     model-recording-get — the blocks (BlockModel each)
//                            and one raw WindowReader per side
//   WindowReader             model-recording-read in 64 KiB windows; ONE
//                            streaming TextDecoder over the windows so a
//                            character split across two stays whole; bytes
//                            that are not UTF-8 are shown as a hex dump
//   TranslationModel         model-recording-translate — chunk 0, then 1..n-1
//                            in turn; Stop, leaving the page and a new
//                            translation of the block abort the running one
//
// Nothing here is stored: no browser storage of any kind, no cache beyond
// the instance — content and translations live as long as the page's
// component state (pinned by the storage scan + runtime trap in the test).
// Stale answers are dropped (generation counters / the AbortController).

import { toApiError, type ApiError } from '../../lib/api'
import {
  getRecording,
  listRecordingConsumers,
  listRecordings,
  readRecording,
  RECORDING_WINDOW_BYTES,
  translateRecording,
  type RecordingListParams,
} from '../../lib/api/recordings'
import type {
  RecordingBlock,
  RecordingConsumerView,
  RecordingGetResponse,
  RecordingListResponse,
  RecordingMeta,
  RecordingOpFilter,
  RecordingSide,
  RecordingStateFilter,
  RecordingStatusFilter,
} from '../../lib/api/types'
import type { ResourceStatus } from '../../lib/resource.svelte'
import { decodeBase64, hexDump, localInputToRfc3339, pickDefaultLanguage, recordingError, type RecordingErrorState } from './recording-data'

export interface RecordingsApi {
  consumers: typeof listRecordingConsumers
  list: typeof listRecordings
  get: typeof getRecording
  read: typeof readRecording
  translate: typeof translateRecording
}

const DEFAULT_API: RecordingsApi = {
  consumers: listRecordingConsumers,
  list: listRecordings,
  get: getRecording,
  read: readRecording,
  translate: translateRecording,
}

function apiOf(api: Partial<RecordingsApi>): RecordingsApi {
  return { ...DEFAULT_API, ...api }
}

// ---------------------------------------------------------------------------

/** The consumers that record or have recordings, with this process's counts. */
export class RecordingConsumersModel {
  consumers = $state<RecordingConsumerView[]>([])
  processId = $state('')
  since = $state('')
  status = $state<ResourceStatus>('idle')
  loadError = $state<ApiError | null>(null)

  readonly #api: RecordingsApi
  #gen = 0

  constructor(api: Partial<RecordingsApi> = {}) {
    this.#api = apiOf(api)
  }

  async load(): Promise<void> {
    const gen = ++this.#gen
    this.status = 'loading'
    this.loadError = null
    try {
      const res = await this.#api.consumers()
      if (gen !== this.#gen) return
      this.consumers = res.consumers
      this.processId = res.process_id
      this.since = res.since
      this.status = 'ready'
    } catch (e) {
      if (gen !== this.#gen) return
      this.consumers = []
      this.loadError = toApiError(e)
      this.status = 'error'
    }
  }
}

// ---------------------------------------------------------------------------

/**
 * The list filter as the form holds it: since/until are typed wall-clock values
 * (the viewer's wall clock, '' = no bound), '' = no state / status filter.
 */
export interface RecordingFilter {
  since: string
  until: string
  ops: RecordingOpFilter[]
  state: '' | RecordingStateFilter
  status: '' | RecordingStatusFilter
}

export const EMPTY_RECORDING_FILTER: RecordingFilter = Object.freeze({ since: '', until: '', ops: [], state: '', status: '' }) as RecordingFilter

/** The list call of a filter (+ cursor), or the name of the time field that does not parse. */
function listParams(
  consumerId: string,
  f: RecordingFilter,
  cursor: string | undefined,
): { ok: true; params: RecordingListParams } | { ok: false; field: 'since' | 'until' } {
  const since = localInputToRfc3339(f.since)
  if (!since.ok) return { ok: false, field: 'since' }
  const until = localInputToRfc3339(f.until)
  if (!until.ok) return { ok: false, field: 'until' }
  const params: RecordingListParams = { consumerId }
  if (cursor !== undefined) params.cursor = cursor
  if (since.value !== undefined) params.since = since.value
  if (until.value !== undefined) params.until = until.value
  if (f.ops.length > 0) params.ops = [...f.ops]
  if (f.state !== '') params.state = f.state
  if (f.status !== '') params.status = f.status
  return { ok: true, params }
}

/** One consumer's recordings, newest first, keyset paged, filtered. */
export class RecordingListModel {
  recordings = $state.raw<RecordingMeta[]>([])
  nextCursor = $state<string | null>(null)
  /** Rows the server walked since the filter was applied (summed over Load more / Continue). */
  scanned = $state(0)
  /** The last call stopped at the server's scan bound (a rare filter) — Continue goes on. */
  scanLimitReached = $state(false)
  status = $state<ResourceStatus>('idle')
  loadError = $state<ApiError | null>(null)
  moreError = $state<ApiError | null>(null)
  loadingMore = $state(false)
  /** The APPLIED filter (the form keeps its own draft). */
  filter = $state<RecordingFilter>({ ...EMPTY_RECORDING_FILTER, ops: [] })
  /** A time bound that does not parse — nothing was sent. */
  filterError = $state<string | null>(null)

  readonly #api: RecordingsApi
  #gen = 0
  #consumerId = ''

  constructor(api: Partial<RecordingsApi> = {}) {
    this.#api = apiOf(api)
  }

  /** Load the first page of a consumer's recordings under `filter`. */
  async load(consumerId: string, filter: RecordingFilter = this.filter): Promise<void> {
    const built = listParams(consumerId, filter, undefined)
    if (!built.ok) {
      this.filterError = `${built.field} is not a valid date and time`
      return
    }
    const gen = ++this.#gen
    this.#consumerId = consumerId
    this.filter = { ...filter, ops: [...filter.ops] }
    this.filterError = null
    this.status = 'loading'
    this.loadError = null
    this.moreError = null
    this.loadingMore = false
    this.recordings = []
    this.nextCursor = null
    this.scanned = 0
    this.scanLimitReached = false
    let res: RecordingListResponse
    try {
      res = await this.#api.list(built.params)
    } catch (e) {
      if (gen !== this.#gen) return
      this.loadError = toApiError(e)
      this.status = 'error'
      return
    }
    if (gen !== this.#gen) return
    this.recordings = res.recordings
    this.nextCursor = res.next_cursor
    this.scanned = res.scanned
    this.scanLimitReached = res.scan_limit_reached
    this.status = 'ready'
  }

  /** Apply a new filter: start over without a cursor. */
  setFilter(filter: RecordingFilter): Promise<void> {
    return this.load(this.#consumerId, filter)
  }

  /** Append the next page (Load more), or go on after the scan bound (Continue). */
  async loadMore(): Promise<void> {
    const cursor = this.nextCursor
    if (cursor === null || this.loadingMore || this.status !== 'ready') return
    const built = listParams(this.#consumerId, this.filter, cursor)
    if (!built.ok) return
    const gen = this.#gen
    this.loadingMore = true
    this.moreError = null
    try {
      const res = await this.#api.list(built.params)
      if (gen !== this.#gen) return
      this.recordings = [...this.recordings, ...res.recordings]
      this.nextCursor = res.next_cursor
      this.scanned += res.scanned
      this.scanLimitReached = res.scan_limit_reached
    } catch (e) {
      if (gen !== this.#gen) return
      this.moreError = toApiError(e)
    } finally {
      if (gen === this.#gen) this.loadingMore = false
    }
  }
}

// ---------------------------------------------------------------------------

/**
 * The bytes of one address — a block, or a whole body ('request' /
 * 'response') — read on demand in windows of RECORDING_WINDOW_BYTES from
 * offset 0. Text is decoded by ONE TextDecoder in streaming mode, so a
 * multi-byte character cut by a window boundary joins its rest in the next
 * window; nothing is repaired (fatal:false only marks invalid bytes, and an
 * address known not to be UTF-8 is shown as hex instead, losslessly).
 */
export class WindowReader {
  /** The loaded bytes decoded (not used for a hex view). */
  text = $state('')
  /** Bytes loaded from offset 0. */
  loaded = $state(0)
  /** The address's whole size, once a window told it. */
  total = $state<number | null>(null)
  eof = $state(false)
  status = $state<ResourceStatus>('idle')
  error = $state<RecordingErrorState | null>(null)
  /** true: the bytes are not valid UTF-8 — shown as a hex dump, never decoded. */
  readonly hexView: boolean

  readonly #api: RecordingsApi
  readonly #id: string
  readonly #address: string
  readonly #decoder = new TextDecoder('utf-8', { fatal: false })
  #windows: { start: number; bytes: Uint8Array }[] = []
  #ctrl: AbortController | null = null

  constructor(api: Partial<RecordingsApi>, id: string, address: string, opts: { utf8: boolean | null }) {
    this.#api = apiOf(api)
    this.#id = id
    this.#address = address
    this.hexView = opts.utf8 === false
  }

  /** Read the next window (the first from offset 0); nothing past eof. */
  async more(): Promise<void> {
    if (this.eof || this.status === 'loading') return
    const ctrl = new AbortController()
    this.#ctrl = ctrl
    this.status = 'loading'
    this.error = null
    try {
      const res = await this.#api.read(
        { id: this.#id, address: this.#address, offset: this.loaded, limit: RECORDING_WINDOW_BYTES },
        ctrl.signal,
      )
      if (ctrl.signal.aborted) return
      const bytes = decodeBase64(res.data)
      this.#windows.push({ start: this.loaded, bytes })
      if (!this.hexView) this.text += this.#decoder.decode(bytes, { stream: !res.eof })
      this.loaded += bytes.length
      this.total = res.total
      this.eof = res.eof
      this.status = 'ready'
    } catch (e) {
      if (ctrl.signal.aborted) return
      this.error = recordingError(toApiError(e))
      this.status = 'error'
    } finally {
      if (this.#ctrl === ctrl) this.#ctrl = null
    }
  }

  /** The loaded bytes as a hex dump with their offsets (reactive on `loaded`). */
  hexText(): string {
    if (this.loaded === 0) return ''
    return this.#windows.map((w) => hexDump(w.bytes, w.start)).join('\n')
  }

  /** Abort a running read (leaving the page). */
  dispose(): void {
    this.#ctrl?.abort()
    this.#ctrl = null
  }
}

// ---------------------------------------------------------------------------

export type TranslationStatus = 'idle' | 'running' | 'done' | 'stopped' | 'error'

/**
 * The translation of one block: chunk 0 tells how many chunks there are, the
 * rest follow one at a time (one translation per person runs on the server —
 * a parallel call would be 429 translate_busy). The parts are Markdown from a
 * model, kept here only — rendered by the page through lib/markdown.
 */
export class TranslationModel {
  /** The target language of the next start(). */
  language = $state('en')
  status = $state<TranslationStatus>('idle')
  /** One Markdown text per translated chunk, in order. */
  parts = $state<string[]>([])
  /** Chunks translated so far. */
  done = $state(0)
  /** The block's chunk count, once chunk 0 answered. */
  chunks = $state<number | null>(null)
  /** The model the server names; null until it answered. */
  model = $state<string | null>(null)
  error = $state<RecordingErrorState | null>(null)

  readonly #api: RecordingsApi
  readonly #id: string
  readonly #address: string
  #ctrl: AbortController | null = null

  constructor(api: Partial<RecordingsApi>, id: string, address: string, language: string) {
    this.#api = apiOf(api)
    this.#id = id
    this.#address = address
    this.language = language
  }

  /** (Re)start the translation in `language`; a running one of this block is aborted first. */
  async start(): Promise<void> {
    this.#ctrl?.abort()
    const ctrl = new AbortController()
    this.#ctrl = ctrl
    const language = this.language
    this.status = 'running'
    this.parts = []
    this.done = 0
    this.chunks = null
    this.model = null
    this.error = null
    let chunk = 0
    try {
      for (;;) {
        const res = await this.#api.translate({ id: this.#id, address: this.#address, language, chunk }, ctrl.signal)
        // aborted: Stop / dispose / a newer start owns the state now
        if (ctrl.signal.aborted) return
        this.parts = [...this.parts, res.text]
        this.chunks = res.chunks
        this.done = chunk + 1
        if (res.model !== '') this.model = res.model
        chunk++
        if (chunk >= res.chunks) break
      }
      this.status = 'done'
    } catch (e) {
      if (ctrl.signal.aborted) return
      this.error = recordingError(toApiError(e))
      this.status = 'error'
    } finally {
      if (this.#ctrl === ctrl) this.#ctrl = null
    }
  }

  /** Stop: abort the running chunk, ask for no further one; the parts so far stay. */
  stop(): void {
    if (this.#ctrl === null) return
    this.#ctrl.abort()
    this.#ctrl = null
    this.status = 'stopped'
  }

  /** Leaving the page: abort without a state change. */
  dispose(): void {
    this.#ctrl?.abort()
    this.#ctrl = null
  }
}

// ---------------------------------------------------------------------------

/** One block of a recording: its bytes (preview, windows, raw) and, when translatable, its translation. */
export class BlockModel {
  readonly block: RecordingBlock
  readonly reader: WindowReader
  readonly translation: TranslationModel | null
  /** The raw view (the block's bytes as read: UTF-8 text, or hex). */
  raw = $state(false)

  constructor(api: Partial<RecordingsApi>, id: string, block: RecordingBlock, language: string) {
    this.block = block
    this.reader = new WindowReader(api, id, block.address, { utf8: block.utf8 })
    this.translation = block.translatable ? new TranslationModel(api, id, block.address, language) : null
  }

  /** The original as text: the preview until windows were read, then the decoded bytes (from offset 0). */
  get originalText(): string {
    return this.reader.loaded > 0 ? this.reader.text : this.block.preview
  }

  dispose(): void {
    this.reader.dispose()
    this.translation?.dispose()
  }
}

/** A side's raw reader: hex when the body is not UTF-8. */
function sideReader(api: RecordingsApi, id: string, side: RecordingSide | null, address: 'request' | 'response'): WindowReader {
  return new WindowReader(api, id, address, { utf8: side?.format === 'not_utf8' ? false : null })
}

/**
 * One recording as blocks, read a page at a time: the server answers at most
 * 500 blocks per call (block_offset / blocks_total), so a request of 100 000
 * messages never arrives — nor renders — whole; Load more blocks appends the
 * next page. Every read carries an AbortSignal: leaving the page cancels it.
 */
export class RecordingDetailModel {
  recording = $state<RecordingMeta | null>(null)
  request = $state<RecordingSide | null>(null)
  response = $state<RecordingSide | null>(null)
  /** The loaded blocks, in the server's order (request first, then the answer's) — pages appended. */
  views = $state.raw<BlockModel[]>([])
  /** All blocks of the recording (the server's blocks_total). */
  blocksTotal = $state(0)
  loadingMoreBlocks = $state(false)
  moreBlocksError = $state<RecordingErrorState | null>(null)
  status = $state<ResourceStatus>('idle')
  /** The server answered 404 not_found. */
  notFound = $state(false)
  error = $state<RecordingErrorState | null>(null)

  /**
   * The target language of the page's next translation (one select for all
   * blocks): the admin's browser language when the server's set has it, else
   * English (pickDefaultLanguage — the SPA's own UI is English, no i18n).
   */
  language = $state('en')

  readonly #api: RecordingsApi
  #gen = 0
  #id = ''
  /** The running detail read (first page or a next one). */
  #ctrl: AbortController | null = null
  #sides = $state.raw<Record<'request' | 'response', WindowReader>>({
    request: new WindowReader({}, '', 'request', { utf8: null }),
    response: new WindowReader({}, '', 'response', { utf8: null }),
  })

  /** `navLanguage`: the browser language the translation default derives from (pickDefaultLanguage). */
  constructor(api: Partial<RecordingsApi> = {}, navLanguage: string | undefined = typeof navigator === 'undefined' ? undefined : navigator.language) {
    this.#api = apiOf(api)
    this.language = pickDefaultLanguage(navLanguage)
  }

  get requestBlocks(): BlockModel[] {
    return this.views.filter((v) => v.block.side === 'request')
  }

  get responseBlocks(): BlockModel[] {
    return this.views.filter((v) => v.block.side === 'response')
  }

  /** Blocks of the recording that are not loaded yet. */
  get hasMoreBlocks(): boolean {
    return this.status === 'ready' && this.views.length < this.blocksTotal
  }

  #read(offset: number | undefined): { ctrl: AbortController; run: Promise<RecordingGetResponse> } {
    this.#ctrl?.abort()
    const ctrl = new AbortController()
    this.#ctrl = ctrl
    const opts = offset === undefined ? { signal: ctrl.signal } : { blockOffset: offset, signal: ctrl.signal }
    return { ctrl, run: this.#api.get(this.#id, opts) }
  }

  /** The raw reader of a whole body. */
  sideReader(address: 'request' | 'response'): WindowReader {
    return this.#sides[address]
  }

  async load(id: string): Promise<void> {
    const gen = ++this.#gen
    this.#disposeParts()
    this.#id = id
    this.status = 'loading'
    this.error = null
    this.notFound = false
    this.moreBlocksError = null
    this.loadingMoreBlocks = false
    const { ctrl, run } = this.#read(undefined)
    try {
      const res = await run
      if (gen !== this.#gen || ctrl.signal.aborted) return
      this.recording = res.recording
      this.request = res.request
      this.response = res.response
      this.views = res.blocks.map((b) => new BlockModel(this.#api, res.recording.id, b, this.language))
      this.blocksTotal = res.blocks_total
      this.#sides = {
        request: sideReader(this.#api, res.recording.id, res.request, 'request'),
        response: sideReader(this.#api, res.recording.id, res.response, 'response'),
      }
      this.status = 'ready'
    } catch (e) {
      if (gen !== this.#gen || ctrl.signal.aborted) return
      const err = recordingError(toApiError(e))
      this.recording = null
      this.request = null
      this.response = null
      this.views = []
      this.blocksTotal = 0
      this.error = err
      this.notFound = err.code === 'not_found'
      this.status = 'error'
    } finally {
      if (this.#ctrl === ctrl) this.#ctrl = null
    }
  }

  /** Append the next page of blocks (block_offset = the blocks loaded so far). */
  async loadMoreBlocks(): Promise<void> {
    if (!this.hasMoreBlocks || this.loadingMoreBlocks) return
    const gen = this.#gen
    this.loadingMoreBlocks = true
    this.moreBlocksError = null
    const { ctrl, run } = this.#read(this.views.length)
    try {
      const res = await run
      if (gen !== this.#gen || ctrl.signal.aborted) return
      const id = this.recording?.id ?? this.#id
      this.views = [...this.views, ...res.blocks.map((b) => new BlockModel(this.#api, id, b, this.language))]
      // an empty page below the total would offer the button for ever: the end is where the blocks end
      this.blocksTotal = res.blocks.length === 0 ? this.views.length : res.blocks_total
      this.#refresh(res)
    } catch (e) {
      if (gen !== this.#gen || ctrl.signal.aborted) return
      this.moreBlocksError = recordingError(toApiError(e))
    } finally {
      if (gen === this.#gen) this.loadingMoreBlocks = false
      if (this.#ctrl === ctrl) this.#ctrl = null
    }
  }

  /**
   * A later page's metadata replaces the first page's: the recording is read
   * anew for every page, and an open one may have finished in between. A
   * side whose state, format or size changed gets a fresh raw reader (what
   * the old one loaded was the body as it was); an unchanged side keeps its
   * reader and the bytes it loaded.
   */
  #refresh(res: RecordingGetResponse): void {
    this.recording = res.recording
    for (const address of ['request', 'response'] as const) {
      const was = address === 'request' ? this.request : this.response
      const now = res[address]
      if (was !== null && was.state === now.state && was.format === now.format && was.bytes === now.bytes) continue
      this.#sides[address].dispose()
      this.#sides = { ...this.#sides, [address]: sideReader(this.#api, res.recording.id, now, address) }
    }
    this.request = res.request
    this.response = res.response
  }

  #disposeParts(): void {
    this.#ctrl?.abort()
    this.#ctrl = null
    for (const v of this.views) v.dispose()
    this.#sides.request.dispose()
    this.#sides.response.dispose()
  }

  /** Leaving the page: abort every running read and translation. */
  dispose(): void {
    this.#gen++
    this.#disposeParts()
  }
}
