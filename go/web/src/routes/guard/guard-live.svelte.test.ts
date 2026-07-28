// GuardLive gates (RC-1 wave S6, design/05 §4.5 probes a–e).
//
// RED against Ist: before this wave GuardReviewPage had exactly TWO load
// triggers — the statusFilter $effect (:46-49) and the post-resolve reload
// (:88). No third path existed, so an externally resolved block stayed on the
// page until someone pressed Reload. Probe (a) is that gap expressed as an
// assertion: a changed counter must produce a reload within ONE poll window.
//
// The load-bearing one is (b): the compare must run on the READ-SCOPE VECTOR.
// A variant that watches only the home-scope four-tuple — the shape a naive
// reading of `guard_review` produces — sees NOTHING when a block in `shared` or
// `work` is resolved, although the list changed. `homeOnlyVector` below IS that
// variant, driven through the same data, and the test asserts it stays blind.
//
// The timer is MANUAL: no wall-clock, every tick explicit.

import { describe, expect, it } from 'vitest'
import type { GuardReviewStatus, StatusResponse } from '../../lib/api/types'
import {
  GuardLive,
  GUARD_POLL_MS,
  GUARD_SECTION_MAX_AGE_MS,
  guardSection,
  guardVector,
  type GuardTimer,
} from './guard-live.svelte'

const T0 = '2026-07-28T12:00:00Z'
const T1 = '2026-07-28T12:00:10Z'
const T2 = '2026-07-28T12:00:20Z'

/** A manual setInterval seam: holds the one scheduled callback; `tick()` runs it
 *  and flushes the async poll round's microtasks. */
function manualTimer() {
  let scheduled: (() => void) | null = null
  const timer: GuardTimer = {
    set(cb) {
      scheduled = cb
      return 1
    },
    clear() {
      scheduled = null
    },
  }
  return {
    timer,
    get armed() {
      return scheduled !== null
    },
    async tick(): Promise<void> {
      scheduled?.()
      await settle()
    },
  }
}

/** Flush the microtasks of one async poll round (start()'s immediate baseline
 *  poll is NOT a timer tick — it fires straight away). */
async function settle(): Promise<void> {
  await Promise.resolve()
  await Promise.resolve()
  await Promise.resolve()
}

function counts(over: Partial<GuardReviewStatus> = {}): GuardReviewStatus {
  return {
    needs_review: 2,
    near_duplicate: 0,
    possible_duplicate: 0,
    oldest_updated_at: '2026-07-20T08:00:00Z',
    built_at: T0,
    ...over,
  }
}

/** A status answer carrying home + one non-home read scope. */
function status(over: {
  asOf?: string
  home?: GuardReviewStatus
  shared?: GuardReviewStatus
  builtAt?: string
  omitByScope?: boolean
  omitGuard?: boolean
} = {}): StatusResponse {
  const asOf = over.asOf ?? T0
  const builtAt = over.builtAt ?? asOf
  const home = over.home ?? counts({ built_at: builtAt })
  const shared = over.shared ?? counts({ needs_review: 1, near_duplicate: 3, built_at: builtAt })
  const base: StatusResponse = {
    success: true,
    as_of: asOf,
    health: { status: 'ok', services: {} },
    backends: [],
    dream: {
      mode: 'on', throttle_interval_s: 0, pickable_now: 0, in_cooldown: 0, never_dreamed: 0,
      awaiting_embed: 0, incoming_1h: 0, incoming_6h: 0, next_pending_at: null, last_cycle_at: null,
    },
    llm_24h: [],
    llm_24h_complete: true,
    activity: null,
  }
  if (!over.omitGuard) base.guard_review = home
  if (!over.omitByScope) base.guard_review_by_scope = { privat: home, shared }
  return base
}

/** The Revision-1 variant this wave replaces: compare the HOME four-tuple only.
 *  Kept in the test file, not in the module — it exists to be proven blind. */
function homeOnlyVector(s: StatusResponse): string {
  const g = s.guard_review
  if (!g) return ''
  return `${g.needs_review}/${g.near_duplicate}/${g.possible_duplicate}/${g.oldest_updated_at ?? '-'}`
}

/** Drive a GuardLive over a scripted sequence of answers, counting reloads. */
function harness(answers: StatusResponse[], opts: { maxSectionAgeMs?: number } = {}) {
  const t = manualTimer()
  let i = 0
  let reloads = 0
  const live = new GuardLive({
    onChanged: () => {
      reloads++
    },
    fetch: () => Promise.resolve(answers[Math.min(i++, answers.length - 1)]),
    timer: t.timer,
    maxSectionAgeMs: opts.maxSectionAgeMs,
  })
  return {
    live,
    t,
    get reloads() {
      return reloads
    },
  }
}

describe('guardVector', () => {
  it('is stable under key order and moves on any element of any scope', () => {
    const a = status()
    const reordered = status()
    reordered.guard_review_by_scope = {
      shared: a.guard_review_by_scope!.shared,
      privat: a.guard_review_by_scope!.privat,
    }
    expect(guardVector(reordered)).toBe(guardVector(a))

    // Each of the four fields, in the NON-HOME scope, must move the vector.
    for (const patch of [
      { needs_review: 9 },
      { near_duplicate: 9 },
      { possible_duplicate: 9 },
      { oldest_updated_at: '2026-07-21T08:00:00Z' },
    ] as Partial<GuardReviewStatus>[]) {
      const moved = status({ shared: counts({ needs_review: 1, near_duplicate: 3, built_at: T0, ...patch }) })
      expect(guardVector(moved), JSON.stringify(patch)).not.toBe(guardVector(a))
    }
  })

  it('built_at does NOT move the vector (it advances every generation)', () => {
    const a = status({ asOf: T0, builtAt: T0 })
    const b = status({ asOf: T1, builtAt: T1 })
    expect(guardVector(b)).toBe(guardVector(a))
  })

  it('degrades to a MARKED home-only string when the server carries no vector', () => {
    const v = guardVector(status({ omitByScope: true }))
    expect(v).toContain('home-only')
    expect(v).not.toBe(guardVector(status()))
  })
})

describe('S6(a) refetch — an external guard-resolve reloads within one poll window', () => {
  it('reloads on the FIRST tick after the counters moved', async () => {
    // Two answers: the baseline, then the world after someone else resolved.
    const h = harness([status(), status({ home: counts({ needs_review: 1, built_at: T0 }) })])
    h.live.start()
    expect(h.t.armed).toBe(true)

    await settle() // start()'s immediate baseline poll — no reload
    expect(h.reloads).toBe(0)

    await h.t.tick() // ONE window later the external resolve is visible
    expect(h.reloads).toBe(1)
  })

  it('polls on GUARD_POLL_MS and stops dead on stop()', async () => {
    const t = manualTimer()
    let ms = 0
    const seam: GuardTimer = {
      set(cb, interval) {
        ms = interval
        return t.timer.set(cb, interval)
      },
      clear: (h) => t.timer.clear(h),
    }
    let polls = 0
    const live = new GuardLive({
      onChanged: () => {},
      fetch: () => {
        polls++
        return Promise.resolve(status())
      },
      timer: seam,
    })
    live.start()
    expect(ms).toBe(GUARD_POLL_MS)
    await settle() // the immediate baseline poll
    expect(polls).toBe(1)
    await t.tick()
    expect(polls).toBe(2)
    live.stop()
    expect(t.armed).toBe(false)
    await t.tick()
    expect(polls).toBe(2)
  })

  it('a hidden tab does not poll', async () => {
    let polls = 0
    const t = manualTimer()
    const live = new GuardLive({
      onChanged: () => {},
      fetch: () => {
        polls++
        return Promise.resolve(status())
      },
      timer: t.timer,
      isVisible: () => false,
    })
    live.start()
    await t.tick()
    expect(polls).toBe(0)
  })
})

describe('S6(b) non-home scope — a decision outside the home scope still reloads', () => {
  it('reloads when ONLY a non-home read scope moved', async () => {
    const before = status()
    // Home four-tuple identical; `shared` lost one needs_review. This is the
    // live case: 24 of 4231 blocks sit in shared/work.
    const after = status({ shared: counts({ needs_review: 0, near_duplicate: 3, built_at: T0 }) })
    expect(homeOnlyVector(after)).toBe(homeOnlyVector(before))

    const h = harness([before, after])
    h.live.start()
    await settle() // baseline
    await h.t.tick()
    expect(h.reloads).toBe(1)
  })

  it('the home-only variant is BLIND to exactly that change (the red baseline)', () => {
    const before = status()
    const after = status({ shared: counts({ needs_review: 0, near_duplicate: 3, built_at: T0 }) })
    // The variant sees nothing…
    expect(homeOnlyVector(after)).toBe(homeOnlyVector(before))
    // …while the shipped comparison does.
    expect(guardVector(after)).not.toBe(guardVector(before))
  })

  it('a scope leaving the read set moves the vector too', async () => {
    const before = status()
    const after = status()
    delete after.guard_review_by_scope!.shared
    const h = harness([before, after])
    h.live.start()
    await settle() // baseline
    await h.t.tick()
    expect(h.reloads).toBe(1)
  })
})

describe('S6(c) no refetch — an unchanged vector never reloads', () => {
  it('ten identical windows produce ZERO reloads', async () => {
    const h = harness([status()])
    h.live.start()
    await settle() // baseline
    for (let i = 0; i < 10; i++) await h.t.tick()
    expect(h.reloads).toBe(0)
    // A variant WITHOUT the comparison would have reloaded once per window:
    expect(h.reloads).not.toBe(10)
  })

  it('a walking as_of/built_at alone produces ZERO reloads', async () => {
    const h = harness([
      status({ asOf: T0, builtAt: T0 }),
      status({ asOf: T1, builtAt: T1 }),
      status({ asOf: T2, builtAt: T2 }),
    ])
    h.live.start()
    await settle() // baseline
    await h.t.tick()
    await h.t.tick()
    expect(h.reloads).toBe(0)
  })

  it('a failed poll keeps the held counts and says reconnecting', async () => {
    const t = manualTimer()
    let fail = false
    let reloads = 0
    const live = new GuardLive({
      onChanged: () => {
        reloads++
      },
      fetch: () => (fail ? Promise.reject(new Error('offline')) : Promise.resolve(status())),
      timer: t.timer,
    })
    live.start()
    await settle() // baseline
    expect(live.status).toBe('open')
    fail = true
    await t.tick()
    expect(live.status).toBe('error')
    expect(live.section.counts?.needs_review).toBe(2) // held, not blanked
    expect(reloads).toBe(0)
  })
})

describe('S6(d) order guard — a late answer cannot undo a resolve', () => {
  it('an answer with an OLDER as_of is discarded after a mutation', async () => {
    const fresh = status({ asOf: T2, builtAt: T2, home: counts({ needs_review: 1, built_at: T2 }) })
    const late = status({ asOf: T1, builtAt: T1, home: counts({ needs_review: 2, built_at: T1 }) })
    const h = harness([fresh, late])
    h.live.start()

    await settle() // fresh: floor rises to T2, one open block left
    expect(h.live.section.counts?.needs_review).toBe(1)
    expect(h.live.floorMs).toBeGreaterThan(0)

    h.live.markMutation()
    await h.t.tick() // the late, pre-resolve answer
    // The resolved row is NOT resurrected and no reload was triggered by it.
    expect(h.live.section.counts?.needs_review).toBe(1)
    expect(h.reloads).toBe(0)
  })

  it('an answer already IN FLIGHT when the operator resolved is dropped', async () => {
    const t = manualTimer()
    let release: ((s: StatusResponse) => void) | null = null
    let reloads = 0
    const live = new GuardLive({
      onChanged: () => {
        reloads++
      },
      fetch: () => new Promise<StatusResponse>((res) => (release = res)),
      timer: t.timer,
    })
    live.start()
    void live.poll() // request leaves…
    live.markMutation() // …operator resolves while it is in the air…
    release!(status({ asOf: T2, home: counts({ needs_review: 9, built_at: T2 }) }))
    await Promise.resolve()
    await Promise.resolve()
    // …and its answer never lands: neither section nor baseline moved.
    expect(live.section.counts).toBeNull()
    expect(reloads).toBe(0)
  })

  it('the mutation absorbs the next vector instead of reloading twice', async () => {
    // resolve() reloads the list itself; a second reload one window later would
    // throw away the operator's expansion/selection for nothing.
    const h = harness([status(), status({ home: counts({ needs_review: 1, built_at: T0 }) })])
    h.live.start()
    await settle() // baseline
    h.live.markMutation()
    await h.t.tick()
    expect(h.reloads).toBe(0)
  })
})

describe('S6(e) degradation — missing, stale and genuinely-empty are three states', () => {
  it('MISSING section renders no counts at all', () => {
    const s = guardSection(status({ omitGuard: true }))
    expect(s.health).toBe('missing')
    expect(s.counts).toBeNull()
    expect(s.ageMs).toBeNull()
  })

  it('STALE section (built_at older than the budget) renders no counts, but an age', () => {
    const old = new Date(Date.parse(T2) - (GUARD_SECTION_MAX_AGE_MS + 60_000)).toISOString()
    const s = guardSection(status({ asOf: T2, builtAt: old }))
    expect(s.health).toBe('stale')
    expect(s.counts).toBeNull()
    expect(s.ageMs).toBeGreaterThan(GUARD_SECTION_MAX_AGE_MS)
  })

  it('FRESH-EMPTY section renders a real 0', () => {
    const s = guardSection(
      status({ asOf: T0, builtAt: T0, home: counts({ needs_review: 0, oldest_updated_at: null, built_at: T0 }) }),
    )
    expect(s.health).toBe('ok')
    expect(s.counts?.needs_review).toBe(0)
  })

  it('a section without a built_at stamp counts as current (pre-S1 shape)', () => {
    const s = guardSection(status({ home: counts({ built_at: undefined }) }))
    expect(s.health).toBe('ok')
    expect(s.ageMs).toBeNull()
  })

  it('ConnState reads stale + partial while degraded, live otherwise', async () => {
    const old = new Date(Date.parse(T2) - (GUARD_SECTION_MAX_AGE_MS + 60_000)).toISOString()
    const h = harness([status({ asOf: T2, builtAt: old }), status({ asOf: T2, builtAt: T2 })])
    h.live.start()
    await settle() // baseline: the degraded answer
    expect(h.live.status).toBe('stale')
    expect(h.live.partial).toBe(true)
    await h.t.tick()
    expect(h.live.status).toBe('open')
    expect(h.live.partial).toBe(false)
  })
})
