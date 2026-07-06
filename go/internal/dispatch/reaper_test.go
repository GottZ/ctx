package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"
)

func reaperSettings() Settings {
	s := DefaultSettings()
	s.LeaseMaxAge = 30 * time.Millisecond
	s.LeaseReapGrace = 10 * time.Millisecond
	return s
}

// B1 negative probe: a lease WITHOUT a deadline hint and WITHOUT a ctx
// deadline (the embed wire path) is reaped after lease_max_age — without the
// fallback a leaked embed lease would NEVER be reaped, and the reaped
// background lease's wire ctx is canceled with cause ErrReaped within the
// reap tick (no silent over-admission into the llama.cpp defer queue).
func TestReapMaxAgeFallback(t *testing.T) {
	d, _ := newTestDispatcher(t, reaperSettings(), onSlotPolicy(1))
	_, runCtx, err := d.Acquire(context.Background(), backgroundReq())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Deliberately NO Release — the leak case.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(withPrincipal(ctx, principal("a")), d, "next", interactiveReq(), ch)
	waitFor(t, "next acquire queued behind the leak", func() bool { return waitingInteractive(d) == 1 })

	time.Sleep(50 * time.Millisecond) // past max_age + grace
	d.reapNow(time.Now())

	a := <-ch
	if a.err != nil {
		t.Fatalf("next acquire must come through after the reap: %v", a.err)
	}
	a.lease.Release()
	if runCtx.Err() == nil {
		t.Fatalf("reaped background wire ctx must be canceled")
	}
	if cause := context.Cause(runCtx); !errors.Is(cause, ErrReaped) {
		t.Fatalf("cancel cause: got %v want ErrReaped", cause)
	}
	if !errors.Is(context.Cause(runCtx), context.Canceled) {
		t.Fatalf("ErrReaped must wrap context.Canceled through the chain (K1)")
	}
	if got := d.Snapshot().ReapsTotal; got != 1 {
		t.Fatalf("reap counter: got %d want 1", got)
	}
}

// The reap reference is the EARLIER of the deadline hint and the ctx
// deadline; a lease held past it is force-released after the grace.
func TestReapDeadlineHint(t *testing.T) {
	s := reaperSettings()
	s.LeaseMaxAge = time.Hour // fallback out of the way — the hint must carry
	d, _ := newTestDispatcher(t, s, onSlotPolicy(1))
	req := backgroundReq()
	req.Deadline = time.Now().Add(20 * time.Millisecond)
	_, runCtx, err := d.Acquire(context.Background(), req)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	time.Sleep(40 * time.Millisecond) // past hint + grace
	d.reapNow(time.Now())
	if cause := context.Cause(runCtx); !errors.Is(cause, ErrReaped) {
		t.Fatalf("hint-based reap: cause got %v want ErrReaped", cause)
	}
	if ts := target(t, d); ts.Held != 0 {
		t.Fatalf("slot not freed by the reap: %+v", ts)
	}
}

// R2 cancel-before-release: the caller's ctx dies (wire call aborted) but
// Release is never called — the reaper must still free the slot, or the
// target stays closed forever.
func TestReapCancelBeforeRelease(t *testing.T) {
	s := reaperSettings()
	s.LeaseMaxAge = time.Hour
	d, _ := newTestDispatcher(t, s, onSlotPolicy(1))
	callerCtx, callerCancel := context.WithCancel(context.Background())
	req := backgroundReq()
	req.Deadline = time.Now().Add(20 * time.Millisecond)
	_, runCtx, err := d.Acquire(callerCtx, req)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	callerCancel() // wire call aborts through ctx inheritance …
	<-runCtx.Done()
	if ts := target(t, d); ts.Held != 1 {
		t.Fatalf("premise: the canceled-but-unreleased lease still holds the slot: %+v", ts)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(withPrincipal(ctx, principal("a")), d, "next", interactiveReq(), ch)
	waitFor(t, "next acquire queued", func() bool { return waitingInteractive(d) == 1 })

	time.Sleep(40 * time.Millisecond)
	d.reapNow(time.Now())
	a := <-ch
	if a.err != nil {
		t.Fatalf("slot not recovered after cancel-before-release: %v", a.err)
	}
	a.lease.Release()
}

// I-D1: the reaper never cancels an interactive lease — its over-admission
// window is an accepted degradation toward today's behavior; the slot is
// freed and the divergence metric (reap counter + ERROR log) alarms.
func TestReapInteractiveFreesSlotWithoutCancel(t *testing.T) {
	d, _ := newTestDispatcher(t, reaperSettings(), onSlotPolicy(1))
	_, runCtx, err := d.Acquire(withPrincipal(context.Background(), principal("a")), interactiveReq())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	d.reapNow(time.Now())
	if ts := target(t, d); ts.Held != 0 {
		t.Fatalf("slot must be freed: %+v", ts)
	}
	if runCtx.Err() != nil {
		t.Fatalf("interactive wire ctx must NEVER be canceled by the dispatcher")
	}
}

// Behavior neutrality: a pass-through lease (no declared policy) is evicted
// from the telemetry registry when leaked, but its wire ctx is NOT canceled —
// before activation the dispatcher must not touch any call.
func TestReapPassthroughEvictsWithoutCancel(t *testing.T) {
	d, _ := newTestDispatcher(t, reaperSettings(), Policy{})
	_, runCtx, err := d.Acquire(context.Background(), backgroundReq())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	d.reapNow(time.Now())
	if ts := target(t, d); ts.Inflight != 0 {
		t.Fatalf("leaked pass-through lease must be evicted from the registry: %+v", ts)
	}
	if runCtx.Err() != nil {
		t.Fatalf("pass-through wire ctx must not be canceled (behavior neutrality)")
	}
}

// A healthy lease inside its reap reference is never touched.
func TestReapLeavesHealthyLeasesAlone(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))
	l, runCtx, err := d.Acquire(context.Background(), backgroundReq())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	d.reapNow(time.Now())
	if runCtx.Err() != nil || d.Snapshot().ReapsTotal != 0 {
		t.Fatalf("healthy lease was reaped")
	}
	l.Release()
}
