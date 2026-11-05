// Package v1census is the admission census of the model groups /v1 and /v2
// (wave A03-W8, design/03 §5 X1, design/02 §4.11.4): a behavioural census over
// every route a mount registers under the model prefixes — found with
// chi.Walk — driven one request at a time through a counting dispatcher
// (Recorder) that the mount's planner (handler.V1Admission) reads. Per
// request it proves:
//
//  1. every acquire is in an external class and in the planner's
//     AllowedClasses (X1: never a /v1 lease in ClassInteractive or
//     ClassBackground);
//  2. a continuation acquire (Continue) and a try (Next) come only after a
//     blocking first acquire (First) of the same request — one wait point
//     per request (L5);
//  3. a request that acquired read ExternalAdmissible (the chain filter) and
//     ExternalWeightCap (the weight clamp) at least once — deadcode sees both
//     as reflection-reachable, so their wiring is proven here, not by the
//     ratchet (A03-W3 deviation 5, A03-W7 deviation 5);
//  4. every origin argument — of the doors, of ExternalAdmissible, of
//     ExternalWeightCap and of RetryAfterHint — is byte for byte the b.Host
//     of a chain row, never a spelling built from request data (A03-W17
//     review 2 F-5: each raw spelling grows the dispatcher's spellings
//     cache);
//  5. RetryAfterHint is never read for an external class (the Retry-After of
//     an external rejection travels in its RejectionError; the class-wide
//     hint leaks the class depth, A03-W6);
//  6. a route marked admitting acquires at least once, every other route not
//     at all (GET /v1/models: 0 acquires);
//  7. no continuation while the request still holds a lease of its own —
//     the block lease goes back before the next block asks: a continuation
//     waits at the class head for units, and units its own request holds
//     never come (a self-deadlock on a full E; with units left, the head
//     held against every other consumer; A03-W8 review 2 F-2). Held =
//     the run context of a lease the request got is not yet done
//     (Lease.Release cancels it).
//
// The mount under test hands its planner the Recorder as dispatcher. The
// census holds no database and no key of its own: Prepare puts what the
// mount's door needs on each request. The package is a test helper, never
// linked into ctxd; the fixture census of A03-W8 and the route waves
// (W02-05/07/08/09/10) run it.
package v1census

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/handler"
)

// Kind names one call the Recorder saw.
type Kind string

// The calls a planner can make on its dispatcher.
const (
	KindAcquire    Kind = "acquire"     // blocking door (First, Continue)
	KindTry        Kind = "try"         // non-blocking door (Next)
	KindAdmissible Kind = "admissible"  // ExternalAdmissible
	KindWeightCap  Kind = "weight_cap"  // ExternalWeightCap
	KindHint       Kind = "retry_hint"  // RetryAfterHint
	KindSteps      Kind = "retry_steps" // ExternalRetryAfterSteps
)

// Event is one recorded call.
type Event struct {
	Kind         Kind
	Origin       string
	Class        dispatch.Class
	Continuation bool
	Weight       int
	// Holding is, for a continuation, the number of leases the recorder
	// handed out since the last Take whose run context was not yet done
	// when the continuation asked (rule 7).
	Holding int
}

// Recorder is the counting dispatcher of the census: every call is recorded
// in order and passed to Inner. It serves one request at a time (Check
// drives them sequentially): the leases it tracks for rule 7 are those
// granted since the last Take.
type Recorder struct {
	Inner handler.V1Dispatch

	mu      sync.Mutex
	events  []Event
	granted []context.Context // run contexts of the leases since the last Take
}

var (
	_ handler.V1Dispatch   = (*Recorder)(nil)
	_ dispatch.RetryHinter = (*Recorder)(nil)
)

// NewRecorder wraps inner.
func NewRecorder(inner handler.V1Dispatch) *Recorder { return &Recorder{Inner: inner} }

func (r *Recorder) add(e Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

// Take returns the events since the last Take and clears them (and the
// tracked leases).
func (r *Recorder) Take() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events, r.granted = nil, nil
	return out
}

// holding counts the tracked leases whose run context is not done.
func (r *Recorder) holding() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.granted {
		if c.Err() == nil {
			n++
		}
	}
	return n
}

// grant tracks the run context of a lease handed out.
func (r *Recorder) grant(runCtx context.Context, err error) {
	if err != nil || runCtx == nil {
		return
	}
	r.mu.Lock()
	r.granted = append(r.granted, runCtx)
	r.mu.Unlock()
}

// Acquire implements dispatch.Admitter.
func (r *Recorder) Acquire(ctx context.Context, req dispatch.Request) (*dispatch.Lease, context.Context, error) {
	e := Event{Kind: KindAcquire, Origin: req.Target.Origin, Class: req.Class, Continuation: req.Continuation, Weight: req.Weight}
	if req.Continuation {
		e.Holding = r.holding()
	}
	r.add(e)
	l, runCtx, err := r.Inner.Acquire(ctx, req)
	r.grant(runCtx, err)
	return l, runCtx, err
}

// TryAcquire implements dispatch.TryAdmitter.
func (r *Recorder) TryAcquire(ctx context.Context, req dispatch.Request) (*dispatch.Lease, context.Context, error) {
	r.add(Event{Kind: KindTry, Origin: req.Target.Origin, Class: req.Class, Continuation: req.Continuation, Weight: req.Weight})
	l, runCtx, err := r.Inner.TryAcquire(ctx, req)
	r.grant(runCtx, err)
	return l, runCtx, err
}

// ExternalAdmissible records the chain-filter read.
func (r *Recorder) ExternalAdmissible(origin string) bool {
	r.add(Event{Kind: KindAdmissible, Origin: origin})
	return r.Inner.ExternalAdmissible(origin)
}

// ExternalWeightCap records the clamp read.
func (r *Recorder) ExternalWeightCap(origin string) int {
	r.add(Event{Kind: KindWeightCap, Origin: origin})
	return r.Inner.ExternalWeightCap(origin)
}

// ExternalRetryAfterSteps records the step read.
func (r *Recorder) ExternalRetryAfterSteps() []time.Duration {
	r.add(Event{Kind: KindSteps})
	return r.Inner.ExternalRetryAfterSteps()
}

// RetryAfterHint records the hint read (rule 5) and answers what Inner
// answers, 0 when Inner has no estimator.
func (r *Recorder) RetryAfterHint(origin string, class dispatch.Class) time.Duration {
	r.add(Event{Kind: KindHint, Origin: origin, Class: class})
	if h, ok := r.Inner.(dispatch.RetryHinter); ok {
		return h.RetryAfterHint(origin, class)
	}
	return 0
}

// Options configure one census.
type Options struct {
	// Prefixes the census walks; each must carry at least one route.
	// Default {"/v1"}.
	Prefixes []string
	// Allowed is the planner's AllowedClasses().
	Allowed []dispatch.Class
	// Hosts are the b.Host spellings of every chain row the mount's planner
	// can see.
	Hosts []string
	// Admitting lists the routes ("METHOD /prefix/pattern" as chi.Walk names
	// them) that must acquire; every other route must not.
	Admitting []string
	// Prepare readies one census request for the mount's door and
	// handlers (credential, context); nil leaves it as built.
	Prepare func(r *http.Request) *http.Request
	// Body returns the body of a census request; nil sends "{}".
	Body func(method, route string) string
}

// Assert runs the census and fails t with every violation.
func Assert(t testing.TB, router chi.Router, rec *Recorder, opts Options) {
	t.Helper()
	for _, v := range Check(router, rec, opts) {
		t.Error(v)
	}
}

// route is one registered route under a census prefix.
type route struct{ method, pattern string }

var paramSegment = regexp.MustCompile(`\{[^}]*\}`)

// concrete fills chi's parameters and wildcard with a fixed segment.
func concrete(pattern string) string {
	p := paramSegment.ReplaceAllString(pattern, "census")
	return strings.ReplaceAll(p, "*", "census")
}

func underPrefix(pattern string, prefixes []string) bool {
	for _, p := range prefixes {
		if pattern == p || strings.HasPrefix(pattern, p+"/") {
			return true
		}
	}
	return false
}

// Check runs one census request per route under the prefixes and returns
// the violations (empty = the census holds).
func Check(router chi.Router, rec *Recorder, opts Options) []string {
	prefixes := opts.Prefixes
	if len(prefixes) == 0 {
		prefixes = []string{"/v1"}
	}
	var routes []route
	_ = chi.Walk(router, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if underPrefix(pattern, prefixes) {
			routes = append(routes, route{method, pattern})
		}
		return nil
	})
	var out []string
	for _, p := range prefixes {
		if !slices.ContainsFunc(routes, func(rt route) bool { return underPrefix(rt.pattern, []string{p}) }) {
			out = append(out, fmt.Sprintf("prefix %s carries no route — a census over nothing proves nothing", p))
		}
	}
	for _, a := range opts.Admitting {
		if !slices.ContainsFunc(routes, func(rt route) bool { return rt.method+" "+rt.pattern == a }) {
			out = append(out, fmt.Sprintf("admitting route %q is not registered", a))
		}
	}
	rec.Take()
	for _, rt := range routes {
		name := rt.method + " " + rt.pattern
		body := "{}"
		if opts.Body != nil {
			body = opts.Body(rt.method, rt.pattern)
		}
		req := httptest.NewRequest(rt.method, concrete(rt.pattern), strings.NewReader(body))
		if opts.Prepare != nil {
			req = opts.Prepare(req)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		_, _ = io.Copy(io.Discard, w.Result().Body)
		for _, v := range checkRequest(rec.Take(), slices.Contains(opts.Admitting, name), opts) {
			out = append(out, name+": "+v)
		}
	}
	return out
}

// checkRequest applies rules 1–7 to the events of one request.
func checkRequest(evs []Event, admitting bool, opts Options) []string {
	var out []string
	var acquires, admissible, caps int
	first := false
	for _, e := range evs {
		if e.Origin != "" && !slices.Contains(opts.Hosts, e.Origin) {
			out = append(out, fmt.Sprintf("%s with origin %q — not the b.Host of a chain row (request-derived spelling?)", e.Kind, e.Origin))
		}
		switch e.Kind {
		case KindAcquire, KindTry:
			acquires++
			if !e.Class.External() || !slices.Contains(opts.Allowed, e.Class) {
				out = append(out, fmt.Sprintf("%s in class %s — outside the external classes %v", e.Kind, e.Class, opts.Allowed))
			}
			switch {
			case e.Kind == KindAcquire && !e.Continuation:
				if first {
					out = append(out, "a second blocking first acquire — a second wait point in one request")
				}
				first = true
			case !first:
				out = append(out, fmt.Sprintf("%s (continuation %v) before the first acquire of the request", e.Kind, e.Continuation))
			}
			if e.Continuation && e.Holding > 0 {
				out = append(out, fmt.Sprintf("continuation while the request holds %d lease(s) of its own — it waits for its own units (release the block lease first)", e.Holding))
			}
		case KindAdmissible:
			admissible++
		case KindWeightCap:
			caps++
		case KindHint:
			if e.Class.External() {
				out = append(out, "RetryAfterHint read for an external class — the Retry-After travels in the RejectionError")
			}
		}
	}
	switch {
	case admitting && acquires == 0:
		out = append(out, "no acquire on an admitting route")
	case !admitting && acquires > 0:
		out = append(out, fmt.Sprintf("%d acquires on a route that must not admit", acquires))
	}
	if acquires > 0 && (admissible == 0 || caps == 0) {
		out = append(out, fmt.Sprintf("acquired without reading ExternalAdmissible (%d) and ExternalWeightCap (%d)", admissible, caps))
	}
	return out
}
