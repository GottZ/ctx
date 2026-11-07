package dispatch

// A03-W10 gates on the settle's lifecycle (settle.go): exactly one end per
// lease whoever ends it, and the race/leak probe of the dispatcher half. They
// read the dispatcher's own books (settleTimer, the settling counters), so
// they have no red run against the base — that is settle_test.go's part.

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"testing"
	"time"
)

// TestSettleEndsOnce: a settling lease ends exactly once whoever comes first
// — a second Release is a no-op, the reaper ends it past its reap deadline
// (the timer then finds it done), Close ends it with its timer stopped and a
// release after Close settles nothing. The books never go below zero.
func TestSettleEndsOnce(t *testing.T) {
	t.Run("release twice", func(t *testing.T) {
		d, _ := newTestDispatcher(t, DefaultSettings(), settlePolicy(t, 4, 2, settleMS(0, 200), false))
		ctx, cancel := context.WithCancel(xctx(context.Background(), "c1", "t1"))
		l, runCtx, _ := d.Acquire(ctx, externalReq())
		cancel()
		<-runCtx.Done()
		l.Release()
		l.Release()
		if held, _, w, s := heldNow(t, d); held != 1 || w != 1 || s != 1 {
			t.Fatalf("after two releases: held=%d weight=%d settling=%d, want 1/1/1", held, w, s)
		}
		waitHeldZero(t, d, 2*time.Second)
		l.Release()
		if held, ext, w, s := heldNow(t, d); held != 0 || ext != 0 || w != 0 || s != 0 {
			t.Fatalf("after the settle and a third release: %d/%d/%d/%d, want 0/0/0/0", held, ext, w, s)
		}
	})
	t.Run("reaper first", func(t *testing.T) {
		d, _ := newTestDispatcher(t, DefaultSettings(), settlePolicy(t, 4, 2, settleMS(0, 300), false))
		ctx, cancel := context.WithCancel(xctx(context.Background(), "c1", "t1"))
		l, runCtx, _ := d.Acquire(ctx, externalReq())
		cancel()
		<-runCtx.Done()
		l.Release()
		d.reapNow(time.Now().Add(24 * time.Hour)) // past every reap deadline
		if held, _, w, s := heldNow(t, d); held != 0 || w != 0 || s != 0 {
			t.Fatalf("after the reaper: held=%d weight=%d settling=%d, want 0/0/0", held, w, s)
		}
		time.Sleep(400 * time.Millisecond) // the timer would have fired by now
		if held, _, w, s := heldNow(t, d); held != 0 || w != 0 || s != 0 {
			t.Fatalf("after the timer's due time: held=%d weight=%d settling=%d, want 0/0/0", held, w, s)
		}
		// Fremd-Review 1 F6: a settle the reaper ends is no leak — its caller
		// released the lease; no reap is counted.
		if snap := d.Snapshot(); snap.ReapsTotal != 0 {
			t.Fatalf("reaps %d, want 0 (a settle past the reap deadline is no leak)", snap.ReapsTotal)
		}
	})
	t.Run("close", func(t *testing.T) {
		d := New(slog.New(slog.DiscardHandler), DefaultSettings())
		d.UpdatePolicy(settlePolicy(t, 4, 2, settleMS(0, 30000), false))
		ctx, cancel := context.WithCancel(xctx(context.Background(), "c1", "t1"))
		l, runCtx, _ := d.Acquire(ctx, externalReq())
		cancel()
		<-runCtx.Done()
		l.Release()
		d.mu.Lock()
		timer := l.settleTimer
		d.mu.Unlock()
		if timer == nil {
			t.Fatal("no settle timer after the release of an ended run")
		}
		d.Close()
		if held, _, w, s := heldNow(t, d); held != 0 || w != 0 || s != 0 {
			t.Fatalf("after Close: held=%d weight=%d settling=%d, want 0/0/0", held, w, s)
		}
		if timer.Stop() {
			t.Fatal("the settle timer was still armed after Close")
		}
		// A release after Close settles nothing.
		ctx2, cancel2 := context.WithCancel(xctx(context.Background(), "c1", "t1"))
		l2, run2, err := d.Acquire(ctx2, externalReq())
		if err != nil {
			t.Fatalf("acquire after Close: %v", err)
		}
		cancel2()
		<-run2.Done()
		l2.Release()
		if held, _, _, s := heldNow(t, d); held != 0 || s != 0 {
			t.Fatalf("release after Close: held=%d settling=%d, want 0/0", held, s)
		}
	})
}

// TestSettleRaceAndLeak is the dispatcher half of the race/leak gate: 1 000
// leases of all four classes with a random run end (or none) before a random
// release, settles of 0–20 ms, a reaper pass and a policy swap in between —
// every lease ends exactly once (the books return to zero), no settling lease
// stays, and no goroutine outlives Close.
func TestSettleRaceAndLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	d := New(slog.New(slog.DiscardHandler), DefaultSettings())
	policies := []Policy{
		settlePolicy(t, 64, 16, settleMS(20, 10), false),
		settlePolicy(t, 64, 16, settleMS(0, 0), false),
		settlePolicy(t, 64, 16, settleMS(5, 20), false),
	}
	d.UpdatePolicy(policies[0])
	reqs := []struct {
		ctx func(context.Context) context.Context
		req Request
	}{
		{func(c context.Context) context.Context { return withPrincipal(c, principal("a")) }, interactiveReq()},
		{func(c context.Context) context.Context { return c }, backgroundReq()},
		{func(c context.Context) context.Context { return xctx(c, "c1", "t1") }, externalReq()},
		{func(c context.Context) context.Context { return xctx(c, "c2", "t1") }, withStream(externalReq())},
	}
	done := make(chan struct{}, 1000)
	for i := range 1000 {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			r := reqs[i%len(reqs)]
			ctx, cancel := context.WithCancel(r.ctx(context.Background()))
			defer cancel()
			l, runCtx, err := d.Acquire(ctx, r.req)
			if err != nil {
				return // a cap or a wait budget: no lease, nothing to end
			}
			if i%3 != 0 {
				cancel()
				<-runCtx.Done()
			}
			if i%7 == 0 {
				time.Sleep(time.Duration(i%5) * time.Millisecond)
			}
			l.Release()
			if i%11 == 0 {
				l.Release()
			}
		}(i)
		if i == 500 {
			d.UpdatePolicy(policies[1])
			d.reapNow(time.Now())
		}
		if i == 750 {
			d.UpdatePolicy(policies[2])
		}
	}
	for range 1000 {
		<-done
	}
	waitHeldZero(t, d, 3*time.Second)
	ts := target(t, d)
	if ts.Held != 0 || ts.HeldWeight != 0 || ts.Inflight != 0 || settlingOf(ts) != 0 {
		t.Fatalf("after the run: held=%d weight=%d inflight=%d settling=%d, want 0/0/0/0", ts.Held, ts.HeldWeight, ts.Inflight, settlingOf(ts))
	}
	d.mu.Lock()
	st := d.targets[testOrigin]
	drift := st.held != 0 || st.heldExt != 0 || st.extLeases != 0 || st.settling != 0 || st.settlingWeight != 0 ||
		len(st.cancelable) != 0 || len(st.extCancel) != 0 || st.classHeld != [numClasses]int{}
	d.mu.Unlock()
	if drift {
		t.Fatal("counter drift after every lease ended")
	}
	d.Close()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("goroutines %d after Close, %d before", n, before)
	}
}

// TestSettleTraceSeesASettle is the control of the differential trace
// (settle_trace_test.go): the same seeds with abort_settle_ms 50/50 differ
// from the trace without the key — a settle shows in the books the trace
// compares, so the trace's equality at 0/0 is not blind.
func TestSettleTraceSeesASettle(t *testing.T) {
	differs := 0
	for seed := uint64(1); seed <= 6; seed++ {
		missing, settled := runSettleTrace(t, seed, -1), runSettleTrace(t, seed, 50)
		for i := range missing {
			if missing[i] != settled[i] {
				differs++
				break
			}
		}
	}
	if differs == 0 {
		t.Fatal("no seed's trace differs with abort_settle_ms 50/50 — the trace cannot see a settle")
	}
	t.Logf("%d of 6 seeds differ with a settle", differs)
}

// TestSettleNeverPicksASettlingVictim (Fremd-Review 1 F4, mutant M3): two
// background leases on a 2-slot preempt target; the first interactive waiter
// preempts the younger, which settles; a second waiter needs a second unit —
// the running lease is the victim, never the settling one (its cancel frees
// nothing).
func TestSettleNeverPicksASettlingVictim(t *testing.T) {
	s := DefaultSettings()
	s.PreemptReleaseTimeout = 2 * time.Second
	d, _ := newTestDispatcher(t, s, settlePolicy(t, 2, 0, settleMS(0, 1500), true))
	var leases []*Lease
	for range 2 {
		l, runCtx, err := d.Acquire(context.Background(), backgroundReq())
		if err != nil {
			t.Fatalf("background acquire: %v", err)
		}
		leases = append(leases, l)
		go func(l *Lease, rc context.Context) { <-rc.Done(); l.Release() }(l, runCtx)
	}
	t.Cleanup(func() {
		for _, l := range leases {
			l.Release()
		}
	})
	ich := make(chan admission, 2)
	startWaiter(withPrincipal(context.Background(), principal("a")), d, "i1", interactiveReq(), ich)
	waitFor(t, "the victim settling", func() bool { return settlingOf(target(t, d)) == 1 })
	// A second waiter needs a second unit: the running lease is the victim,
	// never the settling one (its cancel frees nothing — Fremd-Review 1 F4,
	// mutant M3).
	running := leases[0]
	if settlingOfLease(d, running) {
		running = leases[1]
	}
	startWaiter(withPrincipal(context.Background(), principal("b")), d, "i2", interactiveReq(), ich)
	waitFor(t, "the second preempt", func() bool { return preemptStats(t, d).PreemptsTotal == 2 })
	if !errors.Is(context.Cause(runCtxOf(d, running)), ErrPreempted) {
		t.Fatal("the second preempt did not cancel the running lease (a settling lease was picked again)")
	}
	for range 2 {
		a := recvAdmission(t, ich, "interactive admission")
		if a.err != nil {
			t.Fatalf("interactive: %v", a.err)
		}
		a.lease.Release()
	}
}

// TestSettleOfANonVictimDoesNotCover (Fremd-Review 1 F5): a settling lease
// that was no preempt victim (an external client gone) frees its units on its
// own clock — it does not suppress the preempt a waiting interactive acquire
// needs: the background lease is the victim at once (G1a counts the victim's
// settle only).
func TestSettleOfANonVictimDoesNotCover(t *testing.T) {
	s := DefaultSettings()
	s.PreemptReleaseTimeout = 2 * time.Second
	// The external lease is a stream (settles 5 s), the background victim
	// settles 300 ms (nonstream).
	d, _ := newTestDispatcher(t, s, settlePolicy(t, 2, 1, settleMS(5000, 300), true))
	bg, bgRun, err := d.Acquire(context.Background(), backgroundReq())
	if err != nil {
		t.Fatalf("background acquire: %v", err)
	}
	go func() { <-bgRun.Done(); bg.Release() }()
	xctx2, cancel := context.WithCancel(xctx(context.Background(), "c1", "t1"))
	xl, xRun, err := d.Acquire(xctx2, withStream(externalReq()))
	if err != nil {
		t.Fatalf("external acquire: %v", err)
	}
	cancel()
	<-xRun.Done()
	xl.Release() // settles 5 s: not a victim
	if settlingOf(target(t, d)) != 1 {
		t.Fatal("the external lease does not settle")
	}
	ich := make(chan admission, 1)
	startWaiter(withPrincipal(context.Background(), principal("a")), d, "i1", interactiveReq(), ich)
	a := recvAdmission(t, ich, "interactive admission")
	if a.err != nil {
		t.Fatalf("interactive: %v", a.err)
	}
	a.lease.Release()
	if !errors.Is(context.Cause(bgRun), ErrPreempted) {
		t.Fatal("the background lease was not preempted — the settling external lease covered the need")
	}
}

// settlingOfLease and runCtxOf read one lease's state under the mutex.
func settlingOfLease(d *Dispatcher, l *Lease) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return l.settling
}

func runCtxOf(d *Dispatcher, l *Lease) context.Context {
	d.mu.Lock()
	defer d.mu.Unlock()
	return l.runCtx
}
