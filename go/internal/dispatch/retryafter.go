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
// snapshot data already maintained (queue depth + the MW7 wait window); it
// takes the same mutex as Acquire and does O(K) sort work over the ≤ 512
// window only on this cold path, never in admission.
func (d *Dispatcher) RetryAfterHint(origin string, class Class) time.Duration {
	norm, err := NormalizeOrigin(origin)
	if err != nil {
		norm = origin // defensive: fall back to the raw key
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.targets[norm]
	if st == nil {
		return 0
	}
	depth := st.interactive.total
	if class == ClassBackground {
		depth = st.background.len()
	}
	var ws WaitStats
	st.waits[classIdx(class)].statsInto(&ws)
	return retryAfterEstimate(depth, ws.P95Wait)
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
