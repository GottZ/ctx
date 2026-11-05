package llm

// A03-W17 gates (masterplan K21, design/02 §9.2 A03-7, §4.11.1): the three
// doors of llm.Admission for the /v1 walk — AcquireWith (the one blocking
// wait point, First), TryAcquire (Next: never waits) and Continue (the next
// sub-block, the dispatcher's continuation acquire). All three share
// Acquire's form and its AdmissionError hull; the per-request inputs travel
// in AdmitOptions (set by the /v1 planner, A03-W8). An admitter without a
// non-blocking door is an error before any acquire — never a silent fall
// back onto the blocking Acquire (L5: one wait point per request).

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/dispatch"
)

const doorOrigin = "http://gpu:8089"

// consumerCtx is a model-key request ctx: a consumer principal with a tenant
// and without a knowledge key (I-K0) — the only principal the external class
// admits.
func consumerCtx(consumer string) context.Context {
	return withTestPrincipal(context.Background(), dispatch.Principal{ConsumerID: consumer, TenantID: "tenant-a", ConsumerKind: "app", FairWeight: 10})
}

// partitionDispatcher is a live dispatcher with one slot-managed target
// S = 4, R = 2 (E = 2) that offers the external class.
func partitionDispatcher(t *testing.T) *dispatch.Dispatcher {
	t.Helper()
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	d.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{doorOrigin: {Slots: 4, InternalReserved: 2}}})
	return d
}

func externalWaiting(d *dispatch.Dispatcher) int {
	for _, ts := range d.Snapshot().Targets {
		if ts.Origin == doorOrigin {
			return ts.External.Waiting
		}
	}
	return 0
}

// countingAdmitter has only the blocking door and counts its calls.
type countingAdmitter struct {
	mu   sync.Mutex
	reqs []dispatch.Request
}

func (c *countingAdmitter) Acquire(_ context.Context, req dispatch.Request) (*dispatch.Lease, context.Context, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, req)
	return nil, nil, errors.New("counting admitter: no lease")
}

func (c *countingAdmitter) calls() []dispatch.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]dispatch.Request(nil), c.reqs...)
}

// tryRecorder records both doors and answers with scripted errors.
type tryRecorder struct {
	countingAdmitter
	tries []dispatch.Request
}

func (r *tryRecorder) TryAcquire(_ context.Context, req dispatch.Request) (*dispatch.Lease, context.Context, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tries = append(r.tries, req)
	return nil, nil, dispatch.ErrWouldBlock
}

// TestAdmissionTryAcquireWouldBlock is the masterplan gate (b): on a target
// whose external partition is full, TryAcquire answers ErrWouldBlock at once
// (well under the 50 ms "immediately" bound, Index Q19), queues nothing, is no
// rejection (the 429 mapping is the planner's), and arrives in the
// AdmissionError hull with the K9 payload.
func TestAdmissionTryAcquireWouldBlock(t *testing.T) {
	const immediately = 50 * time.Millisecond
	d := partitionDispatcher(t)
	for _, k := range []string{"k0", "k1"} {
		l, _, err := d.Acquire(consumerCtx(k), dispatch.Request{Target: dispatch.Target{Origin: doorOrigin}, Class: dispatch.ClassExternal, Role: "embed"})
		if err != nil {
			t.Fatalf("filling E: %v", err)
		}
		defer l.Release()
	}
	adm := Admission{Admitter: d, Class: dispatch.ClassExternal}
	b := admissionBackend("gpu", doorOrigin+"/v1")
	start := time.Now()
	lease, runCtx, err := adm.TryAcquire(consumerCtx("k2"), &b, "embed", 30*time.Second, AdmitOptions{MaxWait: 5 * time.Second, Weight: 1})
	elapsed := time.Since(start)
	if !errors.Is(err, dispatch.ErrWouldBlock) || lease != nil || runCtx != nil {
		t.Fatalf("TryAcquire on a full partition: (lease %v, runCtx %v, err %v), want ErrWouldBlock", lease != nil, runCtx != nil, err)
	}
	if elapsed >= immediately {
		t.Fatalf("TryAcquire took %v, want < %v — the non-blocking door never waits", elapsed, immediately)
	}
	if n := externalWaiting(d); n != 0 {
		t.Fatalf("TryAcquire left %d external waiters, want 0", n)
	}
	if dispatch.IsRejection(err) {
		t.Fatal("ErrWouldBlock classified as a rejection — the 429 mapping is A03-W8's")
	}
	var ae *AdmissionError
	if !errors.As(err, &ae) || ae.Backend != "gpu" || ae.Host != doorOrigin+"/v1" {
		t.Fatalf("error %v (%T), want the AdmissionError hull with backend and host", err, err)
	}
}

// TestAdmissionTryAcquireWithoutTryDoor: an admitter without TryAcquire is an
// error BEFORE any acquire — the counting admitter sees no blocking call.
func TestAdmissionTryAcquireWithoutTryDoor(t *testing.T) {
	ca := &countingAdmitter{}
	adm := Admission{Admitter: ca, Class: dispatch.ClassExternal}
	b := admissionBackend("gpu", doorOrigin)
	lease, _, err := adm.TryAcquire(consumerCtx("k1"), &b, "chat", time.Second, AdmitOptions{})
	if n := len(ca.calls()); n != 0 {
		t.Errorf("%d blocking acquire(s) on an admitter without a try door, want 0 (no fallback onto Acquire, L5)", n)
	}
	if !errors.Is(err, ErrNoTryAdmitter) || lease != nil {
		t.Errorf("(lease %v, err %v), want ErrNoTryAdmitter", lease != nil, err)
	}
	if !IsAdmissionError(err) {
		t.Errorf("err %v is not an AdmissionError — the walk must end it like every admission error", err)
	}
	if _, ok := any((*dispatch.Dispatcher)(nil)).(dispatch.TryAdmitter); !ok {
		t.Fatal("*dispatch.Dispatcher does not satisfy dispatch.TryAdmitter")
	}
}

// TestAdmissionDoorsForwardOptions: each door builds the same Request —
// target, class, role, the admission-anchored deadline hint — and forwards
// AdmitOptions; only Continue marks the continuation, only TryAcquire takes
// the non-blocking door, and Acquire stays the option-free form.
func TestAdmissionDoorsForwardOptions(t *testing.T) {
	rec := &tryRecorder{}
	adm := Admission{Admitter: rec, Class: dispatch.ClassExternal}
	b := admissionBackend("gpu", doorOrigin)
	serveBy := time.Now().Add(time.Minute)
	opts := AdmitOptions{MaxWait: 7 * time.Second, ServeBy: serveBy, Weight: 3}
	ctx := consumerCtx("k1")
	_, _, _ = adm.Acquire(ctx, &b, "chat", 11*time.Second)
	_, _, _ = adm.AcquireWith(ctx, &b, "chat", 11*time.Second, opts)
	_, _, errTry := adm.TryAcquire(ctx, &b, "chat", 11*time.Second, opts)
	_, _, _ = adm.Continue(ctx, &b, "chat", 11*time.Second, opts)
	blocking := rec.calls()
	if len(blocking) != 3 || len(rec.tries) != 1 {
		t.Fatalf("blocking calls %d, try calls %d, want 3/1", len(blocking), len(rec.tries))
	}
	want := dispatch.Request{Target: dispatch.Target{Origin: doorOrigin}, Class: dispatch.ClassExternal, Role: "chat", DeadlineIn: 11 * time.Second}
	if blocking[0] != want {
		t.Fatalf("Acquire request %+v, want %+v (no options)", blocking[0], want)
	}
	withOpts := want
	withOpts.MaxWait, withOpts.ServeBy, withOpts.Weight = opts.MaxWait, opts.ServeBy, opts.Weight
	if blocking[1] != withOpts {
		t.Fatalf("AcquireWith request %+v, want %+v", blocking[1], withOpts)
	}
	if rec.tries[0] != withOpts {
		t.Fatalf("TryAcquire request %+v, want %+v", rec.tries[0], withOpts)
	}
	cont := withOpts
	cont.Continuation = true
	if blocking[2] != cont {
		t.Fatalf("Continue request %+v, want %+v (the continuation flag set)", blocking[2], cont)
	}
	var ae *AdmissionError
	if !errors.As(errTry, &ae) || !errors.Is(errTry, dispatch.ErrWouldBlock) || ae.Backend != "gpu" {
		t.Fatalf("TryAcquire error %v, want ErrWouldBlock in the AdmissionError hull", errTry)
	}
	var nilAdm Admission
	for name, door := range map[string]func() error{
		"AcquireWith": func() error { _, _, err := nilAdm.AcquireWith(ctx, &b, "chat", time.Second, opts); return err },
		"TryAcquire":  func() error { _, _, err := nilAdm.TryAcquire(ctx, &b, "chat", time.Second, opts); return err },
		"Continue":    func() error { _, _, err := nilAdm.Continue(ctx, &b, "chat", time.Second, opts); return err },
	} {
		if err := door(); !IsAdmissionError(err) {
			t.Errorf("%s on a zero Admission: %v, want the loud nil-admitter AdmissionError (I-D1)", name, err)
		}
	}
}

// TestAdmissionContinueAtTheHead runs Continue against a live dispatcher: a
// continuation queued behind a full partition is admitted at the next
// release before an ordinary acquire that waited longer.
func TestAdmissionContinueAtTheHead(t *testing.T) {
	d := partitionDispatcher(t)
	req := func() dispatch.Request {
		return dispatch.Request{Target: dispatch.Target{Origin: doorOrigin}, Class: dispatch.ClassExternal, Role: "embed"}
	}
	hold, _, err := d.Acquire(consumerCtx("k0"), req())
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	block1, _, err := d.Acquire(consumerCtx("k1"), req())
	if err != nil {
		t.Fatalf("block 1: %v", err)
	}
	type result struct {
		label string
		lease *dispatch.Lease
		err   error
	}
	ch := make(chan result, 2)
	go func() {
		l, _, err := d.Acquire(consumerCtx("k2"), req())
		ch <- result{"k2", l, err}
	}()
	waitUntil(t, "k2 queued", func() bool { return externalWaiting(d) == 1 })
	adm := Admission{Admitter: d, Class: dispatch.ClassExternal}
	b := admissionBackend("gpu", doorOrigin)
	go func() {
		l, _, err := adm.Continue(consumerCtx("k1"), &b, "embed", 30*time.Second, AdmitOptions{Weight: 1})
		ch <- result{"block2", l, err}
	}()
	waitUntil(t, "continuation queued", func() bool { return externalWaiting(d) == 2 })
	block1.Release() // the next partition release
	var first result
	select {
	case first = <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("no admission after the release")
	}
	if first.label != "block2" || first.err != nil {
		t.Fatalf("first admission %s (err %v), want the continuation", first.label, first.err)
	}
	first.lease.Release()
	second := <-ch
	if second.err != nil {
		t.Fatalf("k2: %v", second.err)
	}
	second.lease.Release()
	hold.Release()
}

// waitUntil polls cond for up to 3 s.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}
