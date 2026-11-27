// Pure helpers of the recordings view (issue #575) — no state, no DOM, no
// storage, node-testable:
//
//   - decodeBase64 / hexDump: a read window's base64 `data` to bytes, and the
//     lossless view of bytes that are not valid UTF-8 (offset + 16 bytes per
//     line — every byte recoverable from the dump);
//   - pickDefaultLanguage: the translation's default target language;
//   - localInputToRfc3339: a typed wall-clock filter value to the list's bound;
//   - recordingError: an ApiError of a recordings call to its named state;
//   - showRecordingsLink: the condition of the entry links on /gateway and
//     the consumer page.

import type { ApiError } from '../../lib/api'
import { recordingErrorCode, RECORDING_LANGUAGES } from '../../lib/api/recordings'
import type { Capabilities } from '../../lib/auth/capabilities'

/** The named state of a failed recordings call. */
export interface RecordingErrorState {
  /** The server's code (writeRecordingError), the HTTP class without one, or 'aborted'. */
  code: string
  /** What the page says. */
  text: string
  requestId: string | null
}

/** Bytes of a base64 window (the read action's `data`). */
export function decodeBase64(s: string): Uint8Array {
  const bin = atob(s)
  const out = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
  return out
}

const HEX_LINE = 16

/**
 * A hex dump of bytes that start at `baseOffset` of their block: one line per
 * 16 bytes — an 8-digit hex offset, two spaces, the bytes as two-digit hex in
 * two groups of eight. Lossless: every byte reads back from its line.
 */
export function hexDump(bytes: Uint8Array, baseOffset: number): string {
  const lines: string[] = []
  for (let i = 0; i < bytes.length; i += HEX_LINE) {
    const row = bytes.subarray(i, i + HEX_LINE)
    const hex = Array.from(row, (b) => b.toString(16).padStart(2, '0'))
    const left = hex.slice(0, 8).join(' ')
    const right = hex.slice(8).join(' ')
    const off = (baseOffset + i).toString(16).padStart(8, '0')
    lines.push(right === '' ? `${off}  ${left}` : `${off}  ${left}  ${right}`)
  }
  return lines.join('\n')
}

/**
 * Browser subtags that name a language of the set by another code: `no`
 * (Norwegian) is written as Bokmål, the server's `nb`.
 */
const LANGUAGE_ALIASES: Readonly<Record<string, string>> = { no: 'nb' }

/** The fallback target language. */
export const FALLBACK_LANGUAGE = 'en'

/**
 * The default target language of a translation: the primary subtag of the
 * admin's browser language when the server's closed set has it, else English.
 * The SPA has no i18n — its interface is English, so English is the fallback
 * a reader of this page reads in any case; the browser language is the better
 * first guess of the language the admin wants a foreign recording in.
 */
export function pickDefaultLanguage(navLanguage: string | null | undefined): string {
  const primary = (navLanguage ?? '').trim().toLowerCase().split(/[-_]/)[0] ?? ''
  const code = LANGUAGE_ALIASES[primary] ?? primary
  return Object.hasOwn(RECORDING_LANGUAGES, code) ? code : FALLBACK_LANGUAGE
}

const LOCAL_TIME = /^(\d{4})-(\d{2})-(\d{2})(?:[T ](\d{2}):(\d{2})(?::(\d{2}))?)?$/

/**
 * A time bound the viewer typed in the wall clock of the browser —
 * `YYYY-MM-DD`, `YYYY-MM-DD HH:MM` or `YYYY-MM-DD HH:MM:SS` (a `T` instead
 * of the space is read too, the datetime-local form) — as the list's RFC 3339
 * bound: '' is no bound, anything else (a day that does not exist included)
 * an error, never a silently dropped bound that widens the list.
 */
export function localInputToRfc3339(v: string): { ok: true; value: string | undefined } | { ok: false } {
  const s = v.trim()
  if (s === '') return { ok: true, value: undefined }
  const m = LOCAL_TIME.exec(s)
  if (m === null) return { ok: false }
  const [y, mo, d, h, mi, se] = m.slice(1).map((x) => (x === undefined ? 0 : Number(x)))
  const t = new Date(y ?? 0, (mo ?? 1) - 1, d ?? 0, h ?? 0, mi ?? 0, se ?? 0)
  // the Date constructor rolls an impossible field over (Feb 30 → Mar 2) — refuse that
  const same =
    t.getFullYear() === y && t.getMonth() === (mo ?? 1) - 1 && t.getDate() === d && t.getHours() === h && t.getMinutes() === mi && t.getSeconds() === se
  return same ? { ok: true, value: t.toISOString() } : { ok: false }
}

/** The text of each named state; the server's own text stands for any other code. */
const ERROR_TEXT: Readonly<Record<string, string>> = {
  no_eligible_backend:
    'No local backend may receive recorded content — the chat role needs a full-trust backend of this instance that is not external.',
  window_too_small: 'The context window of the local chat backends does not hold one part of this block.',
  not_translatable: 'This block holds no text to translate (a payload, tokens or bytes that are not UTF-8).',
  translate_busy: 'A translation of yours is running — wait for it to finish or stop it.',
  translate_failed: 'The model did not answer. Nothing was stored; try again.',
  canceled: 'The translation was canceled on the server.',
  audit_unavailable: 'The read could not be audited, so nothing was served.',
  quota_exceeded: 'The tenant quota does not admit the call.',
  invalid_language: 'This language is not one the server translates into.',
  not_found: 'Not found — no recording or no block at this address.',
  body_cleared: 'The retention cleared this body; its size stays.',
  no_body: 'The recording holds no body here.',
}

/**
 * The named state of a failed recordings call. A call the page aborted itself
 * (Stop, leaving the page, a newer translation of the block) is no error: the
 * models drop it before it gets here, and Stop shows as the stopped state.
 */
export function recordingError(err: ApiError): RecordingErrorState {
  const code = recordingErrorCode(err)
  let text = ERROR_TEXT[code] ?? err.message
  if (code === 'block_too_large') {
    const limit = err.details?.limit_runes
    text = `The block is too large to translate${typeof limit === 'number' ? ` (more than ${limit} characters)` : ''} — read it in the raw view.`
  }
  return { code, text, requestId: err.requestId }
}

/**
 * The entry links to the recordings (on /gateway and the consumer page): a
 * server-admin, and at least one of the listed consumers records. Nobody else
 * gets a link — the actions are server-admin only and the guard keeps the
 * route from everybody else.
 */
export function showRecordingsLink(caps: Capabilities, consumers: ReadonlyArray<{ record_bodies: boolean }>): boolean {
  return caps.tier === 'server-admin' && consumers.some((c) => c.record_bodies)
}
