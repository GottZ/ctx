package dispatch

// A03-W1 black-box gate (design/03 §4.1 C7 precondition, inventory/03 §16.1
// K2/K6/K7/K11/K12/K14): a Class value outside the class table is a code bug
// and must never take a wait slot, a lease, another class's predicate or
// another class's queue depth. The file uses ONLY the pre-A03-W1 surface
// (Acquire, TryAcquire, Snapshot, String, RetryAfterHint and the existing
// test seams), so the very same file is red against the base and green after
// the hardening. Class(2) is the first value past the two-row table; when a
// later wave (A03-W3) gives 2 a table row, these probes move to numClasses —
// they fail loudly instead of passing vacuously, because 2 then admits.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// probeClass is the first value past the class table of the base.
const probeClass = Class(2)

func probeReq() Request {
	return Request{Target: Target{Origin: testOrigin}, Class: probeClass, Role: "test"}
}

// TestInvalidClassAcquireLeavesNoPhantom pins K7/K12/K11: an out-of-table
// acquire on a full 1-slot target must be refused at the door. Pre-A03-W1 it
// parks in the interactive queue (K7), its ctx cancel removes from the
// background queue (K12, a no-op), and the next release hands the slot to
// the dead waiter (K11) — a phantom lease until lease_max_age.
func TestInvalidClassAcquireLeavesNoPhantom(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))
	occ, _, err := d.Acquire(withPrincipal(context.Background(), principal("occ")), interactiveReq())
	if err != nil {
		t.Fatalf("occupier: %v", err)
	}

	ctx, cancel := context.WithCancel(withPrincipal(context.Background(), principal("x")))
	defer cancel()
	var returned atomic.Bool
	ch := make(chan admission, 1)
	go func() {
		l, _, err := d.Acquire(ctx, probeReq())
		returned.Store(true)
		ch <- admission{label: "probe", lease: l, err: err}
	}()
	// Either the door refuses at once (fixed) or the waiter parks in a queue.
	waitFor(t, "probe acquire returned or queued", func() bool {
		ts := target(t, d)
		return returned.Load() || ts.Interactive.Waiting+ts.Background.Waiting > 0
	})
	refusedAtDoor := returned.Load()
	cancel()
	a := <-ch

	if !refusedAtDoor {
		t.Errorf("Acquire(Class(%d)) parked as a waiter instead of being refused at the door", probeClass)
	}
	if a.err == nil || a.lease != nil {
		t.Errorf("Acquire(Class(%d)) = (lease %v, err %v), want (nil, error)", probeClass, a.lease != nil, a.err)
	}
	ts := target(t, d)
	if ts.Interactive.Waiting != 0 || ts.Background.Waiting != 0 {
		t.Errorf("after cancel: Interactive.Waiting=%d Background.Waiting=%d, want 0/0 (dead waiter left in a queue)",
			ts.Interactive.Waiting, ts.Background.Waiting)
	}

	occ.Release()
	ts = target(t, d)
	if ts.Held != 0 || ts.Inflight != 0 {
		t.Errorf("after occupier release: Held=%d Inflight=%d, want 0/0 (phantom lease handed to the dead waiter)",
			ts.Held, ts.Inflight)
	}
}

// TestInvalidClassTryAcquireRefused pins K6: the non-blocking door must not
// admit an out-of-table class through the background predicate on a free
// slot-capped target, nor through the pass-through clause.
func TestInvalidClassTryAcquireRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		pol  Policy
	}{
		{"free 1-slot target", onSlotPolicy(1)},
		{"pass-through target", Policy{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newTestDispatcher(t, DefaultSettings(), tc.pol)
			l, runCtx, err := d.TryAcquire(withPrincipal(context.Background(), principal("x")), probeReq())
			if err == nil || l != nil || runCtx != nil {
				t.Errorf("TryAcquire(Class(%d)) = (lease %v, runCtx %v, err %v), want (nil, nil, error)",
					probeClass, l != nil, runCtx != nil, err)
			}
			if l != nil {
				l.Release()
			}
			if ts := target(t, d); ts.Held != 0 || ts.Inflight != 0 || ts.Interactive.Waiting != 0 || ts.Background.Waiting != 0 {
				t.Errorf("after TryAcquire(Class(%d)): Held=%d Inflight=%d waiting=%d/%d, want all 0",
					probeClass, ts.Held, ts.Inflight, ts.Interactive.Waiting, ts.Background.Waiting)
			}
		})
	}
}

// TestInvalidClassAcquirePassThroughRefused: the class check precedes the
// pass-through clause — an empty policy is behavior neutrality for valid
// classes, never an admission path for a code bug.
func TestInvalidClassAcquirePassThroughRefused(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), Policy{})
	l, runCtx, err := d.Acquire(withPrincipal(context.Background(), principal("x")), probeReq())
	if err == nil || l != nil || runCtx != nil {
		t.Errorf("Acquire(Class(%d)) on pass-through = (lease %v, runCtx %v, err %v), want (nil, nil, error)",
			probeClass, l != nil, runCtx != nil, err)
	}
	if l != nil {
		l.Release()
	}
}

// TestInvalidClassStringIsInvalid pins K2: the out-of-table value names
// itself "invalid", never a real class (pairwise distinct telemetry
// vocabulary, design/03 §3.4 / design/06 B-06-12).
func TestInvalidClassStringIsInvalid(t *testing.T) {
	if got := probeClass.String(); got != "invalid" {
		t.Errorf("Class(%d).String() = %q, want %q", probeClass, got, "invalid")
	}
	if got := Class(255).String(); got != "invalid" {
		t.Errorf("Class(255).String() = %q, want %q", got, "invalid")
	}
}

// TestInvalidClassRetryAfterHintZero pins K14: the Retry-After estimate of
// an out-of-table class is 0 (header omitted), never the interactive queue
// depth times the background wait window.
func TestInvalidClassRetryAfterHintZero(t *testing.T) {
	d := New(nil, DefaultSettings())
	defer d.Close()
	d.SeedWaitSampleForTest(testOrigin, ClassInteractive, 4*time.Second)
	d.SeedWaitSampleForTest(testOrigin, ClassBackground, 4*time.Second)
	d.SeedInteractiveDepthForTest(testOrigin, 3)
	if got := d.RetryAfterHint(testOrigin, probeClass); got != 0 {
		t.Errorf("RetryAfterHint(Class(%d)) = %v, want 0", probeClass, got)
	}
}
