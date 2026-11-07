package dispatch

// A03-W10 gates on the settle's lifecycle (settle.go): exactly one end per
// lease whoever ends it, and the race/leak probe of the dispatcher half. They
// read the dispatcher's own books (settleTimer, the settling counters), so
// they have no red run against the base — that is settle_test.go's part.

import (
	"context"
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
		if snap := d.Snapshot(); snap.ReapsTotal != 1 {
			t.Fatalf("reaps %d, want 1", snap.ReapsTotal)
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
