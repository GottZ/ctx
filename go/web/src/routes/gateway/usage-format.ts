// Usage panel presentation (RC-D C6, wave W05-10, design/05 §4.6 + §7 gate,
// design/06 §4.8). Pure and DOM-free; the panel and the screen-reader table
// render from the SAME rows, so the table can never drift from the chart.
//
// The rules this file owns (each one a W05-10 gate):
//   - null ≠ 0: a value that is null, or any value of a point with
//     complete:false, renders NOT_RECORDED — never "0" (rot gegen `?? 0`).
//   - windowTotal sums the recorded values only; if any point is not
//     recorded the total is partial ("≥ N"), if none is recorded it is null.
//   - hist:false: the latency cells show the MEANS (avg_ms, avg_queue_ms,
//     avg_ttft_ms) marked with MEAN_MARK — never an empty column.
//   - rejected.auth is never a number per consumer (V-06-b: a 401 has no
//     consumer); rejectionCells drops it for a consumer-scoped series even
//     if the wire carries a number.
//
// RC-D S1: contract stubs — every formatter answers its fail-closed value
// ('' / null / []); the vitest suite is red against them by design.

import type { RejectReason, UsagePoint, UsageSeries } from '../../lib/api/types'

/** What a missing value reads as (never "0"). */
export const NOT_RECORDED = 'not recorded'

/** The marker of a mean shown where a percentile would stand (hist:false). */
export const MEAN_MARK = 'Ø'

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

/** The latency columns of one point: p50/p95 from histograms, or marked means. */
export interface LatencyCells {
  total: UsageCell
  queue: UsageCell
  ttft: UsageCell
  /** true = the cells show means (hist:false), marked with MEAN_MARK. */
  mean: boolean
}

/** One refusal cell (reason → count text). */
export interface RejectionCell {
  reason: RejectReason
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
}

/** The cell of one field of one point (null / incomplete ⇒ NOT_RECORDED). */
export function usageCell(point: UsagePoint, field: UsageField): UsageCell {
  void point
  void field
  return { text: '', recorded: false }
}

/** The total of one field over the points. */
export function windowTotal(points: readonly UsagePoint[], field: UsageField): WindowTotal {
  void points
  void field
  return { value: null, partial: false }
}

/** The text of a window total: NOT_RECORDED, "N" or "≥ N" when partial. */
export function totalText(t: WindowTotal): string {
  void t
  return ''
}

/** The latency cells of one point, by the response's hist flag. */
export function latencyCells(point: UsagePoint, hist: boolean): LatencyCells {
  void point
  void hist
  const empty: UsageCell = { text: '', recorded: false }
  return { total: empty, queue: empty, ttft: empty, mean: false }
}

/** The refusal cells of one point; `consumerScoped` drops `auth` (V-06-b). */
export function rejectionCells(point: UsagePoint, consumerScoped: boolean): RejectionCell[] {
  void point
  void consumerScoped
  return []
}

/** The table rows of one series — one per point, in time order. */
export function usageTableRows(series: UsageSeries, hist: boolean, consumerScoped: boolean): UsageTableRow[] {
  void series
  void hist
  void consumerScoped
  return []
}

/** One bar of the chart: x slot, height in [0, 1], or a not-recorded gap. */
export interface UsageBar {
  bucketStart: string
  /** Fraction of the series maximum; null = not recorded (drawn as a gap, never as height 0). */
  height: number | null
}

/** The bar geometry of one field (relative heights; a missing value is a gap). */
export function usageBars(series: UsageSeries, field: UsageField): UsageBar[] {
  void series
  void field
  return []
}
