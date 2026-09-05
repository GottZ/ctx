package handler

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/GottZ/ctx/internal/dispatch"
)

// Rejection → 429 mapping helpers (MW8, D3-W4, DECISIONS amendment B1).
//
// A dispatcher rejection (IsRejection: the Deckel-Staffel sentinels + queue
// full) is a CAPACITY signal, not a fault: mapping it to 500 poisons client
// retry logic (500 = not retryable) and fault alerting (design/03 §4.5.2 / C1).
// It maps to a generic 429 with the B6-generic body (§3.3) — no target, no
// queue depth, no principal — plus a B1 Retry-After header when the admitter
// can estimate one. The header MUST be set before the response header is
// committed (the query synthesis path commits a 200 up front for the
// heartbeat, so it can only carry the body there — the same constraint the
// existing quota-exceeded 429 lives with).

// rejectionBody is the B6-generic 429 body (design/03 §3.3, vorbild
// ErrNoEligibleBackend sanitisation pool.go): success:false plus a fixed text
// that reveals neither WHICH target saturated nor WHICH Deckel-Staffel cap
// fired.
func rejectionBody() map[string]any {
	return map[string]any{"success": false, "error": "server busy — retry shortly"}
}

// admissionHost extracts the rejected target origin from a chain-walk
// AdmissionError — the synthesis/rerank path and the embed path alike. ONE
// branch, because llm.AdmissionError and embedcache.AdmissionError are type
// ALIASES of dispatch.AdmissionError (llm/chain.go, embedcache/embedcache.go)
// and not distinct types: MW11's "one type for both wire families" is what
// makes a second branch unreachable rather than merely redundant.
// "" when the error carries no host ⇒ the caller omits the header rather than
// guessing.
func admissionHost(err error) string {
	var ae *dispatch.AdmissionError
	if errors.As(err, &ae) {
		return ae.Host
	}
	return ""
}

// setRejectRetryAfter sets the Retry-After header for an interactive
// dispatcher rejection using the B1 snapshot estimate, IF the admitter exposes
// the estimator (dispatch.RetryHinter). A test fake admitter or an unknown
// origin ⇒ no header: an honest absence beats a fabricated constant (design/03
// §3.3 rationale, overridden by B1 only where the snapshot source exists).
// No effect after the response header is committed (net/http drops late header
// writes) — call it before writing the status.
func setRejectRetryAfter(w http.ResponseWriter, adm dispatch.Admitter, err error) {
	hinter, ok := adm.(dispatch.RetryHinter)
	if !ok {
		return
	}
	d := hinter.RetryAfterHint(admissionHost(err), dispatch.ClassInteractive)
	if secs := retryAfterSeconds(d); secs > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(secs))
	}
}

// retryAfterSeconds renders a duration as an RFC 7231 delta-seconds value
// (integer, ceil, min 1). Zero in ⇒ 0 out ⇒ header omitted.
func retryAfterSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	secs := int((d + time.Second - 1) / time.Second) // ceil
	if secs < 1 {
		secs = 1
	}
	return secs
}
