package dispatch

import "time"

// Retry-After estimate for interactive dispatcher rejections (MW8, D3-W4).
//
// Provenance note (binding): design/03 §3.3 and the D3-U1 recommendation both
// argued AGAINST a Retry-After header ("no honest estimate exists"). The user
// OVERRODE that on the decision board — DECISIONS amendment B1: answer 429 WITH
// Retry-After, sourced from an HONESTLY DECLARED estimator. This file is that
// estimator. The declared source is the MW7 rolling wait window per
// target × class (metrics.go): a recent admitted acquire of this class waited
// P95Wait at THIS target — an empirical realized wait, not an invented
// constant. The B1 model is "queue depth × sliding lease duration": the
// current queue depth is the peers ahead, P95Wait is the per-position
// clearance proxy. The product deliberately OVER-estimates (a p95 wait already
// spans multiple positions) — the conservative direction for a Retry-After,
// which should err toward "wait a bit longer" rather than train synchronized
// retry waves. It is CAPPED so a 900 s stream lease never becomes an absurd
// client hint, and FLOORED so a known-busy target never advertises "retry
// now". The error body stays B6-generic regardless (§3.3): the header carries a
// coarse time only — no target, no depth, no principal.
const (
	// retryAfterCap bounds the estimate. Leases can run to 900 s, but a
	// Retry-After past a client's plausible patience is worse than none.
	retryAfterCap = 30 * time.Second
	// retryAfterFloor is the minimum a known-busy target advertises.
	retryAfterFloor = 1 * time.Second
)

// RetryHinter is the OPTIONAL Admitter capability the HTTP rejection mapping
// type-asserts for the B1 Retry-After header. Kept separate from Admitter so
// the many test fake admitters need not implement it — an admitter without it
// simply yields no header (honest absence, never a fabricated value).
type RetryHinter interface {
	RetryAfterHint(origin string, class Class) time.Duration
}

// RetryAfterHint returns the B1 Retry-After estimate for a target × class, or
// 0 for an unknown origin (⇒ the handler omits the header — no fabricated
// value for a target we never admitted against). The value derives ONLY from
// data already maintained: the queue depth and the PUBLISHED p95 of the MW7
// wait window (A03-W17, metrics.go — at most waitPublishInterval old). Every
// 429 of an external request asks for it, so it is a hot path: one short
// measured mutex section, no sort, no window copy, no allocation — the origin
// spelling is parsed once (outside the mutex) and then resolved through
// Dispatcher.spellings.
func (d *Dispatcher) RetryAfterHint(origin string, class Class) time.Duration {
	d.mu.Lock()
	st, cached := d.spellings[origin]
	if !cached {
		// A spelling not seen yet: parse it OUTSIDE the mutex (a raw
		// base_url costs allocations, review F5), then resolve it; only a
		// spelling of an existing target is cached (targets are never
		// deleted, so the cache is bounded by the backend rows' hosts) —
		// an unknown origin parses again on its next hint.
		d.mu.Unlock()
		norm := canonicalOrigin(origin)
		d.mu.Lock()
		if st = d.targets[norm]; st != nil {
			d.spellings[origin] = st
		}
	}
	defer d.mu.Unlock()
	opStart := time.Now()
	defer d.opDone(opStart)
	if st == nil {
		return 0
	}
	var depth int
	switch class {
	case ClassInteractive:
		depth = st.interactive.total
	case ClassBackground:
		depth = st.background.len()
	case ClassExternal:
		// The external window includes the censored samples of expired
		// waiters (A03-W5, metrics.go) — the estimate does not read only
		// the survivors; the depth includes waiting continuations (A03-W17),
		// which are served before a newcomer.
		depth = st.external.total
	default:
		// Fail-closed (A03-W1): no estimate for a class without a case here
		// — 0 omits the header (pre-A03-W1: interactive depth × the
		// background window, K14).
		d.logger.Error("dispatch: class without a retry-after case — no estimate (fail-closed default)",
			"target", st.origin, "class", uint8(class))
		return 0
	}
	return retryAfterEstimate(depth, st.ring(class).published().p95)
}

// retryAfterEstimate is the pure B1 formula (queue depth × per-position
// clearance proxy, clamped) — split out so the "known snapshot ⇒ expected
// capped value" gate can pin it without staging a live queue.
func retryAfterEstimate(depth int, perPosition time.Duration) time.Duration {
	if depth < 0 {
		depth = 0
	}
	if perPosition < 0 {
		perPosition = 0
	}
	est := time.Duration(depth) * perPosition
	if est < retryAfterFloor {
		est = retryAfterFloor
	}
	if est > retryAfterCap {
		est = retryAfterCap
	}
	return est
}
