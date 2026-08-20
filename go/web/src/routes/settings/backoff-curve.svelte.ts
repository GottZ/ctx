// Math + save-reaction model behind the dream back-off curve editor. The
// component renders and drags; everything derivable lives here so vitest
// covers it DOM-free: the Go-mirror of the cooldown curve
// (dream.BackoffConfig.effectiveCooldownHours — keep in sync), the hours
// draft parser/renderer (config parseCooldownHours / renderHours), the
// factor solver behind curve-dragging, and the post-save watcher that
// triggers the pipeline re-evaluation (dream-backoff-restamp) exactly once
// per completed save.

import { toApiError } from '../../lib/api'
import { fetchDreamStats, restampDreamBackoff } from '../../lib/api/status'
import type { DreamStatsResponse, DreamBackoffRestampResponse } from '../../lib/api/types'

/** Go-side constants for mode=off (dream.CooldownActiveDays/InertDays). */
const OFF_ACTIVE_DAYS = 3
const OFF_INERT_DAYS = 14

export type BackoffMode = 'exp' | 'log' | 'linear' | 'off'

export interface BackoffPolicy {
  mode: BackoffMode
  factor: number
  grace: number
  minHours: number
  capHours: number
  inertOffset: number
}

/** The six registry keys the curve edits, in registry order. */
export const BACKOFF_KEYS = [
  'dream.backoff_mode',
  'dream.backoff_factor',
  'dream.backoff_grace',
  'dream.backoff_cap',
  'dream.backoff_min',
  'dream.backoff_inert_offset',
] as const

const unitHours: Record<string, number> = { h: 1, d: 24, w: 24 * 7, m: 24 * 30, y: 24 * 365 }

/** Mirror of config.parseCooldownHours: suffix h|d|w|m|y or bare hours. */
export function parseHours(s: string): number | null {
  const t = s.trim()
  if (t === '') return null
  const last = t[t.length - 1].toLowerCase()
  const mult = unitHours[last]
  if (mult !== undefined) {
    const n = Number(t.slice(0, -1).trim())
    if (!Number.isFinite(n) || n < 0) return null
    return n * mult
  }
  const n = Number(t)
  if (!Number.isFinite(n) || n < 0) return null
  return n
}

/** Mirror of config.renderHours: whole-day multiples as "Nd", else "Nh". */
export function fmtHoursDraft(hours: number): string {
  if (hours >= 24 && hours % 24 === 0) return `${hours / 24}d`
  // Trim float noise from drag math to keep drafts readable ("13.5h").
  const rounded = Math.round(hours * 10) / 10
  return `${rounded}h`
}

/**
 * Go-mirror of dream.BackoffConfig.effectiveCooldownHours: cooldown in hours
 * for a block at pre-increment eval count n. Floor 1h, ceiling capHours;
 * mode=off uses the fixed active/inert day constants.
 */
export function cooldownHours(p: BackoffPolicy, n: number, inert: boolean): number {
  if (p.mode === 'off') return (inert ? OFF_INERT_DAYS : OFF_ACTIVE_DAYS) * 24
  let x = n - p.grace
  if (inert) x += p.inertOffset
  if (x < 0) x = 0
  let h = p.minHours
  switch (p.mode) {
    case 'exp':
      h = p.minHours * Math.pow(p.factor, x)
      break
    case 'log':
      h = p.minHours * (1 + p.factor * Math.log(1 + x))
      break
    case 'linear':
      h = p.minHours + p.factor * 24 * x
      break
  }
  if (h > p.capHours) h = p.capHours
  if (h < 1) h = 1
  return h
}

const isMode = (s: string): s is BackoffMode => s === 'exp' || s === 'log' || s === 'linear' || s === 'off'

/**
 * Build the would-be policy from the six drafts. Invalid drafts are listed
 * and the policy is null — the curve shows its stale state plus a notice
 * instead of guessing (the V6 server rules are the authority).
 */
export function policyFromDrafts(draft: (key: string) => string): { policy: BackoffPolicy | null; invalid: string[] } {
  const invalid: string[] = []
  const mode = draft('dream.backoff_mode').trim()
  if (!isMode(mode)) invalid.push('dream.backoff_mode')
  const factor = Number(draft('dream.backoff_factor').trim())
  if (!Number.isFinite(factor) || factor < 0) invalid.push('dream.backoff_factor')
  const grace = Number(draft('dream.backoff_grace').trim())
  if (!Number.isInteger(grace) || grace < 0) invalid.push('dream.backoff_grace')
  const capHours = parseHours(draft('dream.backoff_cap'))
  if (capHours === null || capHours <= 0) invalid.push('dream.backoff_cap')
  const minHours = parseHours(draft('dream.backoff_min'))
  if (minHours === null) invalid.push('dream.backoff_min')
  const inertOffset = Number(draft('dream.backoff_inert_offset').trim())
  if (!Number.isInteger(inertOffset) || inertOffset < 0) invalid.push('dream.backoff_inert_offset')
  if (invalid.length > 0) return { policy: null, invalid }
  return {
    policy: {
      mode: mode as BackoffMode,
      factor,
      grace,
      capHours: capHours as number,
      minHours: minHours as number,
      inertOffset,
    },
    invalid,
  }
}

/**
 * Solve the factor that makes the ACTIVE curve pass through (n, hours).
 * Null when the point cannot determine a factor: n inside the grace plateau
 * (x <= 0), mode off, or a target below the min floor. Results are clamped
 * to >= 0 (V6) and rounded to two decimals — drag precision beyond that is
 * pointer noise, not intent.
 */
export function solveFactor(p: BackoffPolicy, n: number, hours: number): number | null {
  const x = n - p.grace
  if (p.mode === 'off' || x <= 0) return null
  const target = Math.max(hours, 1)
  let f: number
  switch (p.mode) {
    case 'exp':
      if (p.minHours <= 0 || target < p.minHours) return null
      f = Math.pow(target / p.minHours, 1 / x)
      break
    case 'log':
      if (p.minHours <= 0 || target < p.minHours) return null
      f = (target / p.minHours - 1) / Math.log(1 + x)
      break
    case 'linear':
      if (target < p.minHours) return null
      f = (target - p.minHours) / (24 * x)
      break
    default:
      return null
  }
  if (!Number.isFinite(f) || f < 0) return null
  return Math.round(f * 100) / 100
}

/**
 * Upper x-axis bound: far enough to show the cap plateau and the corpus's
 * actual maturity, never a stub axis. Bounded to keep the SVG sane when a
 * flat curve never reaches the cap.
 */
export function xAxisMax(p: BackoffPolicy | null, corpusMaxEval: number): number {
  let n = Math.max(12, corpusMaxEval + 4)
  if (p !== null && p.mode !== 'off') {
    let atCap = 0
    while (atCap < 60 && cooldownHours(p, atCap, false) < p.capHours) atCap++
    n = Math.max(n, atCap + 3)
  }
  return Math.min(n, 80)
}

/** Catmull-Rom → cubic-bezier SVG path through the given points. */
export function bezierPath(pts: Array<{ x: number; y: number }>): string {
  if (pts.length === 0) return ''
  if (pts.length === 1) return `M ${pts[0].x} ${pts[0].y}`
  let d = `M ${pts[0].x.toFixed(2)} ${pts[0].y.toFixed(2)}`
  for (let i = 0; i < pts.length - 1; i++) {
    const p0 = pts[Math.max(0, i - 1)]
    const p1 = pts[i]
    const p2 = pts[i + 1]
    const p3 = pts[Math.min(pts.length - 1, i + 2)]
    const c1x = p1.x + (p2.x - p0.x) / 6
    const c1y = p1.y + (p2.y - p0.y) / 6
    const c2x = p2.x - (p3.x - p1.x) / 6
    const c2y = p2.y - (p3.y - p1.y) / 6
    d += ` C ${c1x.toFixed(2)} ${c1y.toFixed(2)}, ${c2x.toFixed(2)} ${c2y.toFixed(2)}, ${p2.x.toFixed(2)} ${p2.y.toFixed(2)}`
  }
  return d
}

type RestampPhase = 'idle' | 'running' | 'done' | 'error'

/**
 * Post-save reaction: when the SAVED values of the six keys change (a
 * completed PUT — not a draft edit, not the initial catalog load), run the
 * dream-backoff-restamp manage action and refetch dream-stats so the
 * histogram shows the re-evaluated pipeline. Debounced past saveGroup's
 * sequential PUTs so one Save triggers exactly one restamp.
 */
export class BackoffSaveWatcher {
  stats = $state<DreamStatsResponse | null>(null)
  phase = $state<RestampPhase>('idle')
  restamped = $state<DreamBackoffRestampResponse | null>(null)
  errorMessage = $state<string | null>(null)

  #restamp: () => Promise<DreamBackoffRestampResponse>
  #fetchStats: () => Promise<DreamStatsResponse>
  #debounceMs: number
  #fingerprint: string | null = null
  #timer: ReturnType<typeof setTimeout> | null = null
  #seq = 0

  constructor(
    api: {
      restamp?: () => Promise<DreamBackoffRestampResponse>
      fetchStats?: () => Promise<DreamStatsResponse>
    } = {},
    debounceMs = 400,
  ) {
    this.#restamp = api.restamp ?? restampDreamBackoff
    this.#fetchStats = api.fetchStats ?? fetchDreamStats
    this.#debounceMs = debounceMs
  }

  /** Initial histogram load (no restamp — nothing was saved yet). */
  async loadStats(): Promise<void> {
    const seq = ++this.#seq
    try {
      const stats = await this.#fetchStats()
      if (seq === this.#seq) this.stats = stats
    } catch {
      // the curve renders without the histogram; the next save retries
    }
  }

  /**
   * Feed the current SAVED values (joined fingerprint) on every render.
   * First sighting arms the baseline; a later change schedules the restamp.
   */
  sync(fingerprint: string): void {
    if (this.#fingerprint === null) {
      this.#fingerprint = fingerprint
      return
    }
    if (fingerprint === this.#fingerprint) return
    this.#fingerprint = fingerprint
    if (this.#timer !== null) clearTimeout(this.#timer)
    this.#timer = setTimeout(() => {
      this.#timer = null
      void this.#run()
    }, this.#debounceMs)
  }

  async #run(): Promise<void> {
    const seq = ++this.#seq
    this.phase = 'running'
    this.errorMessage = null
    try {
      const res = await this.#restamp()
      if (seq !== this.#seq) return
      this.restamped = res
      const stats = await this.#fetchStats()
      if (seq !== this.#seq) return
      this.stats = stats
      this.phase = 'done'
    } catch (err) {
      if (seq !== this.#seq) return
      this.phase = 'error'
      this.errorMessage = toApiError(err).message
    }
  }
}
