package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingHandler captures slog output for WARN/ERROR assertions.
type recordingHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.msgs = append(h.msgs, r.Message)
	h.mu.Unlock()
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) contains(sub string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

func (h *recordingHandler) count(sub string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, m := range h.msgs {
		if strings.Contains(m, sub) {
			n++
		}
	}
	return n
}

const testOrigin = "http://gpu:8089"

func newTestDispatcher(t *testing.T, s Settings, p Policy) (*Dispatcher, *recordingHandler) {
	t.Helper()
	h := &recordingHandler{}
	d := New(slog.New(h), s)
	d.UpdatePolicy(p)
	t.Cleanup(d.Close)
	return d, h
}

func onSlotPolicy(slots int) Policy {
	return Policy{Targets: map[string]TargetPolicy{testOrigin: {Slots: slots}}}
}

func waitFor(t *testing.T, what string, cond func() bool) {
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

func principal(n string) Principal {
	return Principal{ApiKeyID: "key-" + n, TenantID: "tenant-" + n, HomeScope: "scope-" + n}
}

func interactiveReq(p Principal) Request {
	return Request{Target: Target{Origin: testOrigin}, Class: ClassInteractive, Role: "test", Principal: p}
}

func backgroundReq() Request {
	return Request{Target: Target{Origin: testOrigin}, Class: ClassBackground, Role: "test"}
}

// target returns the snapshot row of the test origin.
func target(t *testing.T, d *Dispatcher) TargetSnapshot {
	t.Helper()
	for _, ts := range d.Snapshot().Targets {
		if ts.Origin == testOrigin {
			return ts
		}
	}
	return TargetSnapshot{}
}

func waitingInteractive(d *Dispatcher) int {
	for _, ts := range d.Snapshot().Targets {
		if ts.Origin == testOrigin {
			return ts.Interactive.Waiting
		}
	}
	return 0
}

func waitingBackground(d *Dispatcher) int {
	for _, ts := range d.Snapshot().Targets {
		if ts.Origin == testOrigin {
			return ts.Background.Waiting
		}
	}
	return 0
}

// admission is what a waiter goroutine reports once admitted.
type admission struct {
	label string
	lease *Lease
	err   error
}

// startWaiter runs one Acquire in a goroutine and reports on ch.
func startWaiter(ctx context.Context, d *Dispatcher, label string, req Request, ch chan admission) {
	go func() {
		l, _, err := d.Acquire(ctx, req)
		ch <- admission{label: label, lease: l, err: err}
	}()
}

// --- pass-through / behavior neutrality.

func TestPassthroughWithoutPolicy(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), Policy{})
	l, runCtx, err := d.Acquire(context.Background(), backgroundReq())
	if err != nil {
		t.Fatalf("pass-through acquire failed: %v", err)
	}
	if runCtx.Err() != nil {
		t.Fatalf("runCtx must be live")
	}
	ts := target(t, d)
	if ts.Inflight != 1 || ts.Held != 0 {
		t.Fatalf("pass-through must track inflight without holding a slot: %+v", ts)
	}
	l.Release()
	if ts := target(t, d); ts.Inflight != 0 {
		t.Fatalf("release must clear the registry: %+v", ts)
	}
}

func TestPassthroughIgnoresHerald(t *testing.T) {
	// Behavior neutrality: without a declared policy even demand > 0 must
	// not delay background — exactly today's behavior.
	d, _ := newTestDispatcher(t, DefaultSettings(), Policy{})
	done := d.InteractiveArrived()
	defer done()
	l, _, err := d.Acquire(context.Background(), backgroundReq())
	if err != nil {
		t.Fatalf("pass-through acquire failed under demand: %v", err)
	}
	l.Release()
}

// --- class order, FIFO, no barging.

func TestClassOrderInteractiveBeforeBackground(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))
	occ, _, err := d.Acquire(context.Background(), interactiveReq(principal("occ")))
	if err != nil {
		t.Fatalf("occupier: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 2)
	// Background enqueues FIRST, interactive second — release must still
	// admit interactive first (strict class order).
	startWaiter(ctx, d, "bg", backgroundReq(), ch)
	waitFor(t, "bg waiter queued", func() bool { return waitingBackground(d) == 1 })
	startWaiter(ctx, d, "ia", interactiveReq(principal("a")), ch)
	waitFor(t, "ia waiter queued", func() bool { return waitingInteractive(d) == 1 })

	occ.Release()
	first := <-ch
	if first.err != nil || first.label != "ia" {
		t.Fatalf("interactive must be admitted before background, got %q err=%v", first.label, first.err)
	}
	first.lease.Release()
	second := <-ch
	if second.err != nil || second.label != "bg" {
		t.Fatalf("background must follow, got %q err=%v", second.label, second.err)
	}
	second.lease.Release()
}

func TestNoBargingSameClass(t *testing.T) {
	// A newcomer never overtakes a waiter of its own or a higher class of
	// the SAME bucket; with a non-empty waitQ the slot is never "free" for
	// newcomers (direct handoff).
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))
	p := principal("same") // one bucket: same principal for all waiters
	occ, _, err := d.Acquire(context.Background(), interactiveReq(p))
	if err != nil {
		t.Fatalf("occupier: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 2)
	startWaiter(ctx, d, "w1", interactiveReq(p), ch)
	waitFor(t, "w1 queued", func() bool { return waitingInteractive(d) == 1 })
	occ.Release() // direct handoff to w1 under the mutex
	first := <-ch
	if first.label != "w1" || first.err != nil {
		t.Fatalf("expected w1 admitted, got %q err=%v", first.label, first.err)
	}
	// The newcomer arrives while w2 is queued — it must land BEHIND w2.
	startWaiter(ctx, d, "w2", interactiveReq(p), ch)
	waitFor(t, "w2 queued", func() bool { return waitingInteractive(d) == 1 })
	startWaiter(ctx, d, "w3", interactiveReq(p), ch)
	waitFor(t, "w3 queued", func() bool { return waitingInteractive(d) == 2 })
	first.lease.Release()
	second := <-ch
	if second.label != "w2" || second.err != nil {
		t.Fatalf("newcomer barged: expected w2, got %q err=%v", second.label, second.err)
	}
	second.lease.Release()
	third := <-ch
	if third.label != "w3" || third.err != nil {
		t.Fatalf("expected w3 last, got %q err=%v", third.label, third.err)
	}
	third.lease.Release()
}

// --- herald term (R3).

func TestHeraldTermBlocksBackground(t *testing.T) {
	// demand > 0 ⇒ background waits despite a free slot; after done() it is
	// admitted (the flip-wakes-waitQ guarantee). R3: background must not
	// start into the inter-lease gaps of a running interactive request.
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))
	done := d.InteractiveArrived()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(ctx, d, "bg", backgroundReq(), ch)
	waitFor(t, "bg queued behind herald", func() bool { return waitingBackground(d) == 1 })
	if ts := target(t, d); ts.Held != 0 {
		t.Fatalf("slot must stay free while background waits: %+v", ts)
	}
	select {
	case a := <-ch:
		t.Fatalf("background admitted despite demand > 0: %+v", a)
	case <-time.After(50 * time.Millisecond):
	}
	done()
	a := <-ch
	if a.err != nil {
		t.Fatalf("background not admitted after demand dropped: %v", a.err)
	}
	a.lease.Release()
}

func TestHeraldDoneIdempotent(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), Policy{})
	done1 := d.InteractiveArrived()
	done2 := d.InteractiveArrived()
	done1()
	done1() // double call must not double-decrement
	if got := d.InteractiveDemand(); got != 1 {
		t.Fatalf("demand after idempotent done: got %d want 1", got)
	}
	done2()
	if got := d.InteractiveDemand(); got != 0 {
		t.Fatalf("demand after all done: got %d want 0", got)
	}
}

// --- class authorization (B8).

func TestInteractiveWithoutPrincipalDowngrades(t *testing.T) {
	// Acquire(ClassInteractive, Principal{}) is enqueued as BACKGROUND +
	// slog-ERROR + counter (fail-closed toward the protected good).
	d, h := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))
	done := d.InteractiveArrived() // herald blocks background ⇒ proves the class
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(ctx, d, "dg", Request{Target: Target{Origin: testOrigin}, Class: ClassInteractive}, ch)
	waitFor(t, "downgraded acquire queued as background", func() bool { return waitingBackground(d) == 1 })
	if waitingInteractive(d) != 0 {
		t.Fatalf("downgraded acquire must not sit in the interactive queue")
	}
	if !h.contains("downgraded to background") {
		t.Fatalf("expected slog-ERROR for the downgrade")
	}
	if got := d.Snapshot().ClassDowngrades; got != 1 {
		t.Fatalf("downgrade counter: got %d want 1", got)
	}
	done()
	a := <-ch
	if a.err != nil {
		t.Fatalf("downgraded acquire must still be served: %v", a.err)
	}
	if a.lease.Class() != ClassBackground {
		t.Fatalf("lease class: got %v want background", a.lease.Class())
	}
	a.lease.Release()
}

func TestRegularBackgroundDoesNotCountAsDowngrade(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), Policy{})
	l, _, err := d.Acquire(context.Background(), backgroundReq())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	l.Release()
	if got := d.Snapshot().ClassDowngrades; got != 0 {
		t.Fatalf("regular background must not increment class_downgrades: %d", got)
	}
}

// --- Deckel-Staffel (D3-U4 + E-U6).

func staffelSettings() Settings {
	s := DefaultSettings()
	s.InteractiveQueuePerPrincipal = 2
	s.InteractiveQueuePerTenant = 3
	s.InteractiveQueueMax = 4
	return s
}

func TestStaffelPrincipalCap(t *testing.T) {
	d, _ := newTestDispatcher(t, staffelSettings(), onSlotPolicy(1))
	occ, _, err := d.Acquire(context.Background(), interactiveReq(principal("occ")))
	if err != nil {
		t.Fatalf("occupier: %v", err)
	}
	defer occ.Release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 4)
	flooder := principal("flood")
	startWaiter(ctx, d, "f1", interactiveReq(flooder), ch)
	startWaiter(ctx, d, "f2", interactiveReq(flooder), ch)
	waitFor(t, "two flooder waiters", func() bool { return waitingInteractive(d) == 2 })

	if _, _, err := d.Acquire(ctx, interactiveReq(flooder)); !errors.Is(err, ErrPrincipalSaturated) {
		t.Fatalf("3rd waiter of one key: got %v want ErrPrincipalSaturated", err)
	}
	// Foreign principals stay unaffected.
	startWaiter(ctx, d, "other", interactiveReq(principal("other")), ch)
	waitFor(t, "foreign principal still admitted to the queue", func() bool { return waitingInteractive(d) == 3 })
}

func TestStaffelTenantCap(t *testing.T) {
	// N keys of the SAME tenant cannot multiply the principal cap (C8).
	d, _ := newTestDispatcher(t, staffelSettings(), onSlotPolicy(1))
	occ, _, err := d.Acquire(context.Background(), interactiveReq(principal("occ")))
	if err != nil {
		t.Fatalf("occupier: %v", err)
	}
	defer occ.Release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 4)
	mk := func(key string) Principal {
		return Principal{ApiKeyID: key, TenantID: "tenant-one", HomeScope: "scope-one"}
	}
	startWaiter(ctx, d, "a1", interactiveReq(mk("key-a")), ch)
	startWaiter(ctx, d, "a2", interactiveReq(mk("key-a")), ch)
	startWaiter(ctx, d, "b1", interactiveReq(mk("key-b")), ch)
	waitFor(t, "three tenant waiters", func() bool { return waitingInteractive(d) == 3 })

	// key-c is below its principal cap, but the tenant is saturated.
	if _, _, err := d.Acquire(ctx, interactiveReq(mk("key-c"))); !errors.Is(err, ErrTenantSaturated) {
		t.Fatalf("4th waiter of one tenant: got %v want ErrTenantSaturated", err)
	}
	// A foreign tenant is unaffected.
	startWaiter(ctx, d, "x", interactiveReq(principal("x")), ch)
	waitFor(t, "foreign tenant still admitted to the queue", func() bool { return waitingInteractive(d) == 4 })
}

func TestStaffelAggregateCap(t *testing.T) {
	d, _ := newTestDispatcher(t, staffelSettings(), onSlotPolicy(1))
	occ, _, err := d.Acquire(context.Background(), interactiveReq(principal("occ")))
	if err != nil {
		t.Fatalf("occupier: %v", err)
	}
	defer occ.Release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 8)
	for i := 0; i < 4; i++ {
		startWaiter(ctx, d, fmt.Sprintf("w%d", i), interactiveReq(principal(fmt.Sprintf("t%d", i))), ch)
	}
	waitFor(t, "aggregate at cap", func() bool { return waitingInteractive(d) == 4 })
	if _, _, err := d.Acquire(ctx, interactiveReq(principal("t9"))); !errors.Is(err, ErrTargetSaturated) {
		t.Fatalf("65th-equivalent waiter: got %v want ErrTargetSaturated", err)
	}
	// The background queue is unaffected by the interactive aggregate cap.
	startWaiter(ctx, d, "bg", backgroundReq(), ch)
	waitFor(t, "background still queueable", func() bool { return waitingBackground(d) == 1 })
}

func TestStaffelCheckOrderPrincipalFirst(t *testing.T) {
	// A request violating principal AND aggregate must see the PRINCIPAL
	// sentinel — the most specific offender first.
	s := staffelSettings()
	s.InteractiveQueueMax = 2
	d, _ := newTestDispatcher(t, s, onSlotPolicy(1))
	occ, _, err := d.Acquire(context.Background(), interactiveReq(principal("occ")))
	if err != nil {
		t.Fatalf("occupier: %v", err)
	}
	defer occ.Release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 2)
	flooder := principal("flood")
	startWaiter(ctx, d, "f1", interactiveReq(flooder), ch)
	startWaiter(ctx, d, "f2", interactiveReq(flooder), ch)
	waitFor(t, "queue at aggregate cap", func() bool { return waitingInteractive(d) == 2 })
	if _, _, err := d.Acquire(ctx, interactiveReq(flooder)); !errors.Is(err, ErrPrincipalSaturated) {
		t.Fatalf("check order: got %v want ErrPrincipalSaturated before ErrTargetSaturated", err)
	}
}

func TestBackgroundQueueFull(t *testing.T) {
	s := DefaultSettings()
	s.BackgroundQueueMax = 2
	d, _ := newTestDispatcher(t, s, onSlotPolicy(1))
	occ, _, err := d.Acquire(context.Background(), backgroundReq())
	if err != nil {
		t.Fatalf("occupier: %v", err)
	}
	defer occ.Release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 2)
	startWaiter(ctx, d, "b1", backgroundReq(), ch)
	startWaiter(ctx, d, "b2", backgroundReq(), ch)
	waitFor(t, "background queue full", func() bool { return waitingBackground(d) == 2 })
	if _, _, err := d.Acquire(ctx, backgroundReq()); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("over-cap background: got %v want ErrQueueFull", err)
	}
}

// --- rejection typology.

func TestIsRejection(t *testing.T) {
	for _, err := range []error{ErrQueueFull, ErrPrincipalSaturated, ErrTenantSaturated, ErrTargetSaturated} {
		if !IsRejection(err) {
			t.Errorf("IsRejection(%v) = false, want true", err)
		}
		if !IsRejection(fmt.Errorf("wrapped: %w", err)) {
			t.Errorf("IsRejection(wrapped %v) = false, want true", err)
		}
	}
	for _, err := range []error{nil, context.Canceled, context.DeadlineExceeded, ErrPreempted, ErrReaped, errors.New("dispatch: background queue full")} {
		if IsRejection(err) {
			t.Errorf("IsRejection(%v) = true, want false", err)
		}
	}
}

// --- origin normalization.

func TestNormalizeOrigin(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://HOST:8089/v1", "http://host:8089"},
		{"http://host:8089", "http://host:8089"},
		{"https://Example.COM/path?q=1#f", "https://example.com:443"},
		{"http://example.com", "http://example.com:80"},
		{"http://10.13.37.11:8081/v1/", "http://10.13.37.11:8081"},
	}
	for _, c := range cases {
		got, err := NormalizeOrigin(c.in)
		if err != nil || got != c.want {
			t.Errorf("NormalizeOrigin(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "not a url", "/relative/only"} {
		if _, err := NormalizeOrigin(bad); err == nil {
			t.Errorf("NormalizeOrigin(%q) succeeded, want error", bad)
		}
	}
}

func TestOriginSpellingsShareOneTargetState(t *testing.T) {
	// Two spellings of the same physical target ⇒ ONE targetState (the
	// slots cap must not be bypassable by string drift); unequal ⇒ two.
	d, _ := newTestDispatcher(t, DefaultSettings(), Policy{
		Targets: map[string]TargetPolicy{"http://gpu:8089": {Slots: 1}},
	})
	l1, _, err := d.Acquire(context.Background(), Request{Target: Target{Origin: "http://GPU:8089/v1"}, Class: ClassBackground})
	if err != nil {
		t.Fatalf("first spelling: %v", err)
	}
	defer l1.Release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(ctx, d, "w", Request{Target: Target{Origin: "http://gpu:8089"}, Class: ClassBackground}, ch)
	waitFor(t, "second spelling queued behind the same slot", func() bool { return waitingBackground(d) == 1 })
	snap := d.Snapshot()
	if len(snap.Targets) != 1 {
		t.Fatalf("expected ONE targetState, got %d: %+v", len(snap.Targets), snap.Targets)
	}
	l2, _, err := d.Acquire(context.Background(), Request{Target: Target{Origin: "http://gpu:9999"}, Class: ClassBackground})
	if err != nil {
		t.Fatalf("distinct origin: %v", err)
	}
	l2.Release()
	if got := len(d.Snapshot().Targets); got != 2 {
		t.Fatalf("distinct origins must stay distinct: got %d targets", got)
	}
}

// --- emergency stop + policy return path.

func TestEnabledFalseDrainsWaitersAsPassthrough(t *testing.T) {
	// B2: a blocked targetState + enabled=false ⇒ the wire call runs.
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))
	occ, _, err := d.Acquire(context.Background(), interactiveReq(principal("occ")))
	if err != nil {
		t.Fatalf("occupier: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 2)
	startWaiter(ctx, d, "ia", interactiveReq(principal("a")), ch)
	startWaiter(ctx, d, "bg", backgroundReq(), ch)
	waitFor(t, "both waiters queued", func() bool {
		return waitingInteractive(d) == 1 && waitingBackground(d) == 1
	})
	s := DefaultSettings()
	s.Enabled = false
	d.UpdateSettings(s)
	for i := 0; i < 2; i++ {
		a := <-ch
		if a.err != nil {
			t.Fatalf("waiter %q not drained on emergency stop: %v", a.label, a.err)
		}
		a.lease.Release()
	}
	// New acquires pass immediately while disabled.
	l, _, err := d.Acquire(context.Background(), interactiveReq(principal("b")))
	if err != nil {
		t.Fatalf("acquire while disabled: %v", err)
	}
	l.Release()
	occ.Release()
}

func TestPolicyClearDrainsWaiters(t *testing.T) {
	// The W7 return path: limits='{}' ⇒ pass-through within NOTIFY latency.
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))
	occ, _, err := d.Acquire(context.Background(), backgroundReq())
	if err != nil {
		t.Fatalf("occupier: %v", err)
	}
	defer occ.Release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(ctx, d, "bg", backgroundReq(), ch)
	waitFor(t, "waiter queued", func() bool { return waitingBackground(d) == 1 })
	d.UpdatePolicy(Policy{})
	a := <-ch
	if a.err != nil {
		t.Fatalf("waiter not drained on policy clear: %v", a.err)
	}
	a.lease.Release()
}

// --- lease lifecycle.

func TestReleaseIdempotent(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(2))
	l1, _, err := d.Acquire(context.Background(), interactiveReq(principal("a")))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	l2, _, err := d.Acquire(context.Background(), interactiveReq(principal("b")))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	l1.Release()
	l1.Release() // no-op
	if ts := target(t, d); ts.Held != 1 {
		t.Fatalf("double release must not double-decrement: held=%d want 1", ts.Held)
	}
	l2.Release()
	if ts := target(t, d); ts.Held != 0 || ts.Inflight != 0 {
		t.Fatalf("final state: %+v", ts)
	}
}

func TestAcquireWithCanceledCtx(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := d.Acquire(ctx, interactiveReq(principal("a"))); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ctx: got %v want context.Canceled", err)
	}
	ts := target(t, d)
	if ts.Interactive.Waiting != 0 || ts.Inflight != 0 {
		t.Fatalf("canceled acquire leaked state: %+v", ts)
	}
}

func TestWaiterCancelVacatesQueueSlot(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))
	occ, _, err := d.Acquire(context.Background(), interactiveReq(principal("occ")))
	if err != nil {
		t.Fatalf("occupier: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan admission, 1)
	startWaiter(ctx, d, "w", interactiveReq(principal("a")), ch)
	waitFor(t, "waiter queued", func() bool { return waitingInteractive(d) == 1 })
	cancel()
	a := <-ch
	if !errors.Is(a.err, context.Canceled) {
		t.Fatalf("canceled waiter: got %v want context.Canceled", a.err)
	}
	if got := waitingInteractive(d); got != 0 {
		t.Fatalf("canceled waiter left a queue entry: %d", got)
	}
	occ.Release()
	if ts := target(t, d); ts.Held != 0 {
		t.Fatalf("slot not free after release: %+v", ts)
	}
}

// --- fairness modes (E-F2 structure).

// drainOrder releases the occupier and then drains the admitted waiters in
// order, returning their labels.
func drainOrder(t *testing.T, occ *Lease, ch chan admission, n int) []string {
	t.Helper()
	occ.Release()
	var order []string
	for i := 0; i < n; i++ {
		a := <-ch
		if a.err != nil {
			t.Fatalf("waiter %q failed: %v", a.label, a.err)
		}
		order = append(order, a.label)
		a.lease.Release()
	}
	return order
}

func fairnessFixture(t *testing.T, mode FairnessMode) []string {
	t.Helper()
	s := DefaultSettings()
	s.Fairness = mode
	d, _ := newTestDispatcher(t, s, onSlotPolicy(1))
	occ, _, err := d.Acquire(context.Background(), interactiveReq(principal("occ")))
	if err != nil {
		t.Fatalf("occupier: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scopeA := Principal{ApiKeyID: "key-a", TenantID: "ta", HomeScope: "bucket-a"}
	scopeB := Principal{ApiKeyID: "key-b", TenantID: "tb", HomeScope: "bucket-b"}
	ch := make(chan admission, 3)
	// Arrival order: a1, a2, b1 — fifo keeps it; principal_rr alternates
	// buckets (a1, b1, a2) while intra-bucket FIFO holds (a1 before a2).
	startWaiter(ctx, d, "a1", interactiveReq(scopeA), ch)
	waitFor(t, "a1 queued", func() bool { return waitingInteractive(d) == 1 })
	startWaiter(ctx, d, "a2", interactiveReq(scopeA), ch)
	waitFor(t, "a2 queued", func() bool { return waitingInteractive(d) == 2 })
	startWaiter(ctx, d, "b1", interactiveReq(scopeB), ch)
	waitFor(t, "b1 queued", func() bool { return waitingInteractive(d) == 3 })
	return drainOrder(t, occ, ch, 3)
}

func TestFairnessFIFOKeepsArrivalOrder(t *testing.T) {
	got := fairnessFixture(t, FairnessFIFO)
	want := []string{"a1", "a2", "b1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fifo order: got %v want %v", got, want)
		}
	}
}

func TestFairnessPrincipalRRAlternatesBuckets(t *testing.T) {
	got := fairnessFixture(t, FairnessPrincipalRR)
	want := []string{"a1", "b1", "a2"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("principal_rr order: got %v want %v", got, want)
		}
	}
}

// --- W1 visibility.

func TestInteractiveWaitWarn(t *testing.T) {
	d, h := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))
	d.warnAfter = time.Millisecond // per-instance, race-free
	occ, _, err := d.Acquire(context.Background(), interactiveReq(principal("occ")))
	if err != nil {
		t.Fatalf("occupier: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(ctx, d, "w", interactiveReq(principal("a")), ch)
	waitFor(t, "waiter queued", func() bool { return waitingInteractive(d) == 1 })
	time.Sleep(10 * time.Millisecond)
	occ.Release()
	a := <-ch
	if a.err != nil {
		t.Fatalf("waiter: %v", a.err)
	}
	a.lease.Release()
	if !h.contains("interactive wait above threshold") {
		t.Fatalf("expected the W1 wait WARN")
	}
	if a.lease.WaitDur() < 10*time.Millisecond {
		t.Fatalf("WaitDur must measure enqueue→admission, got %v", a.lease.WaitDur())
	}
}

func TestDefaultSettings(t *testing.T) {
	s := DefaultSettings()
	if !s.Enabled || s.BackgroundQueueMax != 32 ||
		s.InteractiveQueuePerPrincipal != 8 || s.InteractiveQueuePerTenant != 16 ||
		s.InteractiveQueueMax != 64 ||
		s.LeaseReapGrace != 30*time.Second || s.LeaseMaxAge != 900*time.Second ||
		s.PreemptReleaseTimeout != 2*time.Second ||
		s.Fairness != FairnessFIFO ||
		s.BackgroundAgingAfter != 0 { // MW25: aging escape off by default (E-F5/K6)
		t.Fatalf("defaults drifted from the registry contract: %+v", s)
	}
}

// TestDeadlineInAnchorsAtAdmission pins the MW3 half of the V1 contract on
// the dispatch side (Request.DeadlineIn): a waiter that spent time in the
// queue gets its reap reference anchored at ADMISSION+DeadlineIn, not at
// enqueue+DeadlineIn — an enqueue-anchored hint would underestimate the reap
// reference by the wait time and let the reaper cancel a legitimately
// running wire call.
func TestDeadlineInAnchorsAtAdmission(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))

	// Occupy the single slot.
	first, _, err := d.Acquire(context.Background(), interactiveReq(principal("a")))
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	const hint = time.Minute
	enqueue := time.Now()
	type res struct {
		lease *Lease
		err   error
	}
	ch := make(chan res, 1)
	go func() {
		req := interactiveReq(principal("b"))
		req.DeadlineIn = hint
		l, _, err := d.Acquire(context.Background(), req)
		ch <- res{l, err}
	}()
	waitFor(t, "second acquire queued", func() bool { return target(t, d).Interactive.Waiting == 1 })

	// Hold the slot long enough that enqueue-anchored and admission-anchored
	// references are clearly distinguishable.
	const wait = 120 * time.Millisecond
	time.Sleep(wait)
	first.Release()

	r := <-ch
	if r.err != nil {
		t.Fatalf("second acquire: %v", r.err)
	}
	defer r.lease.Release()
	if got := r.lease.reapRef; got.Before(r.lease.admitted.Add(hint)) {
		t.Fatalf("reap reference anchored before admission+hint: ref=%v admitted=%v (enqueue-anchored would be %v)",
			got, r.lease.admitted, enqueue.Add(hint))
	}
	if waited := r.lease.WaitDur(); waited < wait/2 {
		t.Fatalf("probe vacuous: waiter did not actually wait (waited %v)", waited)
	}
}
