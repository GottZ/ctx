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
