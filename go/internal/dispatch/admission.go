package dispatch

import "errors"

// Admission binds the ONE process-wide dispatch admitter (I-D1) to a call
// site's class (Vorhaben E wave MW3, design/01 §4.6 N1): the CLASS is bound
// by the caller — the query pipeline binds interactive, scheduler arms bind
// background — while target and deadline hint are bound per attempt inside
// the wire walk. The PRINCIPAL is deliberately NOT a field (MW4, design/03
// §4.1.1): the dispatcher derives it exclusively from the Acquire ctx via
// the boot-installed hook, so a stored AuthResult held in a variable cannot
// buy interactive — an interactive class on a ctx without an authenticated
// principal runs into the B8 downgrade. Admission travels as a mandatory
// positional parameter (pattern: the ReportFunc parameter; the wire packages
// stay parameter-pure): a call site without an admitter does not compile,
// and a zero Admission fails the acquire loudly instead of passing an
// unadmitted wire call.
//
// The two wire families name this shape through their own defined types
// (llm.Admission for the chat chain walk, embedcache.Admission for the embed
// sequence) because each declares its own Acquire on it — the chat walk
// carries an admission-anchored deadline hint, the embed walk deliberately
// carries none (design/01 §4.4), and each reports its own package in the
// nil-admitter error. Go allows methods only in the package that defines the
// type, so the two Acquire bodies stay there; the STRUCT lives here once and
// cannot drift.
type Admission struct {
	Admitter Admitter
	Class    Class
}

// AdmissionError marks a wire walk ended by a failed Acquire. Acquire-error
// doctrine (design/01 §4.3, binding): a failed acquire is NOT an attempt —
// no attempt entry, no Classify, no health report, and the walk ends
// TERMINALLY (no failover spill onto a following chain link, e.g. openrouter
// under local saturation). The ONE deliberate exception is the K9 rejection
// TELEMETRY line (MW10, design/05 §3.2): llm.RecordRejection persists a
// never-admitted background acquire_expired/queue_full — everything else
// about the doctrine stays. Unwrap keeps errors.Is against the dispatch
// sentinels (IsRejection) and ctx errors intact.
//
// ONE type for both wire families (MW11): the K9 rejection line has one
// shape regardless of which pipeline family was rejected, so its extraction
// needs one errors.As branch, not one per family.
type AdmissionError struct {
	Err error
	// Backend/Host/WaitMs are the K9 rejection telemetry (MW10): the
	// target of the failed acquire and the futile wait before rejection.
	// Zero-valued for the nil-admitter error (nothing waited anywhere).
	Backend string
	Host    string
	WaitMs  int64
}

func (e *AdmissionError) Error() string { return e.Err.Error() }
func (e *AdmissionError) Unwrap() error { return e.Err }

// IsAdmissionError reports whether err is a wire walk ended by a failed
// acquire (no wire contact of its own) — the ONE check point for the
// telemetry sites honoring the no-llmlog-line doctrine.
func IsAdmissionError(err error) bool {
	var ae *AdmissionError
	return errors.As(err, &ae)
}
