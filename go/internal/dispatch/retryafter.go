package dispatch

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

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
// Dispatcher.spellings. For the external class the estimate is the class
// formula quantized onto Settings.ExternalRetryAfterSteps (A03-W6) — the
// value for a caller WITHOUT an error value; an external rejection carries
// the Retry-After of its own position (RejectionError), which is what the
// /v1 mapping reads (A03-W8). Interactive and background keep B1 unchanged.
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
		return quantizeRetryAfter(classEstimate(st.external.total, st.ring(class).published().p95),
			d.currentSettings().ExternalRetryAfterSteps)
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

// classEstimate is the unclamped class formula depth × per-position wait of
// the external class (A03-W6): the B1 product without the 1/30-s clamp — the
// steps of the setting floor and cap it (quantizeRetryAfter). Saturates
// instead of overflowing.
func classEstimate(depth int, perPosition time.Duration) time.Duration {
	if depth <= 0 || perPosition <= 0 {
		return 0
	}
	if perPosition > maxDuration/time.Duration(depth) {
		return maxDuration
	}
	return time.Duration(depth) * perPosition
}

// maxDuration is the largest representable time.Duration (a saturation
// bound, not a policy value).
const maxDuration = time.Duration(1<<63 - 1)

// quantizeRetryAfter is Q of design/03 §4.9 (E-03-13): est rounded UP to the
// next step, the largest step when est lies above it — the largest step caps
// the external Retry-After, the smallest floors it ("retry now" is never
// advertised). steps is the resolved setting (strictly ascending, non-empty;
// resolveRetryAfterSteps), so the answer is always one of its steps.
func quantizeRetryAfter(est time.Duration, steps []time.Duration) time.Duration {
	for _, s := range steps {
		if est <= s {
			return s
		}
	}
	return steps[len(steps)-1]
}

// RejectionError is the error value of an external rejection (A03-W6,
// design/03 §4.9): the Retry-After travels WITH the rejection, so the /v1
// mapping (A03-W8) needs neither a second mutex section nor a class-depth
// reader. It wraps the rejection sentinel — errors.Is and IsRejection see the
// sentinel unchanged, the server-side text is the sentinel's. The in-house
// classes keep the bare sentinels and RetryAfterHint (B1).
type RejectionError struct {
	// Err is the rejection sentinel: ErrWaitExpired (reasons predicted and
	// expired) or a cap-ladder sentinel (ErrPrincipalSaturated,
	// ErrTenantSaturated, ErrTargetSaturated).
	Err error
	// RetryAfter is Q(ŵ) of the rejected acquire's OWN position (identity
	// weight queued ahead, waiting tenants and consumers, E_eff, the
	// remaining time of the oldest held external lease, lease p50), or on a
	// cold lease window the class formula of the external class, quantized
	// onto Settings.ExternalRetryAfterSteps — always one of the steps; for an
	// expired waiter never below the time it waited. Without jitter: the
	// header carries the jitter (A03-W8, gateway.retry_after_jitter).
	RetryAfter time.Duration
}

func (e *RejectionError) Error() string { return e.Err.Error() }

// Unwrap exposes the sentinel to errors.Is (IsRejection).
func (e *RejectionError) Unwrap() error { return e.Err }

// errRetryAfterSteps is the cause of every malformed step list.
var errRetryAfterSteps = errors.New("dispatch: retry-after steps must be comma-separated whole seconds >= 1, strictly ascending, at least one")

// ParseRetryAfterSteps parses dispatch.external_retry_after_steps (A03-W6):
// comma-separated whole seconds, blanks around an entry ignored, at least one
// entry, every entry 1..maxLimitCount (a representation bound), strictly
// ascending. The one grammar of the setting validator (config V46) and the
// owner mapping (events.DispatchSettings).
func ParseRetryAfterSteps(raw string) ([]time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("%w: empty", errRetryAfterSteps)
	}
	parts := strings.Split(raw, ",")
	steps := make([]time.Duration, 0, len(parts))
	for _, part := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 1 || n > maxLimitCount {
			return nil, fmt.Errorf("%w: entry %q", errRetryAfterSteps, part)
		}
		step := time.Duration(n) * time.Second
		if len(steps) > 0 && step <= steps[len(steps)-1] {
			return nil, fmt.Errorf("%w: %q not above its predecessor", errRetryAfterSteps, part)
		}
		steps = append(steps, step)
	}
	return steps, nil
}

// validRetryAfterSteps reports whether steps is a usable step list: at least
// one step, every step whole seconds ≥ 1 s — the grammar of the setting, the
// Settings doc (review 2 F-7) — strictly ascending.
func validRetryAfterSteps(steps []time.Duration) bool {
	for i, s := range steps {
		if s < time.Second || s%time.Second != 0 || (i > 0 && s <= steps[i-1]) {
			return false
		}
	}
	return len(steps) > 0
}

// resolveRetryAfterSteps resolves Settings.ExternalRetryAfterSteps once per
// settings swap (New, UpdateSettings), the resolveRankOrder pattern: nil
// reads as the default steps silently (the zero value), an invalid list —
// only reachable past the config validation — reads as the default with one
// WARN; a valid list is copied, so no caller-owned backing array is shared.
func (d *Dispatcher) resolveRetryAfterSteps(steps []time.Duration) []time.Duration {
	switch {
	case steps == nil:
		return DefaultSettings().ExternalRetryAfterSteps
	case !validRetryAfterSteps(steps):
		d.logger.Warn("dispatch: invalid external retry-after steps — the default steps are in effect",
			"steps", steps)
		return DefaultSettings().ExternalRetryAfterSteps
	}
	return append([]time.Duration(nil), steps...)
}
