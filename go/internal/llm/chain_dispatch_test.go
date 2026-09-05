package llm

// MW10 dispatch-telemetry tests (design/05 A5-W4 gates): the §4.4c abort
// rule (terminal, no report, errors.Is over context.Cause), the §4.4a
// duration fix (wait-free duration_ms, additive queue_wait_ms), the row
// derivation from the row-defining attempt (§3.2 attribution) and the K9
// rejection line (narrow exception to the acquire-error doctrine).

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llmlog"
)

// TestDispatchAbortVocabulary pins the dispatch_abort value set (design/05
// §3.1: the DB column deliberately has NO CHECK constraint — this test IS
// the value contract; a silent rename would desync every existing row).
func TestDispatchAbortVocabulary(t *testing.T) {
	want := map[string]string{
		llmlog.AbortPreempted:      "preempted",
		llmlog.AbortReaped:         "reaped",
		llmlog.AbortAcquireExpired: "acquire_expired",
		llmlog.AbortQueueFull:      "queue_full",
	}
	if len(want) != 4 {
		t.Fatalf("vocabulary must have exactly 4 distinct values, got %d", len(want))
	}
	for got, pin := range want {
		if got != pin {
			t.Fatalf("abort constant drifted: %q != %q", got, pin)
		}
	}
}

// TestDispatchAbortClass pins the wrap-safe cause discrimination (B-R8): the
// sentinels match through arbitrary decoration via errors.Is, a parent
// cancel and an un-canceled ctx yield "". The classifier itself lives in
// llmlog next to the vocabulary it returns; the probe stays here with the
// pipeline probes that drive the same rule through the real chain walk.
func TestDispatchAbortClass(t *testing.T) {
	mk := func(cause error) context.Context {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(cause)
		return ctx
	}
	cases := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"preempted", mk(dispatch.ErrPreempted), llmlog.AbortPreempted},
		{"reaped", mk(dispatch.ErrReaped), llmlog.AbortReaped},
		// Decorated causes MUST still match (errors.Is, never sentinel
		// identity — a %w wrap with target origin would silently fall to
		// NULL under == and empty every preemption metric).
		{"preempted wrapped", mk(fmt.Errorf("origin gpu:8089: %w", dispatch.ErrPreempted)), llmlog.AbortPreempted},
		{"reaped wrapped", mk(fmt.Errorf("wire aborted: %w", dispatch.ErrReaped)), llmlog.AbortReaped},
		// Parent cancel (shutdown, dream-off, client disconnect) is NOT a
		// dispatcher abort (B-R8).
		{"parent cancel", mk(context.Canceled), ""},
		{"deadline", mk(context.DeadlineExceeded), ""},
		{"live ctx", context.Background(), ""},
	}
	for _, tc := range cases {
		if got := llmlog.AbortClass(tc.ctx); got != tc.want {
			t.Errorf("%s: llmlog.AbortClass = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestChatChainViaPreemptCauseIsTerminal drives the §4.4c abort rule through
// the REAL preempt path (B-R9 negative probe): slots=1 +
// preempt_background=true, a background wire call running, an interactive
// acquire arrives and preempts it. The aborted attempt must be TERMINAL — no
// failover onto the second chain link (no openrouter spill caused by a
// scheduling decision) and no health report (the pool health state is shared
// with interactive; a preempt must never cooldown the target) — and carry
// the class "preempted" instead of the generic "canceled".
func TestChatChainViaPreemptCauseIsTerminal(t *testing.T) {
	const origin = "http://gpu:8089"
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	d.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{
		origin: {Slots: 1, PreemptBackground: true},
	}})

	chain := []backends.Backend{
		admissionBackend("gpu", origin),
		admissionBackend("fallback", "http://fallback:8089"),
	}
	bg := Admission{Admitter: d, Class: dispatch.ClassBackground}

	running := make(chan struct{})
	var mu sync.Mutex
	var wired []string
	var reports int
	call := func(ctx context.Context, b backends.Backend, _, _ string, _ Options, _ time.Duration) (*ChatResponse, error) {
		mu.Lock()
		wired = append(wired, b.Name)
		mu.Unlock()
		if b.Name == "gpu" {
			close(running)
			<-ctx.Done() // the preempt cancels runCtx with cause ErrPreempted
			return nil, fmt.Errorf("wire aborted: %w", context.Cause(ctx))
		}
		return &ChatResponse{Message: Message{Content: "must never spill here"}}, nil
	}
	report := func(string, backends.ErrClass, time.Duration) { reports++ }

	done := make(chan struct{})
	var attempts []ChainAttempt
	var walkErr error
	go func() {
		defer close(done)
		_, _, attempts, walkErr = ChatChainVia(context.Background(), call, chain, "dream",
			"s", "u", Options{}, time.Minute, report, bg)
	}()
	<-running

	// Interactive acquire on the full target opens the preempt (MW18).
	// MW4: the principal binds via the request ctx (testPrincipalCtx installs
	// the hook), never via a Request field.
	ia, iaCtx, err := d.Acquire(testPrincipalCtx(), dispatch.Request{
		Target: dispatch.Target{Origin: origin},
		Class:  dispatch.ClassInteractive,
	})
	if err != nil {
		t.Fatalf("interactive acquire: %v", err)
	}
	_ = iaCtx
	defer ia.Release()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("preempted walk did not return")
	}

	if walkErr == nil {
		t.Fatal("preempted attempt must surface its error")
	}
	if len(attempts) != 1 {
		t.Fatalf("§4.4c terminal: exactly 1 attempt (no failover), got %d: %+v", len(attempts), attempts)
	}
	if attempts[0].Class != llmlog.AbortPreempted {
		t.Fatalf("attempt class = %q, want %q (not the generic 'canceled')", attempts[0].Class, llmlog.AbortPreempted)
	}
	if reports != 0 {
		t.Fatalf("§4.4c: no health report / cooldown for a dispatcher abort, got %d", reports)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(wired) != 1 || wired[0] != "gpu" {
		t.Fatalf("no wire contact past the aborted attempt, got %v", wired)
	}

	// Row derivation: the aborted background attempt yields dispatch_abort.
	var entry llmlog.Entry
	applyDispatchTelemetry(&entry, attempts, dispatch.ClassBackground)
	if entry.DispatchAbort != llmlog.AbortPreempted {
		t.Fatalf("entry.DispatchAbort = %q, want %q", entry.DispatchAbort, llmlog.AbortPreempted)
	}
	if entry.DispatchClass != "background" {
		t.Fatalf("entry.DispatchClass = %q, want background", entry.DispatchClass)
	}
	if entry.QueueWaitMs == nil {
		t.Fatal("aborted attempt must still carry its queue wait")
	}
}

// TestChatChainViaParentCancelStaysCanceled is the B-R8 negative probe: a
// parent-ctx cancel (shutdown, client disconnect) is NOT a dispatcher abort —
// the attempt keeps the generic "canceled" class and the derived row carries
// dispatch_abort NULL.
func TestChatChainViaParentCancelStaysCanceled(t *testing.T) {
	adm := testAdmission(t, dispatch.ClassBackground)
	parent, cancel := context.WithCancel(context.Background())
	call := func(ctx context.Context, _ backends.Backend, _, _ string, _ Options, _ time.Duration) (*ChatResponse, error) {
		cancel() // parent dies mid-call
		<-ctx.Done()
		return nil, ctx.Err()
	}
	var reports int
	report := func(string, backends.ErrClass, time.Duration) { reports++ }

	_, _, attempts, err := ChatChainVia(parent, call,
		[]backends.Backend{admissionBackend("gpu", "http://gpu:8089")}, "dream",
		"s", "u", Options{}, time.Minute, report, adm)
	if err == nil {
		t.Fatal("canceled walk must error")
	}
	if len(attempts) != 1 || attempts[0].Class != "canceled" {
		t.Fatalf("parent cancel must classify as 'canceled', got %+v", attempts)
	}
	if reports != 0 {
		t.Fatalf("ClassCanceled never reports health, got %d", reports)
	}
	var entry llmlog.Entry
	applyDispatchTelemetry(&entry, attempts, dispatch.ClassBackground)
	if entry.DispatchAbort != "" {
		t.Fatalf("parent cancel must leave dispatch_abort NULL, got %q", entry.DispatchAbort)
	}
}

// TestApplyDispatchTelemetry pins the §3.2 row derivation: row-defining
// attempt = last WIRE attempt, wait 0 persists as 0 (B-R4, never the
// nullInt convention), no_model pseudo-attempts never define the row, and
// an interactive row never carries an abort value (class invariant).
func TestApplyDispatchTelemetry(t *testing.T) {
	// 2-attempt failover: wait on link 1, answer from link 2 — the row
	// carries wait/duration of link 2; link 1 stays metadata.chain-only.
	var e llmlog.Entry
	applyDispatchTelemetry(&e, []ChainAttempt{
		{Backend: "gpu", Class: "transport", Ms: 5, WaitMs: 120},
		{Backend: "cpu", Class: "ok", Ms: 30, WaitMs: 0},
	}, dispatch.ClassInteractive)
	if e.QueueWaitMs == nil || *e.QueueWaitMs != 0 {
		t.Fatalf("B-R4: wait 0 must persist as 0, not NULL — got %v", e.QueueWaitMs)
	}
	if e.Duration != 30*time.Millisecond {
		t.Fatalf("duration must be the row-defining attempt's wire elapsed, got %v", e.Duration)
	}
	if e.DispatchClass != "interactive" || e.DispatchAbort != "" {
		t.Fatalf("class invariant: interactive row without abort, got class=%q abort=%q", e.DispatchClass, e.DispatchAbort)
	}

	// Full failure: the LAST tried attempt defines the row.
	var f llmlog.Entry
	applyDispatchTelemetry(&f, []ChainAttempt{
		{Backend: "gpu", Class: "transport", Ms: 5, WaitMs: 40},
		{Backend: "cpu", Class: llmlog.AbortReaped, Ms: 700, WaitMs: 8},
	}, dispatch.ClassBackground)
	if f.QueueWaitMs == nil || *f.QueueWaitMs != 8 || f.Duration != 700*time.Millisecond {
		t.Fatalf("last attempt must define the row, got wait=%v dur=%v", f.QueueWaitMs, f.Duration)
	}
	if f.DispatchAbort != llmlog.AbortReaped {
		t.Fatalf("reaped attempt must set dispatch_abort, got %q", f.DispatchAbort)
	}

	// no_model pseudo-attempts (no lease, no wire) never define the row.
	var g llmlog.Entry
	applyDispatchTelemetry(&g, []ChainAttempt{
		{Backend: "gpu", Class: "ok", Ms: 25, WaitMs: 3},
		{Backend: "broken", Class: "no_model"},
	}, dispatch.ClassBackground)
	if g.QueueWaitMs == nil || *g.QueueWaitMs != 3 || g.Duration != 25*time.Millisecond {
		t.Fatalf("no_model must be skipped, got wait=%v dur=%v", g.QueueWaitMs, g.Duration)
	}

	// Walk with ONLY no_model attempts: lease-free — QueueWaitMs stays nil.
	var h llmlog.Entry
	applyDispatchTelemetry(&h, []ChainAttempt{{Backend: "broken", Class: "no_model"}}, dispatch.ClassBackground)
	if h.QueueWaitMs != nil {
		t.Fatalf("lease-free walk must leave queue_wait_ms NULL, got %v", *h.QueueWaitMs)
	}
}

// TestChainWaitIsSeparateFromDuration is the §4.4a negative probe: an
// engineered lease wait W must land in queue_wait_ms and NOT inflate the
// attempt duration (the old chain-walk span contained it — wait-inflated
// duration_ms was exactly the state the telemetry is meant to measure).
func TestChainWaitIsSeparateFromDuration(t *testing.T) {
	const origin = "http://gpu:8089"
	const engineeredWait = 150 * time.Millisecond
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	d.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{origin: {Slots: 1}}})

	// Occupy the single slot, release after W: the chain's acquire waits W.
	blocker, _, err := d.Acquire(context.Background(), dispatch.Request{
		Target: dispatch.Target{Origin: origin}, Class: dispatch.ClassBackground,
	})
	if err != nil {
		t.Fatalf("blocker acquire: %v", err)
	}
	go func() {
		time.Sleep(engineeredWait)
		blocker.Release()
	}()

	call := func(context.Context, backends.Backend, string, string, Options, time.Duration) (*ChatResponse, error) {
		time.Sleep(20 * time.Millisecond) // the physical call time
		return &ChatResponse{Message: Message{Content: "ok"}}, nil
	}
	adm := Admission{Admitter: d, Class: dispatch.ClassBackground}
	_, _, attempts, err := ChatChainVia(context.Background(), call,
		[]backends.Backend{admissionBackend("gpu", origin)}, "dream",
		"s", "u", Options{}, time.Minute, nil, adm)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("walk failed: err=%v attempts=%+v", err, attempts)
	}

	var entry llmlog.Entry
	applyDispatchTelemetry(&entry, attempts, dispatch.ClassBackground)
	if entry.QueueWaitMs == nil || *entry.QueueWaitMs < 100 {
		t.Fatalf("queue_wait_ms must carry the engineered wait (~150ms), got %v", entry.QueueWaitMs)
	}
	if entry.Duration >= 100*time.Millisecond {
		t.Fatalf("duration must be wait-free (~20ms physical call), got %v — wait-inflated", entry.Duration)
	}
	if entry.Duration <= 0 {
		t.Fatalf("duration must carry the physical call time, got %v", entry.Duration)
	}
}

// TestRejectionEntry pins the K9 line filter (design/05 §3.2): background
// queue_full/acquire_expired build the ONE telemetry line (duration NULL via
// NoWireCall, futile wait attributed to the rejected target); interactive
// rejections and parent cancels write NOTHING (class invariant — tenants
// never see scheduler interventions in their rows).
func TestRejectionEntry(t *testing.T) {
	queueFull := &AdmissionError{Err: dispatch.ErrQueueFull, Backend: "gpu", Host: "http://gpu:8089", WaitMs: 0}
	entry, ok := rejectionEntry("dream-evaluate", queueFull, dispatch.ClassBackground)
	if !ok {
		t.Fatal("background queue_full must build the K9 line")
	}
	if entry.DispatchAbort != llmlog.AbortQueueFull || entry.DispatchClass != "background" {
		t.Fatalf("K9 line wrong: %+v", entry)
	}
	if !entry.NoWireCall {
		t.Fatal("K9 line has no physical call — duration_ms must persist NULL")
	}
	if entry.QueueWaitMs == nil || *entry.QueueWaitMs != 0 {
		t.Fatalf("queue_full fail-fast wait 0 is a real measurement, got %v", entry.QueueWaitMs)
	}
	if entry.BackendName != "gpu" || entry.Host != "http://gpu:8089" {
		t.Fatalf("K9 line must attribute the rejected target, got %+v", entry)
	}

	expired := &AdmissionError{Err: context.DeadlineExceeded, Backend: "gpu", Host: "h", WaitMs: 2500}
	entry, ok = rejectionEntry("dream-evaluate", expired, dispatch.ClassBackground)
	if !ok || entry.DispatchAbort != llmlog.AbortAcquireExpired {
		t.Fatalf("expired background acquire must build acquire_expired, got ok=%v %+v", ok, entry)
	}
	if entry.QueueWaitMs == nil || *entry.QueueWaitMs != 2500 {
		t.Fatalf("futile wait must persist, got %v", entry.QueueWaitMs)
	}

	// Interactive rejections (cap ladder) write NOTHING — the error goes to
	// the caller; scheduler traces stay out of tenant-visible rows.
	if _, ok := rejectionEntry("query-translate",
		&AdmissionError{Err: dispatch.ErrPrincipalSaturated}, dispatch.ClassInteractive); ok {
		t.Fatal("interactive rejection must not write a K9 line")
	}
	if _, ok := rejectionEntry("query-synthesize",
		&AdmissionError{Err: context.DeadlineExceeded}, dispatch.ClassInteractive); ok {
		t.Fatal("interactive expiry must not write a K9 line")
	}
	// Parent cancel (shutdown/dream-off) is lifecycle, not an admission
	// verdict — no line.
	if _, ok := rejectionEntry("dream-evaluate",
		&AdmissionError{Err: context.Canceled}, dispatch.ClassBackground); ok {
		t.Fatal("parent cancel must not write a K9 line")
	}
	// A rejection error without AdmissionError provenance (defensive).
	if _, ok := rejectionEntry("dream-evaluate", dispatch.ErrQueueFull, dispatch.ClassBackground); ok {
		t.Fatal("errors without acquire provenance must not write a K9 line")
	}
}

// TestChainCallDoWritesRejectionThroughFunnel drives the K9 path through the
// ChainCall funnel: the rejected background Do still returns terminally with
// zero wire contact (doctrine unchanged — the line is telemetry-only; the
// row-level assert lives in the integration test).
func TestChainCallDoWritesRejectionThroughFunnel(t *testing.T) {
	wire := newWireRecorder(t, "must never answer")
	bpool := translatePool(wire.backend(backends.ProtocolOllama))
	rec := newRecordingAdmitter(t)
	rec.rejectAt[0] = dispatch.ErrQueueFull
	adm := Admission{Admitter: rec, Class: dispatch.ClassBackground}

	_, err := ChainCall{
		Pool: bpool, Role: backends.RoleTranslate, Required: backends.SensInternal,
		Pipeline: "dream-evaluate", System: "s", User: "u",
		DefTimeout: time.Second,
	}.Do(context.Background(), nil, adm)

	if !IsAdmissionError(err) || !errors.Is(err, dispatch.ErrQueueFull) {
		t.Fatalf("K9 line must not change the terminal error, got %v", err)
	}
	if path, body := wire.recorded(); path != "" || body != "" {
		t.Fatalf("rejected Do made wire contact: path=%q body=%q", path, body)
	}
}
