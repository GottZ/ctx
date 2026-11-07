package dispatch

// A03-W10 gates, dispatcher half (design/03 §1.3 I-03-7, §4.10 no. 2/3, §1.2
// G1a): a slot-holding lease whose run context ended before its Release stays
// booked as settling for the target's abort_settle_ms, then ends exactly once
// — by its timer, the reaper or Close. The policy is derived from limits rows
// (DerivePolicy), the way production gets it; settlingOf and withStream are
// the two seams the red run against the base stubs (settle_seam_test.go).

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

// settleRows is a slot-managed target (S = slots, R = reserved) with
// abort_settle_ms {stream, nonstream} on one authoritative llama.cpp row —
// preempt-enabled when preempt is set.
func settleRows(slots, reserved int, settle map[string]any, preempt bool) []BackendRow {
	limits := map[string]any{"slots": float64(slots)}
	if reserved > 0 {
		limits["internal_reserved_slots"] = float64(reserved)
	}
	if settle != nil {
		limits["abort_settle_ms"] = settle
	}
	if preempt {
		limits["preempt_background"] = true
	}
	return []BackendRow{{Name: "gpu", Scope: GlobalScope, BaseURL: testOrigin, ProviderClass: "llamacpp", Limits: limits}}
}

func settlePolicy(t *testing.T, slots, reserved int, settle map[string]any, preempt bool) Policy {
	t.Helper()
	return DerivePolicy(settleRows(slots, reserved, settle, preempt), slog.New(slog.DiscardHandler))
}

// settleMS builds the limits value {"stream": s, "nonstream": n} as JSONB
// numbers arrive (float64).
func settleMS(stream, nonstream int) map[string]any {
	return map[string]any{"stream": float64(stream), "nonstream": float64(nonstream)}
}

// heldNow is (Held, HeldExternal, HeldWeight, Settling) of the test origin.
func heldNow(t *testing.T, d *Dispatcher) (held, ext, weight, settling int) {
	t.Helper()
	ts := target(t, d)
	return ts.Held, ts.HeldExternal, ts.HeldWeight, settlingOf(ts)
}

// waitHeldZero polls until the test origin holds nothing and returns when.
func waitHeldZero(t *testing.T, d *Dispatcher, limit time.Duration) time.Time {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if target(t, d).HeldWeight == 0 {
			return time.Now()
		}
		time.Sleep(2 * time.Millisecond)
	}
	held, ext, w, s := heldNow(t, d)
	t.Fatalf("still held after %v: held=%d ext=%d weight=%d settling=%d", limit, held, ext, w, s)
	return time.Time{}
}

// TestSettleHoldsTheUnitAfterAnEndedRun is gate (a) (design/03 §7 A03-W10
// (a)): an XU lease of weight 2, abort_settle_ms.nonstream = 3000; the client
// goes away after 0.5 s (the run context ends), the caller releases — the
// lease stays booked as settling for 3 s ± 0.2 s (Settling 1, HeldWeight
// unchanged), then the units are free.
func TestSettleHoldsTheUnitAfterAnEndedRun(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), settlePolicy(t, 6, 2, settleMS(0, 3000), false))
	ctx, cancel := context.WithCancel(xctx(context.Background(), "c1", "t1"))
	defer cancel()
	req := externalReq()
	req.Weight = 2
	l, runCtx, err := d.Acquire(ctx, req)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	cancel() // the client is gone: the run context ends before the release
	<-runCtx.Done()
	released := time.Now()
	l.Release()
	time.Sleep(100 * time.Millisecond)
	held, ext, weight, settling := heldNow(t, d)
	if held != 1 || ext != 1 || weight != 2 || settling != 1 {
		t.Fatalf("100 ms after the release: held=%d ext=%d weight=%d settling=%d, want 1/1/2/1 (settling for abort_settle_ms.nonstream 3000)",
			held, ext, weight, settling)
	}
	free := waitHeldZero(t, d, 5*time.Second)
	if got := free.Sub(released); got < 2800*time.Millisecond || got > 3200*time.Millisecond {
		t.Fatalf("units free %v after the release, want 3 s ± 0.2 s", got)
	}
	if _, _, _, s := heldNow(t, d); s != 0 {
		t.Fatalf("settling %d after the settle, want 0", s)
	}
}

// TestSettleReadsTheStreamField: Request.Stream picks the stream field, a
// request without it the nonstream field (one row {stream: 400, nonstream:
// 0} and the reverse).
func TestSettleReadsTheStreamField(t *testing.T) {
	for _, tc := range []struct {
		name           string
		stream         bool
		settle         map[string]any
		wantSettlingMs int
	}{
		{"stream request reads stream", true, settleMS(400, 0), 400},
		{"stream request ignores nonstream", true, settleMS(0, 400), 0},
		{"nonstream request reads nonstream", false, settleMS(0, 400), 400},
		{"nonstream request ignores stream", false, settleMS(400, 0), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newTestDispatcher(t, DefaultSettings(), settlePolicy(t, 4, 2, tc.settle, false))
			ctx, cancel := context.WithCancel(xctx(context.Background(), "c1", "t1"))
			req := externalReq()
			if tc.stream {
				req = withStream(req)
			}
			l, runCtx, err := d.Acquire(ctx, req)
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}
			cancel()
			<-runCtx.Done()
			released := time.Now()
			l.Release()
			free := waitHeldZero(t, d, 2*time.Second)
			got := free.Sub(released)
			if tc.wantSettlingMs == 0 && got > 50*time.Millisecond {
				t.Fatalf("held %v after the release, want free at once (the other field)", got)
			}
			if tc.wantSettlingMs > 0 && (got < time.Duration(tc.wantSettlingMs-50)*time.Millisecond || got > time.Duration(tc.wantSettlingMs+200)*time.Millisecond) {
				t.Fatalf("held %v after the release, want ≈ %d ms", got, tc.wantSettlingMs)
			}
		})
	}
}

// TestSettleAllClasses (open question 1, lead decision: the settle holds for
// every class — design/03 §4.10 no. 2 names no class, G1a counts it for a
// background preempt): interactive, background, XU and XB each stay settling
// after an ended run.
func TestSettleAllClasses(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  func(context.Context) context.Context
		req  Request
	}{
		{"interactive", func(c context.Context) context.Context { return withPrincipal(c, principal("a")) }, interactiveReq()},
		{"background", func(c context.Context) context.Context { return c }, backgroundReq()},
		{"external", func(c context.Context) context.Context { return xctx(c, "c1", "t1") }, externalReq()},
		{"external bulk", func(c context.Context) context.Context { return xctx(c, "c1", "t1") },
			Request{Target: Target{Origin: testOrigin}, Class: ClassExternalBulk, Role: "test"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newTestDispatcher(t, DefaultSettings(), settlePolicy(t, 4, 2, settleMS(0, 300), false))
			ctx, cancel := context.WithCancel(tc.ctx(context.Background()))
			l, runCtx, err := d.Acquire(ctx, tc.req)
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}
			cancel()
			<-runCtx.Done()
			l.Release()
			if held, _, _, settling := heldNow(t, d); held != 1 || settling != 1 {
				t.Fatalf("right after the release: held=%d settling=%d, want 1/1", held, settling)
			}
			waitHeldZero(t, d, 2*time.Second)
		})
	}
}

// TestSettleOnlyAfterAnEndedRun (pin): a lease whose run is alive at its
// release ends at once, a settle configured or not; a pass-through lease (the
// kill switch) never settles, its run ended or not.
func TestSettleOnlyAfterAnEndedRun(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), settlePolicy(t, 4, 2, settleMS(2000, 2000), false))
	l, _, err := d.Acquire(xctx(context.Background(), "c1", "t1"), externalReq())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	l.Release()
	if held, _, _, settling := heldNow(t, d); held != 0 || settling != 0 {
		t.Fatalf("live run released: held=%d settling=%d, want 0/0", held, settling)
	}
	// The kill switch on a target that declares slots and a settle: in-house
	// leases pass through and hold nothing — nothing to settle.
	off := DefaultSettings()
	off.Enabled = false
	d2, _ := newTestDispatcher(t, off, settlePolicy(t, 4, 2, settleMS(2000, 2000), false))
	ctx, cancel := context.WithCancel(context.Background())
	l2, runCtx, err := d2.Acquire(ctx, backgroundReq())
	if err != nil {
		t.Fatalf("acquire pass-through: %v", err)
	}
	cancel()
	<-runCtx.Done()
	l2.Release()
	ts := target(t, d2)
	if ts.Inflight != 0 || settlingOf(ts) != 0 {
		t.Fatalf("pass-through released after an ended run: inflight=%d settling=%d, want 0/0", ts.Inflight, settlingOf(ts))
	}
}

// TestSettleZeroIsTheReleaseOfBefore (pin, byte-identical default): 0/0 and a
// missing key end the lease at its release after an ended run, like before
// the wave; the WARN-and-zero of a malformed value too.
func TestSettleZeroIsTheReleaseOfBefore(t *testing.T) {
	for _, tc := range []struct {
		name   string
		settle map[string]any
	}{
		{"missing", nil},
		{"0/0", settleMS(0, 0)},
		{"malformed", map[string]any{"stream": "soon", "nonstream": float64(3000)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newTestDispatcher(t, DefaultSettings(), settlePolicy(t, 4, 2, tc.settle, false))
			ctx, cancel := context.WithCancel(xctx(context.Background(), "c1", "t1"))
			l, runCtx, err := d.Acquire(ctx, externalReq())
			if err != nil {
				t.Fatalf("acquire: %v", err)
			}
			cancel()
			<-runCtx.Done()
			l.Release()
			if held, _, _, settling := heldNow(t, d); held != 0 || settling != 0 {
				t.Fatalf("held=%d settling=%d right after the release, want 0/0", held, settling)
			}
		})
	}
}

// TestSettleAppendsToThePreempt is gate (e), G1a (design/03 §1.2: I-wait max
// ≤ preempt_release_timeout + abort_settle_ms.nonstream + 0.5 s): a
// background lease on a 1-slot preempt target with abort_settle_ms.nonstream
// = 500; an interactive acquire preempts it. Regular release (the victim's
// wire returns at its cancel): the interactive acquire is admitted no
// earlier than the settle (≥ 450 ms) and within watchdog + 500 ms + 0.5 s.
// Forced release (the victim never returns): the watchdog's Go half plus the
// settle. One preempt either way — the settling victim covers the demand.
func TestSettleAppendsToThePreempt(t *testing.T) {
	const watchdog = 300 * time.Millisecond
	for _, tc := range []struct {
		name    string
		regular bool
		min     time.Duration
	}{
		{"regular release", true, 450 * time.Millisecond},
		{"forced release", false, watchdog + 450*time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := DefaultSettings()
			s.PreemptReleaseTimeout = watchdog
			d, _ := newTestDispatcher(t, s, settlePolicy(t, 1, 0, settleMS(0, 500), true))
			victim, runCtx, err := d.Acquire(context.Background(), backgroundReq())
			if err != nil {
				t.Fatalf("background acquire: %v", err)
			}
			if tc.regular {
				go func() {
					<-runCtx.Done() // the wire returns at the cancel
					victim.Release()
				}()
			} else {
				t.Cleanup(victim.Release) // returns only after the test
			}
			start := time.Now()
			ich := make(chan admission, 2)
			startWaiter(withPrincipal(context.Background(), principal("a")), d, "i1", interactiveReq(), ich)
			a := recvAdmission(t, ich, "interactive admission")
			if a.err != nil {
				t.Fatalf("interactive: %v", a.err)
			}
			got := time.Since(start)
			if got < tc.min || got > watchdog+500*time.Millisecond+500*time.Millisecond {
				t.Fatalf("interactive admitted after %v, want in [%v, watchdog %v + settle 500 ms + 0.5 s]", got, tc.min, watchdog)
			}
			if !errors.Is(context.Cause(runCtx), ErrPreempted) {
				t.Fatalf("victim cause %v, want ErrPreempted", context.Cause(runCtx))
			}
			if p := preemptStats(t, d); p.PreemptsTotal != 1 {
				t.Fatalf("preempts %d, want 1", p.PreemptsTotal)
			}
			a.lease.Release()
		})
	}
}

// TestSettleCoversThePreemptNeed (pin of the single-preempt guard under a
// settle): two background leases on a 2-slot preempt target; the first
// interactive acquire preempts one, which settles; while it settles a
// second interactive waiter needs a second unit (one preempt more), but a
// lone waiter does not — the settling victim covers it.
func TestSettleCoversThePreemptNeed(t *testing.T) {
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
	// The lone waiter is covered by the settling victim: no second preempt.
	d.mu.Lock()
	d.maybePreemptLocked(d.targets[testOrigin], 2)
	d.mu.Unlock()
	if p := preemptStats(t, d); p.PreemptsTotal != 1 {
		t.Fatalf("preempts %d with one waiter and one settling victim, want 1", p.PreemptsTotal)
	}
	a := recvAdmission(t, ich, "interactive admission")
	if a.err != nil {
		t.Fatalf("interactive: %v", a.err)
	}
	a.lease.Release()
}

// TestSettleReadsTheStreamFieldOnTheTryDoor: the non-blocking door (the /v1
// failover attempt) carries Request.Stream onto its lease the same way.
func TestSettleReadsTheStreamFieldOnTheTryDoor(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), settlePolicy(t, 4, 2, settleMS(400, 0), false))
	ctx, cancel := context.WithCancel(xctx(context.Background(), "c1", "t1"))
	l, runCtx, err := d.TryAcquire(ctx, withStream(externalReq()))
	if err != nil {
		t.Fatalf("try acquire: %v", err)
	}
	cancel()
	<-runCtx.Done()
	released := time.Now()
	l.Release()
	if got := waitHeldZero(t, d, 2*time.Second).Sub(released); got < 350*time.Millisecond || got > 600*time.Millisecond {
		t.Fatalf("stream lease of the try door held %v after the release, want ≈ 400 ms (abort_settle_ms.stream)", got)
	}
}
