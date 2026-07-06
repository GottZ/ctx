// MW19 consumer-semantics pins, unit half (design/02 §7 wave P2, §4.6): what
// the dream pipeline does when the dispatcher preempts one of its wire calls
// mid-flight. The scenarios run the REAL MW18 preempt path — a dedicated
// dispatcher with {slots:1, preempt_background:true} on the dream backend's
// origin, an interactive acquire as the trigger — against the chatJSON seam,
// which mimics the verified transport behavior of a canceled wire call: the
// returned error wraps context.Cause(runCtx) (Go ≥1.23 places the cancel
// CAUSE into the http error chain — pinned wire-level in
// dispatch/k1_wire_test.go, gate P1(h)).
//
// Pinned here:
//   - the error a consumer sees matches errors.Is(err, dispatch.ErrPreempted)
//     AND context.Canceled, and NOT ErrReaped (wrap contract §4.2.6);
//   - no failover spill: the following chain link stays call-less (PB5), with
//     the W5 counter-probe that a ClassTransport failure DOES spill — the
//     test could see a spill;
//   - no health report for the preempted attempt (PB8/B-R9), counter-probed
//     by the transport case reporting;
//   - a preempted keyword attempt retries through a NEW Acquire (P-N9).
//
// The DB-visible half (cooldown without back-off advance, no link rows,
// llmlog row, recurrence exception) lives in
// preempt_semantics_integration_test.go.
package dream

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llm"
)

const (
	mw19PrimaryOrigin   = "http://gpu-mw19:8089"
	mw19SecondaryOrigin = "http://cpu-mw19:8090"
)

// newPreemptDispatcher builds a real dispatcher whose primary origin is
// preempt-enabled with ONE slot — the live herbert-chat shape (design/02
// §4.4). The secondary origin carries no policy row (pass-through).
func newPreemptDispatcher(t *testing.T) *dispatch.Dispatcher {
	t.Helper()
	d := dispatch.New(nil, dispatch.DefaultSettings())
	d.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{
		mw19PrimaryOrigin: {Slots: 1, PreemptBackground: true},
	}})
	t.Cleanup(d.Close)
	return d
}

// reportRecorder captures pool-health feedback (Router.Report) — the PB8 pin
// asserts silence for preempted attempts and the counter-probe asserts a
// report for a genuine transport failure.
type reportRecorder struct {
	mu      sync.Mutex
	reports []struct {
		id    string
		class backends.ErrClass
	}
}

func (rr *reportRecorder) fn() llm.ReportFunc {
	return func(id string, class backends.ErrClass, _ time.Duration) {
		rr.mu.Lock()
		defer rr.mu.Unlock()
		rr.reports = append(rr.reports, struct {
			id    string
			class backends.ErrClass
		}{id, class})
	}
}

func (rr *reportRecorder) failures() []string {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	var out []string
	for _, r := range rr.reports {
		if r.class != backends.ClassOK {
			out = append(out, r.id+":"+r.class.String())
		}
	}
	return out
}

// newTwoBackendRouter seeds a primary (GPU shape, priority 100) and a
// fallback (CPU shape, priority 50) dream backend so the spill probes have a
// following chain link to observe. adm binds the caller's admission.
func newTwoBackendRouter(adm llm.Admission, report llm.ReportFunc) *Router {
	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{
		{
			ID: "mw19-gpu", Name: "mw19-gpu", Host: mw19PrimaryOrigin,
			Trust: backends.TrustFull, Locality: "lan",
			Roles:    []string{backends.RoleDream},
			ModelMap: map[string]backends.ModelSpec{"default": {Model: "m-gpu"}},
			Priority: 100, Enabled: true,
		},
		{
			ID: "mw19-cpu", Name: "mw19-cpu", Host: mw19SecondaryOrigin,
			Trust: backends.TrustFull, Locality: "lan",
			Roles:    []string{backends.RoleDream},
			ModelMap: map[string]backends.ModelSpec{"default": {Model: "m-cpu"}},
			Priority: 50, Enabled: true,
		},
	})
	return &Router{Pool: p, Admit: adm, Report: report}
}

// countingAdmitter counts Acquire calls while delegating to the real
// dispatcher — the P-N9 re-acquire pin needs the count, not a fake.
type countingAdmitter struct {
	inner dispatch.Admitter
	n     *atomic.Int32
}

func (c countingAdmitter) Acquire(ctx context.Context, req dispatch.Request) (*dispatch.Lease, context.Context, error) {
	c.n.Add(1)
	return c.inner.Acquire(ctx, req)
}

// fireInteractive opens the preempt trigger: an interactive acquire on the
// primary origin from an authenticated ctx. It releases its lease as soon as
// it is admitted (which per §4.2.4 happens at the VICTIM's release) and
// reports the acquire error.
func fireInteractive(d *dispatch.Dispatcher) <-chan error {
	done := make(chan error, 1)
	go func() {
		l, _, err := d.Acquire(WithInteractivePrincipal(context.Background()), dispatch.Request{
			Target: dispatch.Target{Origin: mw19PrimaryOrigin},
			Class:  dispatch.ClassInteractive,
			Role:   "mw19-trigger",
		})
		if err == nil {
			l.Release()
		}
		done <- err
	}()
	return done
}

// preemptingSeam installs a chatJSON seam whose FIRST primary-origin call
// signals in-flight, blocks until the dispatcher cancels its runCtx and
// returns the transport-mimicking cause-wrapped error. Every other call is
// recorded and answered with content.
func preemptingSeam(t *testing.T, calls *[]string, mu *sync.Mutex, inflight chan<- struct{}, content string) {
	t.Helper()
	var once sync.Once
	mockChatJSON(t, func(ctx context.Context, host, _, _ string, _ *bool, _, _ string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		mu.Lock()
		*calls = append(*calls, host)
		mu.Unlock()
		if host == mw19PrimaryOrigin {
			once.Do(func() { close(inflight) })
			<-ctx.Done()
			// The transport shape of a canceled wire call: the http error
			// chain carries the cancel CAUSE (pinned against the real
			// transport in dispatch/k1_wire_test.go).
			return nil, fmt.Errorf("post chat: %w", context.Cause(ctx))
		}
		return constResp(content)(ctx, "", "", "", nil, "", "", llm.Options{}, 0)
	})
}

// TestDreamEvalPreempted_ErrChainNoSpillNoHealthReport is the §4.6 dream-eval
// pin, wire-facing half: a preempted eval returns an error whose chain
// matches ErrPreempted (and context.Canceled, not ErrReaped), the following
// chain link is NEVER called (PB5 direction 1) and pool health stays
// untouched (PB8).
func TestDreamEvalPreempted_ErrChainNoSpillNoHealthReport(t *testing.T) {
	d := newPreemptDispatcher(t)
	rr := &reportRecorder{}
	r := newTwoBackendRouter(llm.Admission{Admitter: d, Class: dispatch.ClassBackground}, rr.fn())

	var mu sync.Mutex
	var calls []string
	inflight := make(chan struct{})
	preemptingSeam(t, &calls, &mu, inflight, `[]`)

	type evalResult struct {
		links []Link
		err   error
	}
	res := make(chan evalResult, 1)
	src, cand := srcBlock(uuidA), candBlock(uuidB)
	src.Sensitivity, cand.Sensitivity = backends.SensInternal, backends.SensInternal
	go func() {
		links, err := EvaluateRelationships(context.Background(), nil, r, llm.Options{}, src, []BlockInfo{cand})
		res <- evalResult{links, err}
	}()

	<-inflight
	trigger := fireInteractive(d)

	got := <-res
	if got.err == nil {
		t.Fatal("preempted eval must return an error")
	}
	// Wrap contract §4.2.6: the signature travels on the RETURNED error chain
	// — the only detection consumers may use (context.Cause stays
	// dispatcher-internal).
	if !errors.Is(got.err, dispatch.ErrPreempted) {
		t.Fatalf("errors.Is(err, ErrPreempted) = false, err = %v", got.err)
	}
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("preempt error chain must carry context.Canceled (K1 wrap), err = %v", got.err)
	}
	if errors.Is(got.err, dispatch.ErrReaped) {
		t.Fatalf("preempt error chain must be distinguishable from ErrReaped, err = %v", got.err)
	}
	if len(got.links) != 0 {
		t.Fatalf("preempted eval must yield no links, got %d", len(got.links))
	}

	// PB5 direction 1: the fallback backend stays call-less — a preempted
	// background prompt must never spill toward the next chain link.
	mu.Lock()
	callsCopy := append([]string(nil), calls...)
	mu.Unlock()
	if len(callsCopy) != 1 || callsCopy[0] != mw19PrimaryOrigin {
		t.Fatalf("wire calls = %v, want exactly one call to the primary (no spill)", callsCopy)
	}

	// PB8: no health report for the preempted attempt — a preempt must not
	// cooldown the healthy target (the counter-probe that a report WOULD be
	// visible is TestDreamEvalTransportError_SpillsToNextAndReports).
	if f := rr.failures(); len(f) != 0 {
		t.Fatalf("preempt fed pool health: %v (PB8 — routing self-poisoning)", f)
	}

	if err := <-trigger; err != nil {
		t.Fatalf("interactive trigger acquire: %v", err)
	}
}

// TestDreamEvalTransportError_SpillsToNextAndReports is the W5 counter-probe
// (PB5 direction 2): the SAME two-backend chain with a genuine
// ClassTransport failure on the primary DOES call the following link and DOES
// report the failure — proving the preempt test could see both a spill and a
// health report.
func TestDreamEvalTransportError_SpillsToNextAndReports(t *testing.T) {
	d := newPreemptDispatcher(t)
	rr := &reportRecorder{}
	r := newTwoBackendRouter(llm.Admission{Admitter: d, Class: dispatch.ClassBackground}, rr.fn())

	var mu sync.Mutex
	var calls []string
	mockChatJSON(t, func(ctx context.Context, host, _, _ string, _ *bool, _, _ string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		mu.Lock()
		calls = append(calls, host)
		mu.Unlock()
		if host == mw19PrimaryOrigin {
			return nil, fmt.Errorf("dial tcp %s: %w", host, syscall.ECONNREFUSED) // ClassTransport, Next()=true
		}
		return constResp(fmt.Sprintf(`[{"target_id":%q,"type":"topical","confidence":0.9}]`, uuidB))(ctx, "", "", "", nil, "", "", llm.Options{}, 0)
	})

	src, cand := srcBlock(uuidA), candBlock(uuidB)
	src.Sensitivity, cand.Sensitivity = backends.SensInternal, backends.SensInternal
	links, err := EvaluateRelationships(context.Background(), nil, r, llm.Options{}, src, []BlockInfo{cand})
	if err != nil {
		t.Fatalf("transport failure must fail over, got error: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("links = %d, want 1 from the fallback backend", len(links))
	}

	mu.Lock()
	callsCopy := append([]string(nil), calls...)
	mu.Unlock()
	if len(callsCopy) != 2 || callsCopy[0] != mw19PrimaryOrigin || callsCopy[1] != mw19SecondaryOrigin {
		t.Fatalf("wire calls = %v, want [primary, secondary] (spill visible)", callsCopy)
	}

	f := rr.failures()
	if len(f) != 1 || f[0] != "mw19-gpu:transport" {
		t.Fatalf("health reports = %v, want exactly [mw19-gpu:transport]", f)
	}
}

// TestDreamKeywordsPreempted_RetryReacquires is the §4.6 dream-keywords pin
// (P-N9): a preempted keyword attempt retries inside GenerateKeywords, and
// the retry goes through a NEW Acquire — it waits at the target instead of
// bypassing admission.
func TestDreamKeywordsPreempted_RetryReacquires(t *testing.T) {
	d := newPreemptDispatcher(t)
	var acquires atomic.Int32
	rr := &reportRecorder{}
	r := newTwoBackendRouter(llm.Admission{
		Admitter: countingAdmitter{inner: d, n: &acquires},
		Class:    dispatch.ClassBackground,
	}, rr.fn())
	// Single-link chain: drop the fallback so a preempted keyword attempt
	// cannot mask the retry count behind a spill (the no-spill property has
	// its own pin above).
	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{{
		ID: "mw19-gpu", Name: "mw19-gpu", Host: mw19PrimaryOrigin,
		Trust: backends.TrustFull, Locality: "lan",
		Roles:    []string{backends.RoleDream},
		ModelMap: map[string]backends.ModelSpec{"default": {Model: "m-gpu"}},
		Priority: 100, Enabled: true,
	}})
	r.Pool = p

	// The seam here is call-COUNT driven (unlike preemptingSeam's host-driven
	// shape): the first attempt is preempted mid-flight, the RETRY on the
	// same backend answers — both land on the single-link chain.
	inflight := make(chan struct{})
	var wire atomic.Int32
	mockChatJSON(t, func(ctx context.Context, _, _, _ string, _ *bool, _, _ string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		if wire.Add(1) == 1 {
			close(inflight)
			<-ctx.Done()
			return nil, fmt.Errorf("post chat: %w", context.Cause(ctx))
		}
		return constResp(`["alpha term","beta term","gamma term","delta term","epsilon term"]`)(ctx, "", "", "", nil, "", "", llm.Options{}, 0)
	})

	blk := srcBlock(uuidA)
	blk.Sensitivity = backends.SensInternal
	type kwResult struct {
		kws []string
		err error
	}
	res := make(chan kwResult, 1)
	go func() {
		kws, err := GenerateKeywords(context.Background(), nil, r, &blk)
		res <- kwResult{kws, err}
	}()

	<-inflight
	trigger := fireInteractive(d)

	got := <-res
	if got.err != nil {
		t.Fatalf("retry after preempt must succeed, got: %v", got.err)
	}
	if len(got.kws) != 5 {
		t.Fatalf("keywords = %v, want 5", got.kws)
	}
	if n := acquires.Load(); n != 2 {
		t.Fatalf("Acquire count = %d, want 2 — the retry must re-acquire, not reuse a lease (P-N9)", n)
	}
	if n := wire.Load(); n != 2 {
		t.Fatalf("wire calls = %d, want 2 (preempted attempt + retry)", n)
	}
	if f := rr.failures(); len(f) != 0 {
		t.Fatalf("keyword preempt fed pool health: %v", f)
	}
	if err := <-trigger; err != nil {
		t.Fatalf("interactive trigger acquire: %v", err)
	}
}
