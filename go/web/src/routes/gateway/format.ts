// Pure render helpers of the /gateway list (wave W05-05). They only FORMAT
// what the server sent — a consumer's or key's status is the server's verdict
// at its own clock (design/05 §5 B-11); nothing here compares a timestamp with
// the client's clock (the B-11 scan in consumers.svelte.test.ts holds this
// directory to that).

import type { ConsumerStatus, ModelKeyStatus } from '../../lib/api/types'

/** Display labels of the consumer status vocabulary (design/05 §3.2). */
export const STATUS_LABEL: Record<ConsumerStatus, string> = {
  active: 'active',
  expiring: 'expiring',
  rotating: 'rotating',
  no_key: 'no key',
  inactive: 'inactive',
}

/** Badge tone of a consumer status: ok = serving, warn = needs attention, off = not serving. */
export function consumerTone(status: string): 'ok' | 'warn' | 'off' {
  if (status === 'active' || status === 'rotating') return 'ok'
  if (status === 'expiring' || status === 'no_key') return 'warn'
  return 'off'
}

/** What the consumer view says while recording is on (record_bodies, issue #571) — the server's flag, shown to every reader. */
export const recordingText =
  'on — every request of this consumer is held in full in context_llm_recordings with the answer it got (embedding vectors excepted), credentials in a prompt included; readable in the database and by a server-admin on the recordings page'

/** Badge tone of a key status: active ok, expiring/superseded (grace running) warn, dead off. */
export function keyTone(status: ModelKeyStatus | string): 'ok' | 'warn' | 'off' {
  if (status === 'active') return 'ok'
  if (status === 'expiring' || status === 'superseded') return 'warn'
  return 'off'
}

function parsed(ts: string): Date | null {
  const d = new Date(ts)
  return Number.isNaN(d.getTime()) ? null : d
}

/** Calendar day of a server timestamp in the viewer's locale; '—' when unparseable. */
export function fmtDay(ts: string): string {
  const d = parsed(ts)
  return d === null ? '—' : d.toLocaleDateString(undefined, { year: 'numeric', month: '2-digit', day: '2-digit' })
}

/**
 * A last-used timestamp to the minute — the server records last_used_at at
 * most once a minute per key (design/01 §3.4), so seconds would claim a
 * precision the value does not have; the page adds the "±1 min" mark.
 */
export function fmtMinute(ts: string): string {
  const d = parsed(ts)
  return d === null
    ? '—'
    : d.toLocaleString(undefined, { year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })
}

const BYTE_UNITS = ['KiB', 'MiB', 'GiB', 'TiB'] as const

/**
 * A byte count in binary units (recordings, #575): below 1 KiB exact
 * ("512 B"), above one decimal ("1.5 KiB"). null or an impossible value is a
 * dash — never a made-up 0.
 */
export function fmtBytes(n: number | null): string {
  if (n === null || !Number.isFinite(n) || n < 0) return '—'
  if (n < 1024) return `${Math.trunc(n)} B`
  let v = n / 1024
  let unit = 0
  while (v >= 1024 && unit < BYTE_UNITS.length - 1) {
    v /= 1024
    unit++
  }
  return `${v.toFixed(1)} ${BYTE_UNITS[unit]}`
}

/** A server timestamp to the second in the viewer's locale; '—' when unparseable (recordings, #575). */
export function fmtSecond(ts: string | null): string {
  const d = ts === null ? null : parsed(ts)
  return d === null
    ? '—'
    : d.toLocaleString(undefined, {
        year: 'numeric',
        month: '2-digit',
        day: '2-digit',
        hour: '2-digit',
        minute: '2-digit',
        second: '2-digit',
      })
}

/**
 * Toast line after a revoke / deactivate, from the server's
 * `cancelled_inflight` (design/05 §4.6, V-01-e, B-25): a number is what the
 * server ended; null (until W01-8 can end and count running requests) means
 * running streams finish on their own. Only a real number is ever shown as a
 * count — never a "0 ended" made up for null (Lead decision 4 of W05-06).
 */
export function cancelledText(n: unknown): string {
  if (typeof n === 'number' && Number.isFinite(n)) return `${n} running stream${n === 1 ? '' : 's'} ended.`
  return 'Running streams continue until they finish (live cancellation not available yet).'
}
