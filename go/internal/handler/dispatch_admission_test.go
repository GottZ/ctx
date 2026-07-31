package handler

// MW3 call-site probes on the handler side: classification bindings (query
// pipeline + daily API path — design/01 §4.6 N1/N8a), the E-U2 concurrency
// brake (Deckel = 1 per principal) and the N8b chat-probe admission.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
)

func testAuthResult() *auth.AuthResult {
	return &auth.AuthResult{
		IsValid:   true,
		ApiKeyID:  "key-1",
		TenantID:  "tenant-1",
		HomeScope: "scope-1",
	}
}

// handlerRecordingAdmitter delegates to a real pass-through dispatcher and
// records every request; an optional scripted error rejects instead.
type handlerRecordingAdmitter struct {
	d      *dispatch.Dispatcher
	mu     sync.Mutex
	reqs   []dispatch.Request
	reject error
}

func newHandlerRecordingAdmitter(t *testing.T) *handlerRecordingAdmitter {
	t.Helper()
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	return &handlerRecordingAdmitter{d: d}
}

func (r *handlerRecordingAdmitter) Acquire(ctx context.Context, req dispatch.Request) (*dispatch.Lease, context.Context, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	rej := r.reject
	r.mu.Unlock()
	if rej != nil {
		return nil, nil, rej
	}
	return r.d.Acquire(ctx, req)
}

func (r *handlerRecordingAdmitter) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

// TestQueryAdmissionBindsInteractive pins the query pipeline's
// classification (N1): translate/temporal/rerank-judge/synthesize all run
// through h.admission — interactive. The caller's principal is no Admission
// field since MW4 (design/03 §4.1.1): it derives from the request ctx via
// the boot-installed RequestPrincipal hook (probed below).
func TestQueryAdmissionBindsInteractive(t *testing.T) {
	rec := newHandlerRecordingAdmitter(t)
	h := NewQueryHandler(nil, nil, nil, nil, nil, rec)
	adm := h.admission()
	if adm.Class != dispatch.ClassInteractive {
		t.Fatalf("query pipeline class = %v, want interactive", adm.Class)
	}
	if adm.Admitter == nil {
		t.Fatal("query pipeline admission carries no admitter")
	}
}

// TestDailyAdmissionBindsInteractive pins the daily API path's
// classification (N8a, E-U2): interactive — valid only because of the
// acquireDaily brake. Principal: ctx-derived, see TestRequestPrincipal.
func TestDailyAdmissionBindsInteractive(t *testing.T) {
	rec := newHandlerRecordingAdmitter(t)
	h := NewSynthesizeHandler(nil, nil, nil, rec, nil)
	adm := h.dailyAdmission()
	if adm.Class != dispatch.ClassInteractive {
		t.Fatalf("daily API class = %v, want interactive", adm.Class)
	}
}

// TestRequestPrincipal pins the MW4 hook wrapper (design/03 §4.1.1): the
// dispatch principal derives from the auth middleware's ctx AuthResult —
// field-faithful mapping with an AuthResult present, the zero Principal
// without one (detached/anonymous ctx ⇒ structurally background; the B8
// emptiness pin on ApiKeyID lives inside dispatch and is probed there).
func TestRequestPrincipal(t *testing.T) {
	ctx := context.WithValue(context.Background(), authResultKey, testAuthResult())
	want := dispatch.Principal{ApiKeyID: "key-1", TenantID: "tenant-1", HomeScope: "scope-1"}
	if got := RequestPrincipal(ctx); got != want {
		t.Fatalf("RequestPrincipal = %+v, want %+v", got, want)
	}
	if got := RequestPrincipal(context.Background()); got != (dispatch.Principal{}) {
		t.Fatalf("RequestPrincipal on a bare ctx = %+v, want zero (structurally background)", got)
	}
}

// TestDailyDeckelPerPrincipal pins the E-U2 brake semantics: cap 1 per
// ApiKeyID, foreign keys unaffected, release frees the slot.
func TestDailyDeckelPerPrincipal(t *testing.T) {
	h := NewSynthesizeHandler(nil, nil, nil, nil, nil)
	if !h.acquireDaily("key-a") {
		t.Fatal("first acquire of key-a must pass")
	}
	if h.acquireDaily("key-a") {
		t.Fatal("second concurrent acquire of key-a must be rejected (Deckel = 1)")
	}
	if !h.acquireDaily("key-b") {
		t.Fatal("foreign key-b must be unaffected by key-a's slot")
	}
	h.releaseDaily("key-a")
	if !h.acquireDaily("key-a") {
		t.Fatal("after release the key-a slot must be free again")
	}
}

// TestHandleDailySecondConcurrentCall429 drives the W3 gate probe through
// the full handler: with a daily call of the same key in flight, the second
// POST answers 429 BEFORE any DB or LLM work — pinned by the nil pool (a
// reached GenerateDailyReport would nil-panic) and the untouched admitter.
func TestHandleDailySecondConcurrentCall429(t *testing.T) {
	rec := newHandlerRecordingAdmitter(t)
	h := NewSynthesizeHandler(nil, nil, nil, rec, nil)

	// Call 1 in flight: hold the key's slot exactly as HandleDaily would.
	if !h.acquireDaily("key-1") {
		t.Fatal("setup: could not hold the slot")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/synthesize/daily", nil)
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, testAuthResult()))
	rr := httptest.NewRecorder()
	h.HandleDaily(rr, req)

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("second concurrent daily call: status = %d, want 429", rr.Code)
	}
	if rec.count() != 0 {
		t.Fatalf("the 429 path must not acquire, got %d acquires", rec.count())
	}
	if !strings.Contains(rr.Body.String(), "\"ok\":false") {
		t.Fatalf("429 body not generic-fail: %s", rr.Body.String())
	}

	// A foreign key's call passes the brake (and then fails on the empty
	// pool state visible as a 500 — the brake itself must not block it).
	h.releaseDaily("key-1")
}

// TestProbeChatAcquiresInteractiveLease pins N8b: the backend chat probe
// runs under an interactive lease on the backend's origin; a rejected
// acquire makes NO wire call (doctrine), a nil admitter fails loudly.
func TestProbeChatAcquiresInteractiveLease(t *testing.T) {
	var wireCalls int
	orig := chatProbe
	chatProbe = func(ctx context.Context, b backends.Backend) (any, error) {
		wireCalls++
		return nil, nil
	}
	t.Cleanup(func() { chatProbe = orig })

	b := &backends.Backend{
		Name: "probe-target", Host: "http://probe:8089",
		ModelMap: map[string]backends.ModelSpec{"default": {Model: "m"}},
	}

	rec := newHandlerRecordingAdmitter(t)
	checks := map[string]string{}
	probeChat(context.Background(), rec, b, checks)
	if wireCalls != 1 || rec.count() != 1 {
		t.Fatalf("acquire==wire on probe: %d acquires vs %d wire calls", rec.count(), wireCalls)
	}
	got := rec.reqs[0]
	if got.Class != dispatch.ClassInteractive {
		t.Fatalf("probe request wrong: %+v", got)
	}
	if got.Target.Origin != "http://probe:8089" {
		t.Fatalf("probe target = %q, want the backend origin", got.Target.Origin)
	}
	if got.DeadlineIn != probeChatTimeout {
		t.Fatalf("probe deadline hint = %v, want %v", got.DeadlineIn, probeChatTimeout)
	}
	if checks["model_m"] != "ok" {
		t.Fatalf("probe checks = %v", checks)
	}

	// Rejection: no wire call, generic check result.
	wireCalls = 0
	rec2 := newHandlerRecordingAdmitter(t)
	rec2.reject = dispatch.ErrQueueFull
	checks = map[string]string{}
	probeChat(context.Background(), rec2, b, checks)
	if wireCalls != 0 {
		t.Fatalf("rejected probe made a wire call")
	}
	if checks["model_m"] != "error: admission rejected" {
		t.Fatalf("rejected probe checks = %v", checks)
	}

	// Nil admitter: loud failure, no wire call (I-D1).
	wireCalls = 0
	checks = map[string]string{}
	probeChat(context.Background(), nil, b, checks)
	if wireCalls != 0 || !strings.Contains(checks["model_m"], "not wired") {
		t.Fatalf("nil admitter: calls=%d checks=%v", wireCalls, checks)
	}
}
