package llm

// MW3 admission tests: acquire==wire coupling per attempt, the acquire-error
// doctrine (terminal, no attempt, no health report, no llmlog row) and the
// MW22 usage feed of the non-stream chat path (design/01 §7 W3 gates).

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
)

// testAdmission returns a live pass-through admission (empty policy =
// Durchreiche) bound to class. Interactive gets a non-empty principal so the
// B8 downgrade never fires in unrelated tests.
func testAdmission(t *testing.T, class dispatch.Class) Admission {
	t.Helper()
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	adm := Admission{Admitter: d, Class: class}
	if class == dispatch.ClassInteractive {
		adm.Principal = dispatch.Principal{ApiKeyID: "test-key", TenantID: "test-tenant", HomeScope: "private"}
	}
	return adm
}

// recordingAdmitter delegates to a real dispatcher and records every request
// (classification + deadline-hint probes) — plus an optional scripted
// rejection per call index.
type recordingAdmitter struct {
	d  *dispatch.Dispatcher
	mu sync.Mutex
	// reqs are the recorded Acquire inputs in call order.
	reqs []dispatch.Request
	// rejectAt maps call index (0-based) → error to return instead of
	// delegating (scripted ErrQueueFull etc.).
	rejectAt map[int]error
}

func newRecordingAdmitter(t *testing.T) *recordingAdmitter {
	t.Helper()
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	return &recordingAdmitter{d: d, rejectAt: map[int]error{}}
}

func (r *recordingAdmitter) Acquire(ctx context.Context, req dispatch.Request) (*dispatch.Lease, context.Context, error) {
	r.mu.Lock()
	idx := len(r.reqs)
	r.reqs = append(r.reqs, req)
	rej := r.rejectAt[idx]
	r.mu.Unlock()
	if rej != nil {
		return nil, nil, rej
	}
	return r.d.Acquire(ctx, req)
}

func (r *recordingAdmitter) recorded() []dispatch.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]dispatch.Request, len(r.reqs))
	copy(out, r.reqs)
	return out
}

// admissionBackend builds a minimal resolved-chain backend for the seam.
func admissionBackend(name, host string) backends.Backend {
	return backends.Backend{
		ID:   name + "-id",
		Name: name,
		Host: host,
		ModelMap: map[string]backends.ModelSpec{
			"default": {Model: "test-model"},
		},
	}
}

// TestChatChainViaAcquireEqualsWireCalls pins the W3 coupling gate on the
// non-stream path: one Acquire per ATTEMPT on the attempt's origin — the
// failover walk acquires again for the next link, and the acquire count
// equals the wire-call count (the full 6-site invariant gate is MW6).
func TestChatChainViaAcquireEqualsWireCalls(t *testing.T) {
	rec := newRecordingAdmitter(t)
	adm := Admission{
		Admitter:  rec,
		Class:     dispatch.ClassInteractive,
		Principal: dispatch.Principal{ApiKeyID: "k1", TenantID: "t1", HomeScope: "private"},
	}
	chain := []backends.Backend{
		admissionBackend("first", "http://first:8089"),
		admissionBackend("second", "http://second:8089"),
	}
	var wireCalls int
	call := func(ctx context.Context, b backends.Backend, _, _ string, _ Options, _ time.Duration) (*ChatResponse, error) {
		wireCalls++
		if b.Name == "first" {
			return nil, fmt.Errorf("boom: connection refused") // retryable → failover
		}
		return &ChatResponse{Message: Message{Content: "ok"}, PromptTokens: 10, EvalCount: 5}, nil
	}

	resp, served, attempts, err := ChatChainVia(context.Background(), call, chain, "chat",
		"sys", "usr", Options{}, time.Second, nil, adm)
	if err != nil || resp == nil || served == nil || served.Name != "second" {
		t.Fatalf("expected second backend to serve, got served=%v err=%v", served, err)
	}
	if len(attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(attempts))
	}
	reqs := rec.recorded()
	if len(reqs) != wireCalls || len(reqs) != 2 {
		t.Fatalf("acquire==wire coupling broken: %d acquires vs %d wire calls", len(reqs), wireCalls)
	}
	// Per-attempt target origin: the acquire names the ATTEMPT's backend.
	if reqs[0].Target.Origin != "http://first:8089" || reqs[1].Target.Origin != "http://second:8089" {
		t.Fatalf("acquire targets wrong: %+v", reqs)
	}
	for i, rq := range reqs {
		if rq.Class != dispatch.ClassInteractive {
			t.Fatalf("attempt %d: class = %v, want interactive", i, rq.Class)
		}
		if rq.Principal.ApiKeyID != "k1" {
			t.Fatalf("attempt %d: principal not threaded: %+v", i, rq.Principal)
		}
		// Deadline hint travels admission-anchored: DeadlineIn == attempt
		// timeout, no absolute pre-acquire Deadline (MW1 contract note).
		if rq.DeadlineIn != time.Second || !rq.Deadline.IsZero() {
			t.Fatalf("attempt %d: deadline hint wrong: in=%v abs=%v", i, rq.DeadlineIn, rq.Deadline)
		}
	}
}

// TestChatChainViaAcquireRejectionIsTerminal pins the acquire-error doctrine
// (design/01 §4.3, W3 negative gate): a rejected acquire is NOT an attempt —
// no wire call, no ChainAttempt entry, no health report, and NO failover
// onto the following chain link (no openrouter spill).
func TestChatChainViaAcquireRejectionIsTerminal(t *testing.T) {
	rec := newRecordingAdmitter(t)
	rec.rejectAt[0] = dispatch.ErrQueueFull
	adm := Admission{Admitter: rec, Class: dispatch.ClassBackground}
	chain := []backends.Backend{
		admissionBackend("local", "http://local:8089"),
		admissionBackend("openrouter", "https://openrouter.example"),
	}
	var wireCalls, reports int
	call := func(context.Context, backends.Backend, string, string, Options, time.Duration) (*ChatResponse, error) {
		wireCalls++
		return &ChatResponse{Message: Message{Content: "must never run"}}, nil
	}
	report := func(string, backends.ErrClass, time.Duration) { reports++ }

	resp, served, attempts, err := ChatChainVia(context.Background(), call, chain, "dream",
		"sys", "usr", Options{}, time.Second, report, adm)
	if resp != nil || served != nil {
		t.Fatalf("rejected acquire must not serve: resp=%v served=%v", resp, served)
	}
	if !errors.Is(err, dispatch.ErrQueueFull) || !dispatch.IsRejection(err) {
		t.Fatalf("error must carry the rejection sentinel, got %v", err)
	}
	if !IsAdmissionError(err) {
		t.Fatalf("error must be an AdmissionError, got %T %v", err, err)
	}
	if wireCalls != 0 {
		t.Fatalf("no wire call allowed after rejection, got %d", wireCalls)
	}
	if len(attempts) != 0 {
		t.Fatalf("a rejection is not an attempt, got %d entries", len(attempts))
	}
	if reports != 0 {
		t.Fatalf("no health report allowed for a rejection, got %d", reports)
	}
	if got := len(rec.recorded()); got != 1 {
		t.Fatalf("terminal: no acquire on the following link, got %d acquires", got)
	}
}

// TestChatChainViaNilAdmitterFailsLoudly pins the no-nil-default rule: a
// zero Admission never passes an unadmitted wire call (I-D1).
func TestChatChainViaNilAdmitterFailsLoudly(t *testing.T) {
	var wireCalls int
	call := func(context.Context, backends.Backend, string, string, Options, time.Duration) (*ChatResponse, error) {
		wireCalls++
		return nil, nil
	}
	_, _, attempts, err := ChatChainVia(context.Background(), call,
		[]backends.Backend{admissionBackend("b", "http://b:1")}, "chat",
		"s", "u", Options{}, time.Second, nil, Admission{})
	if err == nil || wireCalls != 0 || len(attempts) != 0 {
		t.Fatalf("nil admitter must fail loudly before the wire: err=%v calls=%d attempts=%d", err, wireCalls, len(attempts))
	}
	if !IsAdmissionError(err) {
		t.Fatalf("nil-admitter error must be an AdmissionError, got %v", err)
	}
}

// TestChatChainViaReportsUsageAtRelease pins the MW22 feed: the non-stream
// path books the backend-reported tokens into the interactive fairness
// window at Release.
func TestChatChainViaReportsUsageAtRelease(t *testing.T) {
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	adm := Admission{
		Admitter:  d,
		Class:     dispatch.ClassInteractive,
		Principal: dispatch.Principal{ApiKeyID: "k1", TenantID: "t1", HomeScope: "scope-a"},
	}
	call := func(context.Context, backends.Backend, string, string, Options, time.Duration) (*ChatResponse, error) {
		return &ChatResponse{Message: Message{Content: "ok"}, PromptTokens: 40, EvalCount: 2}, nil
	}
	_, _, _, err := ChatChainVia(context.Background(), call,
		[]backends.Backend{admissionBackend("b", "http://b:8089")}, "chat",
		"s", "u", Options{}, time.Second, nil, adm)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	snap := d.Snapshot()
	if snap.UnchargedCalls != 0 {
		t.Fatalf("usage was reported — uncharged must stay 0, got %d", snap.UnchargedCalls)
	}
	var tokens int64
	for _, ts := range snap.Targets {
		for _, b := range ts.Buckets {
			if b.FairKey == "scope-a" {
				tokens += b.Tokens
			}
		}
	}
	if tokens != 42 {
		t.Fatalf("expected 42 charged tokens in bucket scope-a, got %d", tokens)
	}
}

// TestChainCallDoRejectionMakesNoWireContact drives the doctrine through the
// ChainCall funnel itself (the home of the 4 Q-only call sites): a rejected
// acquire ends Do terminally with ZERO wire contact — the recorder's server
// never sees a request — and Do returns before the llmlog row is built (the
// K9 rejection line is MW10's; the row-level assert needs a DB and lands
// there).
func TestChainCallDoRejectionMakesNoWireContact(t *testing.T) {
	wire := newWireRecorder(t, "must never answer")
	bpool := translatePool(wire.backend(backends.ProtocolOllama))
	rec := newRecordingAdmitter(t)
	rec.rejectAt[0] = dispatch.ErrPrincipalSaturated
	adm := Admission{Admitter: rec, Class: dispatch.ClassBackground}

	_, err := ChainCall{
		Pool: bpool, Role: backends.RoleTranslate, Required: backends.SensInternal,
		Pipeline: "query-translate", System: "s", User: "u",
		DefTimeout: time.Second,
	}.Do(context.Background(), nil, adm)

	if !IsAdmissionError(err) || !errors.Is(err, dispatch.ErrPrincipalSaturated) {
		t.Fatalf("want terminal admission error, got %v", err)
	}
	if path, body := wire.recorded(); path != "" || body != "" {
		t.Fatalf("rejected Do made wire contact: path=%q body=%q", path, body)
	}
	if got := len(rec.recorded()); got != 1 {
		t.Fatalf("terminal: exactly one acquire expected, got %d", got)
	}
}

// TestChainAdmissionOrderInteractiveBeforeBackground is the W3 integration
// gate (design/01 §7): slots=1 fake policy, 1 background wire call running,
// 1 interactive + 1 background waiting ⇒ admission order interactive →
// background, driven through the REAL non-stream chain walk.
func TestChainAdmissionOrderInteractiveBeforeBackground(t *testing.T) {
	const origin = "http://gpu:8089"
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	d.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{origin: {Slots: 1}}})

	chain := []backends.Backend{admissionBackend("gpu", origin)}
	bg := Admission{Admitter: d, Class: dispatch.ClassBackground}
	ia := Admission{Admitter: d, Class: dispatch.ClassInteractive,
		Principal: dispatch.Principal{ApiKeyID: "k", TenantID: "t", HomeScope: "s"}}

	release := make(chan struct{})
	firstRunning := make(chan struct{})
	var mu sync.Mutex
	var order []string
	call := func(name string, hold bool) ChatFunc {
		return func(context.Context, backends.Backend, string, string, Options, time.Duration) (*ChatResponse, error) {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			if hold {
				close(firstRunning)
				<-release
			}
			return &ChatResponse{Message: Message{Content: name}}, nil
		}
	}

	var wg sync.WaitGroup
	run := func(name string, adm Admission, hold bool, started chan struct{}) {
		defer wg.Done()
		if started != nil {
			close(started)
		}
		_, _, _, err := ChatChainVia(context.Background(), call(name, hold), chain, "chat",
			"s", "u", Options{}, time.Minute, nil, adm)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	wg.Add(1)
	go run("bg-running", bg, true, nil)
	<-firstRunning // slot held by the background wire call

	// Queue one interactive and one background acquire behind the slot.
	waitDepth := func(want int) {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			snap := d.Snapshot()
			for _, ts := range snap.Targets {
				if ts.Origin == origin && ts.Interactive.Waiting+ts.Background.Waiting == want {
					return
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
		t.Fatalf("timeout waiting for queue depth %d", want)
	}
	wg.Add(1)
	go run("bg-waiting", bg, false, nil)
	waitDepth(1)
	wg.Add(1)
	go run("interactive-waiting", ia, false, nil)
	waitDepth(2)

	close(release) // background wire call ends, slot frees
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	want := []string{"bg-running", "interactive-waiting", "bg-waiting"}
	if len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Fatalf("admission order = %v, want %v (strict class order interactive→background)", order, want)
	}
}
