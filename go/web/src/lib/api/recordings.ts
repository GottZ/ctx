// Recordings client (issue #575, docs/api.md "Recordings of a recording
// consumer"). One named function per server call — five POST /api/manage
// actions, every one server-admin only (tierServerAdmin):
//
//   listRecordingConsumers  model-recording-consumers  —
//   listRecordings          model-recording-list       {consumer_id, cursor?, since?, until?, op?, state?, status?, limit?}
//   getRecording            model-recording-get        {id}
//   readRecording           model-recording-read       {id, address, offset?, limit?}
//   translateRecording      model-recording-translate  {id, address, language, chunk?}
//
// Bodies are built from explicit field lists, never a spread of the caller's
// object: translate names a block by its address, never text (the server
// refuses an unknown field), so this client cannot send content anywhere.
// An optional field is left out rather than sent empty. read and translate
// take an AbortSignal (Stop, leaving the page).

import { apiFetch, type ApiError } from '../api'
import type {
  RecordingConsumersResponse,
  RecordingGetResponse,
  RecordingListResponse,
  RecordingOpFilter,
  RecordingReadResponse,
  RecordingStateFilter,
  RecordingStatusFilter,
  RecordingTranslateResponse,
} from './types'

/** The window size of a read: the server's default (recordingWindowDefault, 64 KiB). */
export const RECORDING_WINDOW_BYTES = 64 * 1024

/**
 * The target languages of a translation — the server's closed set
 * (llm.RecordingLanguages), by ISO 639-1 code, with the English name the
 * server puts into its prompt. A code outside it is a 400 invalid_language.
 */
export const RECORDING_LANGUAGES: Readonly<Record<string, string>> = {
  ar: 'Arabic',
  cs: 'Czech',
  da: 'Danish',
  de: 'German',
  el: 'Greek',
  en: 'English',
  es: 'Spanish',
  fi: 'Finnish',
  fr: 'French',
  he: 'Hebrew',
  hi: 'Hindi',
  hu: 'Hungarian',
  id: 'Indonesian',
  it: 'Italian',
  ja: 'Japanese',
  ko: 'Korean',
  nb: 'Norwegian Bokmål',
  nl: 'Dutch',
  pl: 'Polish',
  pt: 'Portuguese',
  ro: 'Romanian',
  ru: 'Russian',
  sv: 'Swedish',
  th: 'Thai',
  tr: 'Turkish',
  uk: 'Ukrainian',
  vi: 'Vietnamese',
  zh: 'Chinese',
}

/** model-recording-list parameters. */
export interface RecordingListParams {
  consumerId: string
  cursor?: string
  /** RFC 3339, created_at ≥ since. */
  since?: string
  /** RFC 3339, created_at < until. */
  until?: string
  ops?: RecordingOpFilter[]
  state?: RecordingStateFilter
  status?: RecordingStatusFilter
  limit?: number
}

/** model-recording-read parameters (offset/limit in bytes). */
export interface RecordingReadParams {
  id: string
  address: string
  offset: number
  limit: number
}

/** model-recording-translate parameters. */
export interface RecordingTranslateParams {
  id: string
  address: string
  language: string
  chunk: number
}

function manage<T>(action: string, data: Record<string, unknown>, signal?: AbortSignal): Promise<T> {
  return apiFetch<T>('/api/manage', {
    method: 'POST',
    body: JSON.stringify({ action, data }),
    ...(signal !== undefined ? { signal } : {}),
  })
}

/** model-recording-consumers — the consumers that record or have recordings, with this process's counts. */
export function listRecordingConsumers(): Promise<RecordingConsumersResponse> {
  return manage<RecordingConsumersResponse>('model-recording-consumers', {})
}

/** model-recording-list — one consumer's recordings, newest first; metadata and sizes only. */
export function listRecordings(p: RecordingListParams): Promise<RecordingListResponse> {
  const data: Record<string, unknown> = { consumer_id: p.consumerId }
  if (p.cursor !== undefined) data.cursor = p.cursor
  if (p.since !== undefined) data.since = p.since
  if (p.until !== undefined) data.until = p.until
  if (p.ops !== undefined && p.ops.length > 0) data.op = [...p.ops]
  if (p.state !== undefined) data.state = p.state
  if (p.status !== undefined) data.status = p.status
  if (p.limit !== undefined) data.limit = p.limit
  return manage<RecordingListResponse>('model-recording-list', data)
}

/** model-recording-get — one recording as blocks, each with a preview (audited). */
export function getRecording(id: string): Promise<RecordingGetResponse> {
  return manage<RecordingGetResponse>('model-recording-get', { id })
}

/** model-recording-read — one window of a block or of a whole body (audited). */
export function readRecording(p: RecordingReadParams, signal?: AbortSignal): Promise<RecordingReadResponse> {
  return manage<RecordingReadResponse>(
    'model-recording-read',
    { id: p.id, address: p.address, offset: p.offset, limit: p.limit },
    signal,
  )
}

/** model-recording-translate — one chunk of one block, by a model of this instance (audited, stored nowhere). */
export function translateRecording(p: RecordingTranslateParams, signal?: AbortSignal): Promise<RecordingTranslateResponse> {
  return manage<RecordingTranslateResponse>(
    'model-recording-translate',
    { id: p.id, address: p.address, language: p.language, chunk: p.chunk },
    signal,
  )
}

/**
 * The machine code of a recordings error: the view's envelope carries `code`
 * (writeRecordingError); the shared 403 of the dispatch table and a network
 * failure carry none, so the HTTP class (ApiError.code) stands in.
 */
export function recordingErrorCode(err: ApiError): string {
  const code = err.details?.code
  return typeof code === 'string' && code !== '' ? code : err.code
}
