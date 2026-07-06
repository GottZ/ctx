// Dispatch-section presentation helpers (inference-scheduler MW12b, design/05
// §4.5). DOM-free + pure so the coarsening / formatting invariants are vitest-
// covered without a component mount. The status_dispatch.go wire shapes carry
// the numbers; this file only FORMATS them — the exposure decisions (which
// fields exist per shape) live server-side (K13), never here.

import type { DispatchDepth, DispatchTenantTarget } from '../../lib/api/types'

// The coarse tenant-depth vocabulary → its display label. A CLOSED set: the
// tenant occupancy view shows a bucket, never an exact foreign waitQ count
// (E-A5-6b / F-B3). depthLabel maps ONLY the three known buckets and falls back
// to '—' for anything else — it must never echo a raw value (a number would
// leak the exact foreign depth the coarsening deliberately withholds).
export const DEPTH_LABELS: Record<DispatchDepth, string> = {
  leer: 'idle',
  niedrig: 'low',
  hoch: 'high',
}

export function depthLabel(d: string): string {
  return (DEPTH_LABELS as Record<string, string>)[d] ?? '—'
}

// depthClass maps the coarse depth to a closed CSS-class set (Q3: data→colour
// only through classes of a closed set, never a style attribute or a raw value).
export function depthClass(d: string): 'idle' | 'low' | 'high' | 'unknown' {
  switch (d) {
    case 'leer':
      return 'idle'
    case 'niedrig':
      return 'low'
    case 'hoch':
      return 'high'
    default:
      return 'unknown'
  }
}

// tenantForeignFields returns the keys of a tenant occupancy target that are NOT
// in the sanctioned coarsened set. The tenant shape must carry ONLY {origin,
// busy, depth} aggregates + the caller's OWN bucket (own_*) — NEVER a fair_key
// or an exact foreign count (F-B3, structural). A non-empty result means the
// wire shape widened into a cross-tenant load oracle; the exposure test asserts
// it stays empty, so a regression on either side (Go struct or this TS type)
// turns red.
const TENANT_ALLOWED_FIELDS = new Set([
  'origin',
  'busy',
  'depth',
  'own_waiting',
  'own_inflight',
  'own_oldest_wait_ms',
])

export function tenantForeignFields(t: DispatchTenantTarget): string[] {
  return Object.keys(t).filter((k) => !TENANT_ALLOWED_FIELDS.has(k))
}

// fmtMs renders an integer-millisecond measurand as a compact human string
// ('—' for a missing value, 'Nms' under a second, else 'N.Ns'). Used for wait
// aggregates + last-run ages.
export function fmtMs(ms: number | null | undefined): string {
  if (ms === null || ms === undefined) return '—'
  if (ms < 1000) return `${Math.round(ms)}ms`
  return `${(ms / 1000).toFixed(1)}s`
}

// fmtTokens renders a token count with thin thousands separators, '—' for null.
export function fmtTokens(n: number | null | undefined): string {
  if (n === null || n === undefined) return '—'
  return n.toLocaleString('en-US')
}
