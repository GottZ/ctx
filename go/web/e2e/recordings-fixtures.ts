// e2e mocks of the recordings view (issue #575): the five server-admin-only
// manage actions model-recording-* as go/internal/handler/model_recordings*.go
// answers them (docs/api.md "Recordings of a recording consumer"):
//   - every action is tierServerAdmin: below server-admin the shared 403
//     {success:false, error:'admin key required'} (no code) — the generated
//     deny test asserts the SPA never even asks;
//   - the view's own errors carry {success:false, error, code} (+ limit_runes);
//   - model-recording-list walks newest first with an opaque cursor; with a
//     filter (op/state/status) one call walks at most 2000 rows and then
//     answers scan_limit_reached with a cursor after the last walked row;
//   - model-recording-read answers windows of a block or a whole body as
//     base64 (utf8 only for a block); translate one Markdown chunk per call.
// Kept apart from fixtures.ts (one hook there, like gateway-panels-fixtures).
// The world never records anything a test sends — it is a pure function of
// the request, so a reload shows the same recording.

import type { Role, SeedState } from './fixtures'

/** The recording consumer of tenant A (record_bodies on). */
export const REC_CONSUMER = '01900000-0000-7000-8000-00000000c575'
/** A consumer switched off that keeps its recordings. */
export const REC_CONSUMER_OFF = '01900000-0000-7000-8000-00000000c576'
/** A deleted consumer whose rows stay (no foreign key). */
export const REC_CONSUMER_GONE = '01900000-0000-7000-8000-00000000c577'
/** The detail recording: a chat request with every block kind, a 502 answer that is not UTF-8. */
export const REC_ID = '01900000-0000-7000-8000-0000000575a1'
/** A recording of 1200 blocks (1150 request messages, 50 answer choices) — three pages of 500. */
export const REC_ID_MANY = '01900000-0000-7000-8000-0000000575b1'
/** The block count of REC_ID_MANY and how many of them belong to the answer. */
export const REC_MANY_TOTAL = 1200
export const REC_MANY_RESPONSES = 50

/** The hostile strings of the translated block (original AND translation carry them). */
export const REC_HOSTILE_LINES = [
  '<script>alert(1)</script>',
  '<img src=x onerror=alert(1)>',
  '[x](javascript:alert(1))',
  '![x](https://tracker.example/p.png)',
] as const

/** The original of request/messages/1 — a foreign-language prompt with the hostile lines inside. */
export const REC_USER_TEXT = [
  'Bitte fasse den Vertrag zusammen. Das Passwort für das Portal ist hunter2.',
  ...REC_HOSTILE_LINES,
  'Mehr unter https://example.org/vertrag',
].join('\n')

/** The two Markdown chunks the mock model answers for request/messages/1. */
export const REC_TRANSLATION = [
  [
    '## Request',
    '',
    'Please **summarise the contract**. The password for the portal is `hunter2`.',
    '',
    ...REC_HOSTILE_LINES.map((l) => `- ${l}`),
    '',
    'More at [the contract](https://example.org/vertrag).',
  ].join('\n'),
  'The rest of the block: *no further instructions*.',
] as const

/** The long block: 70 000 bytes; a 3-byte "€" straddles the first 64 KiB window boundary. */
export const REC_LONG_ADDRESS = 'request/messages/5'
const LONG_TOTAL = 70_000
const WINDOW = 65_536

const REC_ADDRESSES_TRANSLATE_FAILS: Record<string, { status: number; code: string; error: string }> = {
  // the tool result: no local backend may receive it (the named error state in the free spec)
  'request/messages/3': {
    status: 503,
    code: 'no_eligible_backend',
    error: 'no backend of this instance may receive a recording (chat role, full trust, not external)',
  },
}

interface MockAnswer {
  status: number
  json: unknown
}

const FORBIDDEN: MockAnswer = { status: 403, json: { success: false, error: 'admin key required' } }

function recError(status: number, code: string, error: string, extra: Record<string, unknown> = {}): MockAnswer {
  return { status, json: { success: false, error, code, ...extra } }
}

// btoa, not Buffer: svelte-check walks this file with the browser tsconfig
// (no node types) via routes/index.test.ts → registry.ts (as fixtures.ts).
function b64(bytes: Uint8Array): string {
  let bin = ''
  for (const b of bytes) bin += String.fromCharCode(b)
  return btoa(bin)
}

const enc = new TextEncoder()

/** The long block's bytes: ASCII lines, a "€" at offset WINDOW-1 (E2 | 82 AC across the boundary). */
function longBytes(): Uint8Array {
  const out = new Uint8Array(LONG_TOTAL)
  // ASCII only: the euro sign below is the one multi-byte character
  const line = enc.encode('log line of a long tool output - the model was asked to read all of it.\n')
  for (let i = 0; i < LONG_TOTAL; i++) out[i] = line[i % line.length] ?? 0x20
  // make the boundary region plain ASCII, then put the euro sign across it
  for (let i = WINDOW - 8; i < WINDOW + 8; i++) out[i] = 0x2e
  out.set([0xe2, 0x82, 0xac], WINDOW - 1)
  return out
}

/** The 502 answer the backend relayed: not UTF-8. */
const NOT_UTF8 = Uint8Array.from([0xff, 0xfe, 0x00, 0x41, 0x0a, 0x80, 0x7f, 0x20, 0xc3, 0x28, 0xa0, 0xa1, 0xe2, 0x28, 0xa1, 0x00, 0x62, 0x61, 0x64, 0x20, 0x67, 0x77, 0x00, 0xff])

const REQUEST_JSON = JSON.stringify({
  model: 'chat',
  temperature: 0.2,
  messages: [
    { role: 'system', content: 'You are a careful contract assistant.' },
    { role: 'user', content: REC_USER_TEXT },
  ],
})

function meta(over: Record<string, unknown>): Record<string, unknown> {
  return {
    id: REC_ID,
    consumer_id: REC_CONSUMER,
    created_at: '2026-06-29T11:59:30Z',
    finished_at: '2026-06-29T11:59:31Z',
    process_id: '7a1c5e00-0000-4000-8000-0000000005a1',
    model_key_id: '01900000-0000-7000-8000-0000000575k1',
    path: '/v1/chat/completions',
    op: 'chat',
    client_request_id: 'req-4711',
    model: 'chat',
    stream: false,
    declared_bytes: REQUEST_JSON.length,
    request_bytes: REQUEST_JSON.length,
    request_complete: true,
    state: 'done',
    status: 200,
    code: null,
    response_bytes: 812,
    request_cleared: false,
    response_null: false,
    ...over,
  }
}

/** The seven recordings of the default world, newest first. */
function defaultRows(): Record<string, unknown>[] {
  return [
    meta({ status: 502, code: 'backend_error', response_bytes: NOT_UTF8.length }),
    meta({ id: '01900000-0000-7000-8000-0000000575a2', created_at: '2026-06-29T11:58:00Z', finished_at: null, state: 'open', status: null, response_bytes: 0, response_null: true, stream: true }),
    meta({ id: '01900000-0000-7000-8000-0000000575a3', created_at: '2026-06-29T11:50:00Z', op: 'embed', path: '/v1/embeddings', model: 'embed', response_bytes: 0, response_null: true }),
    meta({ id: '01900000-0000-7000-8000-0000000575a4', created_at: '2026-06-29T11:40:00Z', op: 'rerank', path: '/v2/rerank', model: 'rerank', response_bytes: 412 }),
    meta({ id: '01900000-0000-7000-8000-0000000575a5', created_at: '2026-06-29T11:30:00Z', status: 429, code: 'rate_limited', response_bytes: 160, stream: true }),
    meta({ id: '01900000-0000-7000-8000-0000000575a6', created_at: '2026-06-28T09:00:00Z', request_cleared: true, response_null: true, response_bytes: 900 }),
    meta({ id: '01900000-0000-7000-8000-0000000575a7', created_at: '2026-06-27T08:00:00Z', op: null, model: null, stream: null, path: '/v1/chat/completions', status: 400, code: 'invalid_request', response_bytes: 96, declared_bytes: null }),
  ]
}

const SCALE_TOTAL = 10_000
const SCALE_BASE_MS = Date.UTC(2026, 5, 29, 11, 0, 0)
/** Every 2500th scale row answered 5xx — the rare filter that runs into the 2000-row walk bound. */
const SCALE_RARE_EVERY = 2500

function scaleRow(i: number): Record<string, unknown> {
  const rare = i % SCALE_RARE_EVERY === SCALE_RARE_EVERY - 1
  return meta({
    id: `01900000-0000-7000-8000-${String(i).padStart(12, '0')}`,
    created_at: new Date(SCALE_BASE_MS - i * 1000).toISOString(),
    finished_at: new Date(SCALE_BASE_MS - i * 1000 + 400).toISOString(),
    status: rare ? 503 : 200,
    code: rare ? 'backend_unavailable' : null,
    model: `chat-${String(i).padStart(5, '0')}`,
  })
}

const LIST_DEFAULT = 50
const LIST_MAX = 100
const SCAN_BOUND = 2000

function statusClass(status: unknown): string {
  if (typeof status !== 'number') return 'none'
  return `${Math.floor(status / 100)}xx`
}

function matches(row: Record<string, unknown>, d: Record<string, unknown>): boolean {
  const ops = Array.isArray(d.op) ? (d.op as string[]) : []
  if (ops.length > 0 && !ops.includes((row.op as string | null) ?? 'none')) return false
  if (typeof d.state === 'string' && d.state !== '' && row.state !== d.state) return false
  if (typeof d.status === 'string' && d.status !== '' && statusClass(row.status) !== d.status) return false
  const at = Date.parse(String(row.created_at))
  if (typeof d.since === 'string' && at < Date.parse(d.since)) return false
  if (typeof d.until === 'string' && at >= Date.parse(d.until)) return false
  return true
}

const LIST_FIELDS = new Set(['consumer_id', 'cursor', 'since', 'until', 'op', 'state', 'status', 'limit'])

function list(data: Record<string, unknown>, world: 'default' | 'empty' | '10k'): MockAnswer {
  for (const k of Object.keys(data)) if (!LIST_FIELDS.has(k)) return recError(400, 'invalid_body', 'invalid data for model-recording-list')
  for (const k of ['since', 'until']) {
    const v = data[k]
    if (v !== undefined && (typeof v !== 'string' || Number.isNaN(Date.parse(v)))) return recError(400, 'invalid_body', `${k} must be an RFC 3339 time`)
  }
  const consumer = data.consumer_id
  const rowsOf = (i: number): Record<string, unknown> | null => {
    if (consumer !== REC_CONSUMER || world === 'empty') return null
    if (world === '10k') return i < SCALE_TOTAL ? scaleRow(i) : null
    return defaultRows()[i] ?? null
  }
  const limit = typeof data.limit === 'number' ? Math.min(Math.max(data.limit, 1), LIST_MAX) : LIST_DEFAULT
  let start = 0
  if (typeof data.cursor === 'string') {
    const m = /^walk:(\d+)$/.exec(data.cursor)
    if (m === null) return recError(400, 'invalid_body', 'cursor is not one this list gave')
    start = Number(m[1])
  }
  const filtered = (Array.isArray(data.op) && data.op.length > 0) || (typeof data.state === 'string' && data.state !== '') || (typeof data.status === 'string' && data.status !== '')
  const out: Record<string, unknown>[] = []
  let i = start
  let scanned = 0
  for (;;) {
    const row = rowsOf(i)
    if (row === null) return { status: 200, json: { success: true, recordings: out, next_cursor: null, scanned, scan_limit_reached: false } }
    if (out.length === limit) {
      return { status: 200, json: { success: true, recordings: out, next_cursor: `walk:${i}`, scanned, scan_limit_reached: false } }
    }
    if (filtered && scanned === SCAN_BOUND) {
      return { status: 200, json: { success: true, recordings: out, next_cursor: `walk:${i}`, scanned, scan_limit_reached: true } }
    }
    scanned++
    if (matches(row, data)) out.push(row)
    i++
  }
}

function block(address: string, over: Record<string, unknown>): Record<string, unknown> {
  return {
    address,
    side: 'request',
    kind: 'text',
    bytes: 0,
    utf8: true,
    translatable: true,
    inline: true,
    preview: '',
    preview_truncated: false,
    preview_omitted: false,
    ...over,
  }
}

const TOOL_CALL = '{"name":"lookup_contract","arguments":"{\\"id\\":\\"C-17\\"}"}'
const TOOL_RESULT = '{"contract":"C-17","parties":["Acme","Globex"],"term_months":24}'
const REMOTE_IMAGE = 'https://tracker.example/cat.png'
const PARAMS = '{"model":"chat","temperature":0.2}'
const SYSTEM = 'You are a careful contract assistant.'

/** The full content of each block address (what read and translate take). */
function content(address: string): Uint8Array | null {
  switch (address) {
    case 'request/messages/0':
      return enc.encode(SYSTEM)
    case 'request/messages/1':
      return enc.encode(REC_USER_TEXT)
    case 'request/messages/2/tool_calls/0':
      return enc.encode(TOOL_CALL)
    case 'request/messages/3':
      return enc.encode(TOOL_RESULT)
    case 'request/messages/4/content/0':
      return enc.encode(REMOTE_IMAGE)
    case 'request/messages/4/content/1':
      return enc.encode('data:image/png;base64,' + 'iVBORw0KGgo'.repeat(40))
    case REC_LONG_ADDRESS:
      return longBytes()
    case 'request/params':
      return enc.encode(PARAMS)
    case 'response':
      return NOT_UTF8
    default:
      return null
  }
}

function utf8Of(bytes: Uint8Array): boolean {
  try {
    new TextDecoder('utf-8', { fatal: true }).decode(bytes)
    return true
  } catch {
    return false
  }
}

function detail(world: 'default' | 'empty' | '10k'): Record<string, unknown> {
  if (world === 'empty') {
    // the retention cleared both bodies: sizes stay, no block
    return {
      success: true,
      recording: meta({ request_cleared: true, response_null: true, status: 200, code: null, response_bytes: 900 }),
      request: { address: 'request', state: 'cleared', bytes: REQUEST_JSON.length },
      response: { address: 'response', state: 'cleared', bytes: 900 },
      blocks: [],
    }
  }
  const long = longBytes()
  const longPreview = new TextDecoder().decode(long.subarray(0, 220))
  return {
    success: true,
    recording: meta({ status: 502, code: 'backend_error', response_bytes: NOT_UTF8.length }),
    request: { address: 'request', state: 'present', format: 'json', bytes: REQUEST_JSON.length },
    response: { address: 'response', state: 'present', format: 'not_utf8', bytes: NOT_UTF8.length },
    blocks: [
      block('request/messages/0', { role: 'system', bytes: SYSTEM.length, preview: SYSTEM }),
      block('request/messages/1', { role: 'user', bytes: enc.encode(REC_USER_TEXT).length, preview: REC_USER_TEXT }),
      block('request/messages/2/tool_calls/0', { role: 'assistant', kind: 'tool_call', name: 'lookup_contract', tool_call_id: 'call_1', bytes: TOOL_CALL.length, preview: TOOL_CALL, translatable: false }),
      block('request/messages/3', { role: 'tool', kind: 'tool_result', tool_call_id: 'call_1', bytes: TOOL_RESULT.length, preview: TOOL_RESULT }),
      block('request/messages/4/content/0', { role: 'user', kind: 'image', bytes: REMOTE_IMAGE.length, preview: REMOTE_IMAGE, translatable: false }),
      block('request/messages/4/content/1', { role: 'user', kind: 'image', media_type: 'image/png', bytes: 462, inline: false, preview_omitted: true, translatable: false }),
      block(REC_LONG_ADDRESS, { role: 'user', bytes: LONG_TOTAL, preview: longPreview, preview_truncated: true }),
      block('request/params', { kind: 'json', bytes: PARAMS.length, preview: PARAMS, translatable: false }),
      block('response', { side: 'response', kind: 'raw', bytes: NOT_UTF8.length, utf8: false, translatable: false, preview_omitted: true }),
    ],
  }
}

/** The server's page bound of model-recording-get (recordingBlocksPage). */
const BLOCKS_PAGE = 500

/** One page of a detail answer: blocks[block_offset, +block_limit), with block_offset and blocks_total. */
function blockPage(all: Record<string, unknown>, d: Record<string, unknown>): MockAnswer {
  const offset = d.block_offset === undefined ? 0 : d.block_offset
  const limit = d.block_limit === undefined ? BLOCKS_PAGE : d.block_limit
  if (typeof offset !== 'number' || typeof limit !== 'number' || offset < 0 || limit < 1 || limit > BLOCKS_PAGE) {
    return recError(400, 'invalid_body', 'block_offset must be >= 0 and block_limit between 1 and 500')
  }
  const blocks = all.blocks as unknown[]
  return { status: 200, json: { ...all, blocks: blocks.slice(offset, offset + limit), block_offset: offset, blocks_total: blocks.length } }
}

/** REC_ID_MANY: a chat of many short messages and choices (the 100 000-message case, scaled down). */
function manyDetail(): Record<string, unknown> {
  const req = REC_MANY_TOTAL - REC_MANY_RESPONSES
  const blocks = Array.from({ length: REC_MANY_TOTAL }, (_, i) =>
    i < req
      ? block(`request/messages/${i}`, { role: 'user', bytes: String(i).length, preview: String(i) })
      : block(`response/choices/${i - req}/content`, { side: 'response', role: 'assistant', bytes: 2, preview: 'ok' }),
  )
  return {
    success: true,
    recording: meta({ id: REC_ID_MANY }),
    request: { address: 'request', state: 'present', format: 'json', bytes: 9000 },
    response: { address: 'response', state: 'present', format: 'json', bytes: 2400 },
    blocks,
  }
}

function read(data: Record<string, unknown>, world: 'default' | 'empty' | '10k'): MockAnswer {
  if (data.id !== REC_ID) return recError(404, 'not_found', 'recording not found')
  const address = String(data.address ?? '')
  const offset = typeof data.offset === 'number' ? data.offset : 0
  const limit = typeof data.limit === 'number' ? data.limit : WINDOW
  let bytes: Uint8Array | null
  let side = false
  if (address === 'request' || address === 'response') {
    if (world === 'empty') return recError(409, 'body_cleared', 'the retention cleared this body')
    bytes = address === 'request' ? enc.encode(REQUEST_JSON) : NOT_UTF8
    side = true
  } else {
    bytes = content(address)
  }
  if (bytes === null) return recError(404, 'not_found', 'no block at this address')
  const lo = Math.min(offset, bytes.length)
  const hi = Math.min(offset + limit, bytes.length)
  const json: Record<string, unknown> = { success: true, address, offset: lo, limit: hi - lo, total: bytes.length, eof: hi >= bytes.length, data: b64(bytes.subarray(lo, hi)) }
  if (!side) json.utf8 = utf8Of(bytes)
  return { status: 200, json }
}

const TRANSLATE_FIELDS = new Set(['id', 'address', 'language', 'chunk'])

function translate(data: Record<string, unknown>): MockAnswer {
  for (const k of Object.keys(data)) if (!TRANSLATE_FIELDS.has(k)) return recError(400, 'invalid_body', 'invalid data')
  if (data.id !== REC_ID) return recError(404, 'not_found', 'recording not found')
  const address = String(data.address ?? '')
  const fail = REC_ADDRESSES_TRANSLATE_FAILS[address]
  if (fail !== undefined) return recError(fail.status, fail.code, fail.error)
  if (content(address) === null) return recError(404, 'not_found', 'no block at this address')
  const chunk = typeof data.chunk === 'number' ? data.chunk : 0
  const parts: readonly string[] = address === 'request/messages/1' ? REC_TRANSLATION : [`Translated text of ${address}.`]
  const text = parts[chunk]
  if (text === undefined) return recError(400, 'invalid_body', 'chunk is out of range', { chunks: parts.length })
  return {
    status: 200,
    json: { success: true, id: REC_ID, address, chunk, chunks: parts.length, language: data.language, text, model: 'qwen38-27b', generated: true },
  }
}

function consumers(world: 'default' | 'empty' | '10k'): Record<string, unknown> {
  const rows =
    world === 'empty'
      ? []
      : [
          {
            consumer_id: REC_CONSUMER,
            label: 'rec-app',
            tenant_id: '550e8400-e29b-41d4-a716-446655440aaa',
            tenant_slug: 'acme',
            record_bodies: true,
            active: true,
            deleted: false,
            last_recorded_at: '2026-06-29T11:59:30Z',
            process: { admitted: 12, in_flight: 1, recorded: 10, unrecordable: 1, unfinished: 0, stream_answers_unrecorded: 0 },
          },
          {
            consumer_id: REC_CONSUMER_OFF,
            label: 'old-bot',
            tenant_id: '550e8400-e29b-41d4-a716-446655440bbb',
            tenant_slug: 'globex',
            record_bodies: false,
            active: false,
            deleted: false,
            last_recorded_at: '2026-06-20T08:00:00Z',
            process: null,
          },
          {
            consumer_id: REC_CONSUMER_GONE,
            label: null,
            tenant_id: null,
            tenant_slug: null,
            record_bodies: false,
            active: false,
            deleted: true,
            last_recorded_at: '2026-05-02T07:00:00Z',
            process: null,
          },
        ]
  return { success: true, process_id: '7a1c5e00-0000-4000-8000-0000000005a1', since: '2026-06-29T10:00:00Z', consumers: rows }
}

/**
 * The model-recording-* answers of a session, or null for any other action.
 * `state` picks the world: 'empty' = no consumer, no recording, the detail
 * with both bodies cleared; '10k' = 10 000 rows of the recording consumer.
 */
export function recordingsManage(action: string | undefined, data: Record<string, unknown> | undefined, role: Role, state: SeedState): MockAnswer | null {
  if (action === undefined || !action.startsWith('model-recording-')) return null
  if (role !== 'server-admin') return FORBIDDEN
  const world = state === 'empty' ? 'empty' : state === '10k' ? '10k' : 'default'
  const d = data ?? {}
  switch (action) {
    case 'model-recording-consumers':
      return { status: 200, json: consumers(world) }
    case 'model-recording-list':
      return list(d, world)
    case 'model-recording-get': {
      const all = d.id === REC_ID ? detail(world) : d.id === REC_ID_MANY ? manyDetail() : null
      return all === null ? recError(404, 'not_found', 'recording not found') : blockPage(all, d)
    }
    case 'model-recording-read':
      return read(d, world)
    case 'model-recording-translate':
      return translate(d)
    default:
      return recError(400, 'invalid_body', `unknown action ${action}`)
  }
}
