// usage-format.ts gates (RC-D C6 S1, W05-10 — design/05 §7: "`complete:false`
// bzw. `null` ⇒ „nicht erfasst" statt 0 — rot gegen `?? 0`; `rejected.auth`
// je Konsument nie als Zahl; `hist:false` ⇒ Ø-Werte mit Kennzeichnung — rot
// gegen leere Spalte; a11y-Tabelle zum Diagramm"). Red against the S1 stubs.
// S2 (Go wins, C2 llmusage.Point): percentile names queue_p50_ms/ttft_p50_ms…,
// one mean only (avg_ms — queue wait and TTFT have none on the wire, so under
// hist:false they read NOT_RECORDED, still never empty), `rejected` is a list
// of {reason, n, wait_ms_avg} (null before the ledger), series carry no label.

import { readFileSync, readdirSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'
import type { UsagePoint, UsageSeries } from '../../lib/api/types'
import {
  MEAN_MARK,
  NOT_RECORDED,
  latencyCells,
  rejectionCells,
  rejectionsText,
  totalText,
  usageBars,
  usageCell,
  usageTableRows,
  windowTotal,
} from './usage-format'

function point(over: Partial<UsagePoint> = {}): UsagePoint {
  return {
    bucket_start: '2026-06-29T10:00:00Z',
    complete: true,
    calls: 12,
    errors: 1,
    estimated_calls: 0,
    prompt_tokens: 3400,
    completion_tokens: 900,
    gpu_ms: 15_000,
    cost_usd: null,
    avg_ms: 820,
    p50_ms: 700,
    p95_ms: 1900,
    queue_p50_ms: 9,
    queue_p95_ms: 40,
    ttft_p50_ms: 180,
    ttft_p95_ms: 420,
    rejected: [{ reason: 'rate', n: 3, wait_ms_avg: 0 }],
    ...over,
  }
}

describe('null ≠ 0 (W05-10: "not recorded" instead of 0)', () => {
  it('a recorded 0 reads "0" (positive control) and a number is formatted', () => {
    expect(usageCell(point({ errors: 0 }), 'errors'), 'gate W05-10: a recorded zero stays a zero').toEqual({
      text: '0',
      recorded: true,
    })
    expect(usageCell(point({ prompt_tokens: 3400 }), 'prompt_tokens')).toEqual({ text: '3,400', recorded: true })
  })

  it('null reads NOT_RECORDED — never "0"', () => {
    const c = usageCell(point({ calls: null }), 'calls')
    expect(c, 'gate W05-10: null ⇒ "not recorded" (rot gegen ?? 0)').toEqual({ text: NOT_RECORDED, recorded: false })
  })

  it('every value of a complete:false bucket reads NOT_RECORDED, even a wire 0', () => {
    const p = point({ complete: false, calls: 0, errors: 0 })
    expect(usageCell(p, 'calls').text, 'gate W05-10: complete:false nie als 0').toBe(NOT_RECORDED)
    expect(usageCell(p, 'errors').text).toBe(NOT_RECORDED)
  })

  it('a window total sums the recorded values and is partial over an unrecorded bucket', () => {
    const pts = [point({ calls: 5 }), point({ calls: null }), point({ calls: 7, complete: false }), point({ calls: 2 })]
    const t = windowTotal(pts, 'calls')
    expect(t, 'gate W05-10: total over recorded points only').toEqual({ value: 7, partial: true })
    expect(totalText(t)).toBe('≥ 7')
  })

  it('a window with no recorded value has a null total, read NOT_RECORDED (rot gegen ?? 0)', () => {
    const t = windowTotal([point({ calls: null }), point({ complete: false })], 'calls')
    expect(t, 'gate W05-10: nothing recorded ⇒ null, not 0').toEqual({ value: null, partial: true })
    expect(totalText(t)).toBe(NOT_RECORDED)
    expect(totalText({ value: 0, partial: false }), 'positive control: a recorded zero total').toBe('0')
  })
})

describe('hist:false (W05-10: marked means instead of an empty column)', () => {
  it('hist:true shows p50/p95 from the histograms, not marked', () => {
    const l = latencyCells(point(), true)
    expect(l.mean, 'gate W05-10: percentiles when histograms exist').toBe(false)
    expect(l.total.text).toBe('700ms / 1.9s')
  })

  it('hist:false shows the mean marked with Ø — never an empty cell', () => {
    const l = latencyCells(point({ p50_ms: null, p95_ms: null, queue_p50_ms: null, queue_p95_ms: null, ttft_p50_ms: null, ttft_p95_ms: null }), false)
    expect(l.mean, 'gate W05-10: means flagged').toBe(true)
    expect(l.total.text, 'gate W05-10: hist:false ⇒ Ø-Wert (rot gegen leere Spalte)').toBe(`${MEAN_MARK} 820ms`)
    // S2 hardening (drawn mutant #7 survived): the mean IS a recorded value (not styled as missing)
    expect(l.total.recorded, 'the marked mean counts as recorded').toBe(true)
    // queue wait and TTFT have no mean on the wire (C2) — not recorded, never empty, never 0
    expect(l.queue.text).toBe(NOT_RECORDED)
    expect(l.ttft.text).toBe(NOT_RECORDED)
    for (const c of [l.total, l.queue, l.ttft]) expect(c.text.trim(), 'no empty latency column').not.toBe('')
  })

  it('hist:false with a null mean still reads NOT_RECORDED, not empty and not 0', () => {
    const l = latencyCells(point({ avg_ms: null, p50_ms: null, p95_ms: null }), false)
    expect(l.total.text, 'gate W05-10 + null ≠ 0').toBe(NOT_RECORDED)
  })
})

describe('rejected.auth per consumer (W05-10 / V-06-b)', () => {
  it('other refusal reasons render their count (positive control)', () => {
    const cells = rejectionCells(point({ rejected: [{ reason: 'quota', n: 1, wait_ms_avg: null }, { reason: 'rate', n: 3, wait_ms_avg: 0 }] }), true)
    expect(cells, 'gate W05-10: refusals per reason are shown').toEqual([
      { reason: 'rate', text: '3' },
      { reason: 'quota', text: '1' },
    ])
  })

  it('auth is never a number per consumer — even when the wire carries one', () => {
    const p = point({ rejected: [{ reason: 'auth', detail: 'invalid_key', n: 7, wait_ms_avg: null }, { reason: 'rate', n: 2, wait_ms_avg: 0 }] })
    const cells = rejectionCells(p, true)
    expect(cells.find((c) => c.reason === 'auth'), 'gate W05-10: rejected.auth je Konsument nie als Zahl').toBeUndefined()
    expect(cells, 'positive control: rate stays').toEqual([{ reason: 'rate', text: '2' }])
  })

  it('several details of one reason are summed; rejected:null reads not recorded, [] none (S2)', () => {
    const p = point({ rejected: [{ reason: 'rate', detail: 'rate_requests', n: 2, wait_ms_avg: 0 }, { reason: 'rate', detail: 'rate_tokens', n: 5, wait_ms_avg: 0 }] })
    expect(rejectionCells(p, true)).toEqual([{ reason: 'rate', text: '7' }])
    expect(rejectionsText(point({ rejected: null }), true)).toBe(NOT_RECORDED)
    expect(rejectionsText(point({ rejected: [] }), true)).toBe('none')
  })

  it('an incomplete bucket has no final refusals: not recorded (review L3)', () => {
    const p = point({ complete: false, rejected: [{ reason: 'rate', n: 2, wait_ms_avg: 0 }] })
    expect(rejectionCells(p, true), 'gate W05-10: complete:false nie als Zahl').toEqual([])
    expect(rejectionsText(p, true)).toBe(NOT_RECORDED)
  })

  it('an instance-wide (not consumer-scoped) series may show auth', () => {
    const p = point({ rejected: [{ reason: 'auth', n: 7, wait_ms_avg: null }] })
    expect(rejectionCells(p, false)).toEqual([{ reason: 'auth', text: '7' }])
  })
})

describe('a11y table == chart rows (W05-10)', () => {
  const series: UsageSeries = {
    key: 'c1',
    points: [point({ bucket_start: '2026-06-29T09:00:00Z' }), point({ bucket_start: '2026-06-29T10:00:00Z', calls: null }), point({ bucket_start: '2026-06-29T11:00:00Z', calls: 6 })],
  }

  it('one table row per bucket, in time order, from the same values as the bars', () => {
    const rows = usageTableRows(series, true, true)
    expect(rows.map((r) => r.bucketStart), 'gate W05-10: a11y table row per bar').toEqual(series.points.map((p) => p.bucket_start))
    expect(rows.map((r) => r.calls.text)).toEqual(['12', NOT_RECORDED, '6'])
    const bars = usageBars(series, 'calls')
    expect(bars.map((b) => b.bucketStart)).toEqual(rows.map((r) => r.bucketStart))
  })

  it('a not-recorded bucket is a gap in the chart (height null), never a zero-height bar', () => {
    const bars = usageBars(series, 'calls')
    expect(bars.map((b) => b.height), 'gate W05-10: gap, not 0').toEqual([1, null, 0.5])
  })
})

// The literal gate "rot gegen `?? 0`": no coalescing of a usage value to 0 in
// the panel surface. A structural ratchet — green by construction against the
// stubs; its red proof is the S2 overlay mut/w05-10-coalesce-zero.json.
describe('no `?? 0` / `|| 0` in the usage surface (W05-10 ratchet)', () => {
  it('the usage files carry no zero-coalescing', () => {
    const zero = /\?\?\s*0(?![.\d])|\|\|\s*0(?![.\d])/
    for (const bad of ['x ?? 0', 'x?? 0', 'x || 0']) expect(bad, bad).toMatch(zero)
    for (const ok of ['x ?? 0.5', 'x ?? null', 'a || b']) expect(ok, ok).not.toMatch(zero)
    const dir = new URL('.', import.meta.url).pathname
    const files = readdirSync(dir).filter((f) => /^(usage|UsagePanel|SeriesBars)/.test(f) && !f.endsWith('.test.ts'))
    expect(files.length, 'the scan sees the usage surface').toBeGreaterThanOrEqual(4)
    for (const f of files) expect(readFileSync(join(dir, f), 'utf8'), f).not.toMatch(zero)
  })
})
