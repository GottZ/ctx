package dispatch

// MW9 probes for the non-blocking TryAcquire (Q-I3, design/03 §4.4): the
// eligible-now path shares Acquire's predicate and bookkeeping, everything
// else answers ErrWouldBlock immediately — no waiter, no wait slot, no
// blocking. Pass-through stays unconditional (behavior neutrality with
// empty policy, D3-W5 gate).

import (
	"context"
	"errors"
	"testing"
	"time"
)

// tryNoBlockBudget bounds "immediately": generous against scheduler jitter,
// far below any plausible admission wait.
const tryNoBlockBudget = 500 * time.Millisecond

func bgReq() Request {
	return Request{Target: Target{Origin: testOrigin}, Class: ClassBackground, Role: "embed"}
}

// TestTryAcquireEligibleNowAdmits pins the happy path: a free declared slot
// admits exactly like Acquire (slot held), and the same slot answers a
// second try with ErrWouldBlock until the lease is released.
func TestTryAcquireEligibleNowAdmits(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))

	l, runCtx, err := d.TryAcquire(context.Background(), bgReq())
	if err != nil || l == nil || runCtx == nil {
		t.Fatalf("TryAcquire on free slot = (%v, %v, %v), want admitted lease", l, runCtx, err)
	}
	if l.Class() != ClassBackground {
		t.Fatalf("lease class = %v, want background", l.Class())
	}

	if _, _, err := d.TryAcquire(context.Background(), bgReq()); !errors.Is(err, ErrWouldBlock) {
		t.Fatalf("TryAcquire on held slot = %v, want ErrWouldBlock", err)
	}

	l.Release()
	l2, _, err := d.TryAcquire(context.Background(), bgReq())
	if err != nil || l2 == nil {
		t.Fatalf("TryAcquire after release = (%v, %v), want admitted lease", l2, err)
	}
	l2.Release()
}

// TestTryAcquireBusyTargetNoBlockNoWaiter is the core Q-I3 semantics gate:
// a busy target answers immediately (bounded wall time) and enqueues NO
// waiter — after the holder releases, the slot is free instead of handed to
// a phantom queue entry, and a fresh try admits.
func TestTryAcquireBusyTargetNoBlockNoWaiter(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))

	holder, _, err := d.Acquire(context.Background(), bgReq())
	if err != nil {
		t.Fatalf("holder acquire: %v", err)
	}

	start := time.Now()
	_, _, err = d.TryAcquire(context.Background(), bgReq())
	if elapsed := time.Since(start); elapsed > tryNoBlockBudget {
		t.Fatalf("TryAcquire took %v, want < %v (non-blocking)", elapsed, tryNoBlockBudget)
	}
	if !errors.Is(err, ErrWouldBlock) {
		t.Fatalf("TryAcquire on busy target = %v, want ErrWouldBlock", err)
	}
	if IsRejection(err) {
		t.Fatal("ErrWouldBlock must not classify as client rejection (no 429 mapping)")
	}

	holder.Release()
	// No phantom waiter may have consumed the freed slot: a fresh
	// blocking Acquire admits without waiting.
	admitted := make(chan *Lease, 1)
	go func() {
		l, _, err := d.Acquire(context.Background(), bgReq())
		if err != nil {
			t.Errorf("post-release acquire: %v", err)
		}
		admitted <- l
	}()
	select {
	case l := <-admitted:
		if l != nil {
			l.Release()
		}
	case <-time.After(3 * time.Second):
		t.Fatal("slot not free after holder release — TryAcquire left a phantom waiter")
	}
}

// TestTryAcquirePassThroughEmptyPolicy pins behavior neutrality (D3-W5):
// without a declared policy every try admits unconditionally — never
// ErrWouldBlock, arbitrarily many concurrent pass-through leases.
func TestTryAcquirePassThroughEmptyPolicy(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), Policy{})

	var leases []*Lease
	for i := 0; i < 3; i++ {
		l, _, err := d.TryAcquire(context.Background(), bgReq())
		if err != nil || l == nil {
			t.Fatalf("pass-through try %d = (%v, %v), want lease", i, l, err)
		}
		leases = append(leases, l)
	}
	for _, l := range leases {
		l.Release()
	}
}

// TestTryAcquireHeraldDemandGatesBackground pins the R3 herald term on the
// non-blocking door: interactive demand in the house blocks a background
// try even on a free slot — until the demand settles.
func TestTryAcquireHeraldDemandGatesBackground(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))

	done := d.InteractiveArrived()
	if _, _, err := d.TryAcquire(context.Background(), bgReq()); !errors.Is(err, ErrWouldBlock) {
		t.Fatalf("background try under demand = %v, want ErrWouldBlock (herald term)", err)
	}
	done()

	l, _, err := d.TryAcquire(context.Background(), bgReq())
	if err != nil || l == nil {
		t.Fatalf("background try after demand settled = (%v, %v), want lease", l, err)
	}
	l.Release()
}

// TestTryAcquireB8DowngradeWithoutPrincipal pins that the non-blocking door
// enforces the same ctx-bound class authorization as Acquire: interactive
// without principal admits as background (fail-closed, counted).
func TestTryAcquireB8DowngradeWithoutPrincipal(t *testing.T) {
	d, h := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))

	req := bgReq()
	req.Class = ClassInteractive
	l, _, err := d.TryAcquire(context.Background(), req)
	if err != nil || l == nil {
		t.Fatalf("downgraded try = (%v, %v), want lease", l, err)
	}
	defer l.Release()
	if l.Class() != ClassBackground {
		t.Fatalf("lease class = %v, want background (B8 downgrade)", l.Class())
	}
	if got := d.Snapshot().ClassDowngrades; got != 1 {
		t.Fatalf("class_downgrades = %d, want 1", got)
	}
	if !h.contains("downgraded to background") {
		t.Fatal("downgrade must be logged at ERROR")
	}
}

// TestTryAcquireCanceledCtx pins the ctx guard: a canceled context never
// admits and returns the ctx error, not ErrWouldBlock.
func TestTryAcquireCanceledCtx(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), Policy{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := d.TryAcquire(ctx, bgReq()); !errors.Is(err, context.Canceled) {
		t.Fatalf("try on canceled ctx = %v, want context.Canceled", err)
	}
}
