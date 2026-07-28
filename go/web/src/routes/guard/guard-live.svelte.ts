// GuardLive — the /guard page's live channel, Stufe 1 (design/05 §4.5, RC-1
// wave S6). A POLL of GET /api/status that turns a moved guard queue into ONE
// signal: reload().
//
// The Ist it replaces: GuardReviewPage loaded exactly twice — once per
// statusFilter change and once after a resolve. A guard decision made anywhere
// else (a second operator, the CLI, an agent) stayed invisible until someone
// pressed Reload.
//
// WHAT IS COMPARED — the scope VECTOR, not the home-scope counter. Counter and
// list run on two different scope predicates:
//
//   list    b.scope = ANY(ar.ReadScopes)   store.GuardList
//   counter scope = ar.HomeScope           status_guard.go guardReviewForScope
//
// so every decision on a block in a non-home read scope moves the list without
// moving the counter. Wave S6 added `guard_review_by_scope` (one four-tuple per
// READ scope, same per-tick generation, 0 extra queries) precisely so this
// client can compare the predicate the list actually runs on.
//
// Four fields per scope rather than a hash over the list: a keep+archive pair in
// one window can leave needs_review unchanged while the list changes —
// oldest_updated_at moves then. Cheaper and more honest than a list digest.
//
// STUFE 2 (S13) swaps the transport for SSE and keeps this poll as the permanent
// fallback (the E04-7 doctrine, lib/workflow/live.ts) — which is why `status`
// already speaks the SseStatus vocabulary ConnState renders.

import { fetchStatus } from '../../lib/api/status'
import type { GuardReviewStatus, StatusResponse } from '../../lib/api/types'
import type { SseStatus } from '../../lib/sse.svelte'
import { acceptAsOf, asOfMs } from '../status/status-store.svelte'

/**
 * Poll cadence (design/05 §4.5: "alle guard.poll_interval (Default 10 s)").
 *
 * A CLIENT constant, not a settings key: `internal/config` has 161 registry keys
 * across 27 groups and no `guard.*` group, and — decisive — not one of them
 * reaches the browser. Every FE cadence in this codebase is a local constant
 * (StatusPage POLL_MS 5000, corpus.svelte.ts POLL_MS 2000, workflow/live.ts
 * DEFAULT_POLL_MS 10_000). Registering `guard.poll_interval` server-side would
 * need a whole new config→client wire path, which is a bigger surface than this
 * wave; the constant is injectable (`pollMs`) so that path can land later
 * without touching a call site.
 */
export const GUARD_POLL_MS = 10_000

/**
 * How old the server's guard generation may be before this client renders '—'
 * instead of a number.
 *
 * Measured as `as_of − built_at` — BOTH server stamps, so it is immune to
 * browser clock skew (the sse.svelte.ts lesson: `Date.now() − serverStamp` fires
 * permanently on a tab running ahead of the server).
 *
 * The SERVER is the authority here: it already drops the whole section past
 * three of its own tick intervals (status_guard.go guardGenStaleFactor). This
 * threshold is the client's independent belt, derived from the only cadence the
 * client knows — its own. Three poll windows: a generation older than that is
 * old by any measure the page can apply.
 */
export const GUARD_SECTION_MAX_AGE_MS = 3 * GUARD_POLL_MS

/** setInterval seam — a real timer in production, a manual one under vitest so
 *  the poll is driven tick-by-tick with no wall-clock (the CorpusModel Timer
 *  pattern, routes/admin/corpus.svelte.ts). */
export interface GuardTimer {
  set(cb: () => void, ms: number): unknown
  clear(handle: unknown): void
}

const defaultTimer: GuardTimer = {
  set: (cb, ms) => setInterval(cb, ms),
  clear: (h) => clearInterval(h as ReturnType<typeof setInterval>),
}

function defaultVisible(): boolean {
  return typeof document === 'undefined' || document.visibilityState === 'visible'
}

/** Why the counters cannot be trusted as numbers right now. `ok` is the only
 *  state that renders digits; both others render '—'. */
export type GuardSectionHealth = 'ok' | 'missing' | 'stale'

/** What the page renders in its head: the caller's own (home-scope) four-tuple
 *  plus WHY it may not be a number. */
export interface GuardSection {
  /** null whenever health !== 'ok' — the page must not print a stale count. */
  counts: GuardReviewStatus | null
  health: GuardSectionHealth
  /** Generation age in ms (as_of − built_at), null when unknowable. Rendered
   *  next to the '—' so "no data" is distinguishable from "old data". */
  ageMs: number | null
}

const MISSING: GuardSection = { counts: null, health: 'missing', ageMs: null }

/**
 * Canonical serialization of the compare vector: one line per READ scope with
 * its four-tuple, scopes sorted so key order off the wire cannot fake a change.
 *
 * Falls back to the home-scope section alone when the server does not carry
 * `guard_review_by_scope` (an older binary). That fallback is DEGRADED by
 * definition — it is exactly the predicate divergence S6 exists to close — and
 * is marked as such in the string so it can never be mistaken for a full vector.
 */
export function guardVector(s: StatusResponse | null | undefined): string {
  if (!s) return ''
  const row = (g: GuardReviewStatus): string =>
    `${g.needs_review}/${g.near_duplicate}/${g.possible_duplicate}/${g.oldest_updated_at ?? '-'}`
  const byScope = s.guard_review_by_scope
  if (byScope) {
    return Object.keys(byScope)
      .sort()
      .map((scope) => `${scope}=${row(byScope[scope])}`)
      .join(';')
  }
  // No vector on the wire: compare what there is, and say so.
  return s.guard_review ? `home-only:${row(s.guard_review)}` : ''
}

/** Classify the caller's own section for rendering (B10: missing / stale / fresh
 *  are three different things, and only the third may print '0'). */
export function guardSection(s: StatusResponse | null | undefined, maxAgeMs = GUARD_SECTION_MAX_AGE_MS): GuardSection {
  const counts = s?.guard_review
  if (!s || !counts) return MISSING
  const built = asOfMs(counts.built_at)
  const asOf = asOfMs(s.as_of)
  // No stamp at all: pre-S1 shape. Nothing to age it against, so it counts as
  // current — the server drops what it knows to be stale before it ships.
  if (built === 0 || asOf === 0) return { counts, health: 'ok', ageMs: null }
  const ageMs = Math.max(0, asOf - built)
  if (ageMs > maxAgeMs) return { counts: null, health: 'stale', ageMs }
  return { counts, health: 'ok', ageMs }
}

export interface GuardLiveOptions {
  /** Fired when the compare vector MOVED — the page answers with reload(). */
  onChanged: () => void
  /** Status fetcher seam (default: GET /api/status). */
  fetch?: () => Promise<StatusResponse>
  timer?: GuardTimer
  pollMs?: number
  maxSectionAgeMs?: number
  /** Tab-visibility guard (default: document.visibilityState, always-visible in
   *  the node test env). A hidden tab costs the server nothing. */
  isVisible?: () => boolean
}

export class GuardLive {
  /** ConnState vocabulary (Pick<SseClient,'status'|'partial'>): idle before
   *  start, connecting until the first answer, live once a fresh section landed,
   *  stale on a missing/aged section, reconnecting on a failed poll. */
  status = $state<SseStatus>('idle')
  /** The server answered but its guard section did not — ConnState's 'partial'. */
  partial = $state(false)
  /** The caller's own section, classified for rendering. */
  section = $state<GuardSection>(MISSING)
  /** Highest as_of applied (ms). Every older answer is discarded — N14. */
  floorMs = $state(0)

  readonly #onChanged: () => void
  readonly #fetch: () => Promise<StatusResponse>
  readonly #timer: GuardTimer
  readonly #pollMs: number
  readonly #maxAgeMs: number
  readonly #isVisible: () => boolean

  #handle: unknown = null
  #started = false
  /** Last applied vector; null means "nothing to compare against yet", which is
   *  NOT a change (the first answer must never trigger a reload — the page has
   *  just loaded the list itself). */
  #vector: string | null = null
  /** Bumped by every mutation. A poll answer from a request issued BEFORE the
   *  mutation is dropped whatever its stamp — the in-flight half of the order
   *  guard, clock-free (StatusStore's #seq). */
  #mutation = 0

  constructor(opts: GuardLiveOptions) {
    this.#onChanged = opts.onChanged
    this.#fetch = opts.fetch ?? fetchStatus
    this.#timer = opts.timer ?? defaultTimer
    this.#pollMs = opts.pollMs ?? GUARD_POLL_MS
    this.#maxAgeMs = opts.maxSectionAgeMs ?? GUARD_SECTION_MAX_AGE_MS
    this.#isVisible = opts.isVisible ?? defaultVisible
  }

  /**
   * Arm the poll and take the baseline NOW. Idempotent.
   *
   * The immediate first poll is load-bearing, not cosmetic: the baseline vector
   * must be taken at the moment the page loads its list. Deferring it by one
   * window would absorb every change made INSIDE that window into the baseline
   * — silently, and on exactly the surface this wave exists to make non-silent.
   * It also gives the header real counts instead of '—' on first paint, at the
   * cost of one tick-CACHED /api/status read.
   */
  start(): void {
    if (this.#started) return
    this.#started = true
    this.status = 'connecting'
    this.#handle = this.#timer.set(() => void this.poll(), this.#pollMs)
    void this.poll()
  }

  /** Disarm. Idempotent — an unmount leaves no live timer and no further fetch. */
  stop(): void {
    if (!this.#started) return
    this.#started = false
    this.#timer.clear(this.#handle)
    this.#handle = null
    this.status = 'closed'
  }

  /**
   * Announce a local mutation (a resolve()) — the N14 floor half of the order
   * guard, applied to a mutation whose answer carries no as_of.
   *
   * N14 raises the floor to the MUTATION ANSWER's as_of; guard-resolve's
   * envelope is resolved/skipped accounting and has none. The floor therefore
   * stands at the newest server stamp already applied (poll() keeps it there),
   * and this call adds the two things that stamp cannot express:
   *
   *  - a sequence bump, so an answer to a request that was ALREADY IN THE AIR
   *    when the operator resolved is dropped whatever its stamp (StatusStore's
   *    #seq half). /api/status serves a tick-CACHED snapshot, so such an answer
   *    can legitimately be newer than the floor and still predate the mutation.
   *  - baseline invalidation: the caller reloads the list itself right after
   *    resolving, so the next poll's vector IS the new truth and must be adopted
   *    silently. Without this every resolve would be followed by a second,
   *    redundant reload that throws away the operator's expansion and selection.
   *    Nothing is missed by it — the caller's own reload already fetched the
   *    list that vector describes.
   */
  markMutation(): void {
    this.#mutation++
    this.#vector = null
  }

  /** One poll round. Public so a test (and, later, S13's fallback effect) can
   *  drive it without a timer. */
  async poll(): Promise<void> {
    if (!this.#isVisible()) return
    const mutation = this.#mutation
    try {
      const data = await this.#fetch()
      // Order guard, both halves (see markMutation).
      if (mutation !== this.#mutation) return
      if (!acceptAsOf(data.as_of, this.floorMs)) return
      this.floorMs = Math.max(this.floorMs, asOfMs(data.as_of))
      this.#applySection(data)
      const next = guardVector(data)
      const prev = this.#vector
      this.#vector = next
      if (prev !== null && prev !== next) this.#onChanged()
    } catch {
      // A failed poll is a TRANSPORT fault, not a data fault: the held counts
      // stay on screen and ConnState says 'reconnecting'. The next tick retries.
      this.status = 'error'
    }
  }

  #applySection(data: StatusResponse): void {
    const section = guardSection(data, this.#maxAgeMs)
    this.section = section
    this.partial = section.health !== 'ok'
    this.status = section.health === 'ok' ? 'open' : 'stale'
  }
}
