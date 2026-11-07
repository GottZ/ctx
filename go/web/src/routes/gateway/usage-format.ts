// Usage panel presentation (RC-D C6, wave W05-10, design/05 §4.6 + §7 gate,
// design/06 §4.8; wire = C2 llmusage.Result). Pure and DOM-free; the panel
// and the screen-reader table render from the SAME rows, so the table can
// never drift from the chart.
//
// The rules this file owns (each one a W05-10 gate):
//   - null ≠ 0: a value that is null, or any value of a point with
//     complete:false (before the rollup's coverage, or the running bucket),
//     renders NOT_RECORDED — never "0" (the zero-coalescing gate).
//   - windowTotal sums the recorded values only; if any point is not
//     recorded the total is partial ("≥ N"), if none is recorded it is null.
//   - hist:false: the latency column shows the MEAN (avg_ms) marked with
//     MEAN_MARK — never an empty column. Queue wait and TTFT have no mean on
//     the wire (llmusage.Point), so without histograms they read NOT_RECORDED.
//   - rejected.auth is never a number per consumer (V-06-b: a 401 has no
//     consumer); rejectionCells drops it for a consumer-scoped series even
//     if the wire carries one. rejected:null (before the ledger) reads
//     NOT_RECORDED, [] reads "none".

import type { UsagePoint, UsageSeries } from '../../lib/api/types'
import { fmtMs } from '../status/dispatch'

/** What a missing value reads as (never "0"). */
export const NOT_RECORDED = 'not recorded'

/** The marker of a mean shown where a percentile would stand (hist:false). */
export const MEAN_MARK = 'Ø'

/** The refusal reasons in display order (keyquota/ledger.go). Unknown reasons follow, sorted. */
const REASON_ORDER = ['rate', 'quota', 'concurrency', 'capacity', 'model', 'federation', 'auth']

/** The numeric fields of a point the panel totals and tabulates. */
export type UsageField =
  | 'calls'
  | 'errors'
  | 'estimated_calls'
  | 'prompt_tokens'
  | 'completion_tokens'
  | 'gpu_ms'
  | 'cost_usd'

/** One rendered cell: its text and whether it carries a recorded number. */
export interface UsageCell {
  text: string
  recorded: boolean
}

/** The total of a field over a window: recorded sum + whether it is partial. */
export interface WindowTotal {
  /** Sum of the recorded values; null = not a single value recorded. */
  value: number | null
  /** At least one point of the window is not recorded. */
  partial: boolean
}

/** The latency columns of one point: p50/p95 from histograms, or the marked mean. */
export interface LatencyCells {
  total: UsageCell
  queue: UsageCell
  ttft: UsageCell
  /** true = the total cell shows the mean (hist:false), marked with MEAN_MARK. */
  mean: boolean
}

/** One refusal cell (reason → count text). */
export interface RejectionCell {
  reason: string
  text: string
}

/** One row of the screen-reader table (and of the bar chart). */
export interface UsageTableRow {
  bucketStart: string
  calls: UsageCell
  errors: UsageCell
  tokens: UsageCell
  latency: LatencyCells
  rejections: RejectionCell[]
  /** The refusals as one text: NOT_RECORDED (before the ledger), "none", or "rate 3, quota 1". */
  rejectionsText: string
}

const NOT: UsageCell = { text: NOT_RECORDED, recorded: false }

function fmtNumber(field: UsageField, v: number): string {
  if (field === 'cost_usd') return `$${v.toFixed(4)}`
  if (field === 'gpu_ms') return fmtMs(v)
  return v.toLocaleString('en-US')
}

/** The recorded value of a field, or null (incomplete point or null value). */
function recorded(point: UsagePoint, field: UsageField): number | null {
  if (!point.complete) return null
  const v = point[field]
  return typeof v === 'number' && Number.isFinite(v) ? v : null
}

/** The cell of one field of one point (null / incomplete ⇒ NOT_RECORDED). */
export function usageCell(point: UsagePoint, field: UsageField): UsageCell {
  const v = recorded(point, field)
  return v === null ? NOT : { text: fmtNumber(field, v), recorded: true }
}

/** The total of one field over the points. */
export function windowTotal(points: readonly UsagePoint[], field: UsageField): WindowTotal {
  let value: number | null = null
  let partial = false
  for (const p of points) {
    const v = recorded(p, field)
    if (v === null) partial = true
    else value = (value === null ? v : value + v)
  }
  return { value, partial }
}

/** The text of a window total: NOT_RECORDED, "N" or "≥ N" when partial. */
export function totalText(t: WindowTotal): string {
  if (t.value === null) return NOT_RECORDED
  const n = t.value.toLocaleString('en-US')
  return t.partial ? `≥ ${n}` : n
}

function pair(p50: number | null, p95: number | null): UsageCell {
  if (p50 === null || p95 === null) return NOT
  return { text: `${fmtMs(p50)} / ${fmtMs(p95)}`, recorded: true }
}

/** The latency cells of one point, by the response's hist flag. */
export function latencyCells(point: UsagePoint, hist: boolean): LatencyCells {
  if (!point.complete) return { total: NOT, queue: NOT, ttft: NOT, mean: !hist }
  if (hist) {
    return {
      total: pair(point.p50_ms, point.p95_ms),
      queue: pair(point.queue_p50_ms, point.queue_p95_ms),
      ttft: pair(point.ttft_p50_ms, point.ttft_p95_ms),
      mean: false,
    }
  }
  const avg = point.avg_ms
  return {
    total: avg === null ? NOT : { text: `${MEAN_MARK} ${fmtMs(avg)}`, recorded: true },
    queue: NOT,
    ttft: NOT,
    mean: true,
  }
}

function reasonRank(r: string): number {
  const i = REASON_ORDER.indexOf(r)
  return i === -1 ? REASON_ORDER.length : i
}

/** The refusal cells of one point (n summed over details); `consumerScoped` drops `auth` (V-06-b). */
export function rejectionCells(point: UsagePoint, consumerScoped: boolean): RejectionCell[] {
  if (point.rejected === null) return []
  const sums = new Map<string, number>()
  for (const r of point.rejected) {
    if (consumerScoped && r.reason === 'auth') continue
    if (typeof r.n !== 'number' || !Number.isFinite(r.n)) continue
    const prev = sums.get(r.reason)
    sums.set(r.reason, prev === undefined ? r.n : prev + r.n)
  }
  return [...sums.entries()]
    .sort((a, b) => reasonRank(a[0]) - reasonRank(b[0]) || a[0].localeCompare(b[0]))
    .map(([reason, n]) => ({ reason, text: n.toLocaleString('en-US') }))
}

/** The refusals of one point as one text. */
export function rejectionsText(point: UsagePoint, consumerScoped: boolean): string {
  if (point.rejected === null) return NOT_RECORDED
  const cells = rejectionCells(point, consumerScoped)
  return cells.length === 0 ? 'none' : cells.map((c) => `${c.reason} ${c.text}`).join(', ')
}

function tokensCell(point: UsagePoint): UsageCell {
  const p = recorded(point, 'prompt_tokens')
  const c = recorded(point, 'completion_tokens')
  if (p === null || c === null) return NOT
  return { text: (p + c).toLocaleString('en-US'), recorded: true }
}

/** The table rows of one series — one per point, in time order. */
export function usageTableRows(series: UsageSeries, hist: boolean, consumerScoped: boolean): UsageTableRow[] {
  return series.points.map((p) => ({
    bucketStart: p.bucket_start,
    calls: usageCell(p, 'calls'),
    errors: usageCell(p, 'errors'),
    tokens: tokensCell(p),
    latency: latencyCells(p, hist),
    rejections: rejectionCells(p, consumerScoped),
    rejectionsText: rejectionsText(p, consumerScoped),
  }))
}

/** One bar of the chart: x slot, height in [0, 1], or a not-recorded gap. */
export interface UsageBar {
  bucketStart: string
  /** Fraction of the series maximum; null = not recorded (drawn as a gap, never as height 0). */
  height: number | null
}

/** The bar geometry of one field (relative heights; a missing value is a gap). */
export function usageBars(series: UsageSeries, field: UsageField): UsageBar[] {
  const values = series.points.map((p) => recorded(p, field))
  let max: number | null = null
  for (const v of values) if (v !== null && (max === null || v > max)) max = v
  return series.points.map((p, i) => {
    const v = values[i] ?? null
    if (v === null) return { bucketStart: p.bucket_start, height: null }
    // a recorded value under a zero maximum is itself 0: a zero-height bar (recorded), not a gap
    return { bucketStart: p.bucket_start, height: max !== null && max > 0 ? v / max : 0 }
  })
}
