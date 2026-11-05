package dispatch

// A03-W5 gates (design/03 §4.9, DECISIONS A-E2 no. 2, masterplan K6): the ONE
// wait budget of the external class. Saturation answers a rejection inside
// the budget instead of hanging until the caller ctx ends; an expired waiter
// leaves its queue without residue and its waited time enters the external
// wait window as a censored sample; Request.MaxWait / Request.ServeBy can only
// shorten the setting; a budget already ≤ 0 answers at once without an
// enqueue (reason predicted); the in-house classes never use a wait budget
// (V3). The race gate pins the handoff-against-timer coincidence: exactly one
// lease or ErrWaitExpired per acquire, no phantom lease, no goroutine left.

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// budgetSettings returns the registry defaults with the external wait budget
// set to maxWait.
func budgetSettings(maxWait time.Duration) Settings {
	s := DefaultSettings()
	s.ExternalMaxWait = maxWait
	return s
}

// saturateWithBackground fills both slots of a partitionPolicy(2, 1) target
// with background leases: an external acquire has to wait, and the external
// wait window holds no admission sample of its own (every sample in it is
// then the external acquire under test).
func saturateWithBackground(t *testing.T, d *Dispatcher) {
	t.Helper()
	for i := 0; i < 2; i++ {
		l, _, err := d.Acquire(context.Background(), backgroundReq())
		if err != nil {
			t.Fatalf("background %d: %v", i, err)
		}
		t.Cleanup(l.Release)
	}
	if ts := target(t, d); ts.HeldInternal != 2 || ts.HeldExternal != 0 {
		t.Fatalf("saturation: heldInt=%d heldExt=%d, want 2/0", ts.HeldInternal, ts.HeldExternal)
	}
}

// externalQueueResidue sums every counter and index of the external queue
// (cap counters, total, buckets, ring) — 0 means no waiter left a trace.
func externalQueueResidue(d *Dispatcher) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.targets[canonicalOrigin(testOrigin)]
	if st == nil {
		return 0
	}
	q := st.external
	n := q.total + len(q.buckets) + len(q.ring)
	for _, v := range q.byKey {
		n += v
	}
	for _, v := range q.byTenant {
		n += v
	}
	return n
}

// targetSeq reads the per-target arrival counter — every enqueue bumps it,
// so an unchanged seq proves that no waiter was ever queued. Since the A03-W6
// hardening (review 2 F-1) a refusal predicted after the push is a push
// removed again in the same mutex section: it bumps seq too (the cap ladder,
// the relational cap and a budget ≤ 0 still refuse before any push).
func targetSeq(d *Dispatcher) uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	if st := d.targets[canonicalOrigin(testOrigin)]; st != nil {
		return st.seq
	}
	return 0
}

// within reports whether got lies in want ± tol.
func within(got, want, tol time.Duration) bool {
	return got >= want-tol && got <= want+tol
}

// timedExternal runs one external acquire with a 2-s caller ctx and returns
// its outcome and duration; a lease is released at test cleanup.
func timedExternal(t *testing.T, d *Dispatcher, consumer string, req Request) (*Lease, time.Duration, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(xctx(context.Background(), consumer, "tenant-a"), 2*time.Second)
	defer cancel()
	start := time.Now()
	l, runCtx, err := d.Acquire(ctx, req)
	el := time.Since(start)
	if l != nil {
		t.Cleanup(l.Release)
	} else if runCtx != nil {
		t.Errorf("refused acquire returned a runCtx")
	}
	return l, el, err
}

// TestExternalWaitBudgetEndsSaturatedWait is the masterplan gate "saturation
// ⇒ 429 instead of a hang": with dispatch.external_max_wait = 100 ms an
// external waiter on a saturated target gets ErrWaitExpired after 100 ± 50 ms
// (a rejection), the queue is left without residue, expired counts it, and
// the waited time is ONE censored sample in the external window (before
// A03-W5: back only at the 2-s ctx end, context.DeadlineExceeded, no sample).
func TestExternalWaitBudgetEndsSaturatedWait(t *testing.T) {
	const budget = 100 * time.Millisecond
	d, _ := newTestDispatcher(t, budgetSettings(budget), partitionPolicy(2, 1))
	saturateWithBackground(t, d)

	l, el, err := timedExternal(t, d, "c1", externalReq())
	if l != nil || !errors.Is(err, ErrWaitExpired) || !IsRejection(err) {
		t.Fatalf("saturated external acquire = (lease %v, %v, IsRejection %v), want ErrWaitExpired as a rejection", l != nil, err, IsRejection(err))
	}
	if !within(el, budget, 50*time.Millisecond) {
		t.Fatalf("ErrWaitExpired after %v, want %v ± 50ms", el, budget)
	}
	ts := target(t, d)
	if ts.External.Waiting != 0 || externalQueueResidue(d) != 0 {
		t.Fatalf("after the expiry: waiting=%d residue=%d, want 0/0", ts.External.Waiting, externalQueueResidue(d))
	}
	if r := ts.ExternalRejects; r != (ExternalRejectStats{Expired: 1}) {
		t.Fatalf("external rejects = %+v, want exactly expired=1", r)
	}
	if ts.External.Samples != 1 || !within(ts.External.MaxWait, budget, 50*time.Millisecond) || ts.External.P95Wait != ts.External.MaxWait {
		t.Fatalf("external window = samples %d, max %v, p95 %v, want one censored sample ≈ %v",
			ts.External.Samples, ts.External.MaxWait, ts.External.P95Wait, budget)
	}
	if ts.HeldInternal != 2 || ts.HeldExternal != 0 {
		t.Fatalf("held after the expiry: int=%d ext=%d, want 2/0", ts.HeldInternal, ts.HeldExternal)
	}
}

// TestExternalCallerCtxEndIsNoExpiry is the other side of the budget: a
// caller ctx that ends BEFORE the budget stays the caller's own error (not a
// rejection), and it is no expiry — no expired count, no censored sample.
func TestExternalCallerCtxEndIsNoExpiry(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1))
	saturateWithBackground(t, d)
	ctx, cancel := context.WithTimeout(xctx(context.Background(), "c1", "tenant-a"), 150*time.Millisecond)
	defer cancel()
	l, _, err := d.Acquire(ctx, externalReq())
	if l != nil || !errors.Is(err, context.DeadlineExceeded) || IsRejection(err) {
		t.Fatalf("acquire past its own ctx = (lease %v, %v), want context.DeadlineExceeded, no rejection", l != nil, err)
	}
	ts := target(t, d)
	if ts.ExternalRejects != (ExternalRejectStats{}) || ts.External.Samples != 0 || externalQueueResidue(d) != 0 {
		t.Fatalf("ctx end counted as expiry: rejects %+v samples %d residue %d, want none", ts.ExternalRejects, ts.External.Samples, externalQueueResidue(d))
	}
}

// TestExternalWaitBudgetAnchorsAtArrival: the budget runs from the acquire's
// arrival (the same instant its ServeBy term and its censored sample are
// measured from), not from the moment its timer is armed — time spent
// waiting for the dispatcher mutex counts against the budget instead of
// silently extending it (fremd-review F4: a 200-ms mutex hold moved the
// expiry to ServeBy − 299 ms instead of − 500 ms).
func TestExternalWaitBudgetAnchorsAtArrival(t *testing.T) {
	const budget = 300 * time.Millisecond
	d, _ := newTestDispatcher(t, budgetSettings(budget), partitionPolicy(2, 1))
	saturateWithBackground(t, d)
	ch := make(chan admission, 1)
	start := time.Now()
	d.mu.Lock()
	startWaiter(xctx(context.Background(), "c1", "tenant-a"), d, "late", externalReq(), ch)
	time.Sleep(200 * time.Millisecond) // the acquire has arrived and blocks on the mutex
	d.mu.Unlock()
	a := recvAdmission(t, ch, "the expiry")
	el := time.Since(start)
	if a.lease != nil || !errors.Is(a.err, ErrWaitExpired) {
		t.Fatalf("acquire = (lease %v, %v), want ErrWaitExpired", a.lease != nil, a.err)
	}
	if !within(el, budget, 50*time.Millisecond) {
		t.Fatalf("ErrWaitExpired %v after arrival, want %v ± 50ms (the mutex wait counts against the budget)", el, budget)
	}
	if ts := target(t, d); !within(ts.External.MaxWait, budget, 50*time.Millisecond) {
		t.Fatalf("censored sample %v, want ≈ %v", ts.External.MaxWait, budget)
	}
}

// TestExternalWaitBudgetRequestOnlyShortens pins I-03-5: Request.MaxWait can
// shorten the setting, never lengthen it; zero (and a negative value) is "not
// set".
func TestExternalWaitBudgetRequestOnlyShortens(t *testing.T) {
	for _, tc := range []struct {
		name             string
		setting, request time.Duration
		want             time.Duration
	}{
		{"request shortens the setting", 20 * time.Second, 100 * time.Millisecond, 100 * time.Millisecond},
		{"request cannot lengthen the setting", 200 * time.Millisecond, 60 * time.Second, 200 * time.Millisecond},
		{"zero request is not set", 200 * time.Millisecond, 0, 200 * time.Millisecond},
		{"negative request is not set", 200 * time.Millisecond, -time.Second, 200 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newTestDispatcher(t, budgetSettings(tc.setting), partitionPolicy(2, 1))
			saturateWithBackground(t, d)
			req := externalReq()
			req.MaxWait = tc.request
			l, el, err := timedExternal(t, d, "c1", req)
			if l != nil || !errors.Is(err, ErrWaitExpired) {
				t.Fatalf("acquire = (lease %v, %v), want ErrWaitExpired", l != nil, err)
			}
			if !within(el, tc.want, 50*time.Millisecond) {
				t.Fatalf("ErrWaitExpired after %v, want %v ± 50ms (setting %v, request %v)", el, tc.want, tc.setting, tc.request)
			}
			if ts := target(t, d); ts.ExternalRejects != (ExternalRejectStats{Expired: 1}) {
				t.Fatalf("external rejects = %+v, want expired=1", ts.ExternalRejects)
			}
		})
	}
}

// TestExternalWaitBudgetServeBy pins the ServeBy term: the budget shrinks to
// ServeBy − now − serveMargin (ŝ = 0 until A03-W6). 300 ms ahead leaves no
// budget — ErrWaitExpired at once (reason predicted), no waiter was ever
// queued; 1 s ahead expires after ≈ 500 ms. A ServeBy governs the wait only:
// an acquire that is admissible right now is admitted.
func TestExternalWaitBudgetServeBy(t *testing.T) {
	if serveMargin != 500*time.Millisecond {
		t.Fatalf("serveMargin = %v, want the design's m = 500 ms (design/03 §3.3)", serveMargin)
	}
	t.Run("too close is predicted", func(t *testing.T) {
		d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1))
		saturateWithBackground(t, d)
		seq := targetSeq(d)
		req := externalReq()
		req.ServeBy = time.Now().Add(300 * time.Millisecond)
		l, el, err := timedExternal(t, d, "c1", req)
		if l != nil || !errors.Is(err, ErrWaitExpired) || !IsRejection(err) {
			t.Fatalf("acquire = (lease %v, %v), want ErrWaitExpired as a rejection", l != nil, err)
		}
		// "At once" means no enqueue and no timer — the bound only has to sit
		// far below the 300 ms the ServeBy would otherwise allow; 50 ms leaves
		// room for scheduler noise and GC on a loaded CI runner.
		if el >= 50*time.Millisecond {
			t.Fatalf("predicted refusal took %v, want < 50ms (no enqueue, no timer)", el)
		}
		ts := target(t, d)
		if targetSeq(d) != seq || ts.External.Waiting != 0 || externalQueueResidue(d) != 0 || ts.External.Samples != 0 {
			t.Fatalf("predicted refusal queued a waiter: seq %d → %d, waiting %d, residue %d, samples %d", seq, targetSeq(d), ts.External.Waiting, externalQueueResidue(d), ts.External.Samples)
		}
		if ts.ExternalRejects != (ExternalRejectStats{Predicted: 1}) {
			t.Fatalf("external rejects = %+v, want exactly predicted=1", ts.ExternalRejects)
		}
	})
	t.Run("one second ahead expires after the margin", func(t *testing.T) {
		d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1))
		saturateWithBackground(t, d)
		req := externalReq()
		req.ServeBy = time.Now().Add(time.Second)
		l, el, err := timedExternal(t, d, "c1", req)
		if l != nil || !errors.Is(err, ErrWaitExpired) {
			t.Fatalf("acquire = (lease %v, %v), want ErrWaitExpired", l != nil, err)
		}
		if !within(el, 500*time.Millisecond, 50*time.Millisecond) {
			t.Fatalf("ErrWaitExpired after %v, want 500ms ± 50ms", el)
		}
		if ts := target(t, d); ts.ExternalRejects != (ExternalRejectStats{Expired: 1}) || ts.External.Samples != 1 {
			t.Fatalf("external rejects = %+v samples %d, want expired=1 and one censored sample", ts.ExternalRejects, ts.External.Samples)
		}
	})
	t.Run("admissible now is admitted", func(t *testing.T) {
		d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1))
		req := externalReq()
		req.ServeBy = time.Now().Add(300 * time.Millisecond)
		l, _, err := timedExternal(t, d, "c1", req)
		if l == nil || err != nil {
			t.Fatalf("admissible acquire with a close ServeBy = (lease %v, %v), want the lease — the budget governs waiting only", l != nil, err)
		}
	})
}

// TestExternalWaitBudgetZeroNeverWaits pins the meaning of 0: an external
// acquire never WAITS — admitted at once when admissible, else ErrWaitExpired
// at once (predicted, no enqueue) — fail-closed, NOT unbounded. A negative
// value (a literal Settings bypassing V17) reads the same.
func TestExternalWaitBudgetZeroNeverWaits(t *testing.T) {
	for _, setting := range []time.Duration{0, -time.Second} {
		t.Run(setting.String(), func(t *testing.T) {
			free, _ := newTestDispatcher(t, budgetSettings(setting), partitionPolicy(2, 1))
			if l, _, err := timedExternal(t, free, "c1", externalReq()); l == nil || err != nil {
				t.Fatalf("admissible acquire under budget %v = (lease %v, %v), want the lease", setting, l != nil, err)
			}

			d, _ := newTestDispatcher(t, budgetSettings(setting), partitionPolicy(2, 1))
			saturateWithBackground(t, d)
			seq := targetSeq(d)
			l, el, err := timedExternal(t, d, "c1", externalReq())
			if l != nil || !errors.Is(err, ErrWaitExpired) || el >= 50*time.Millisecond {
				t.Fatalf("saturated acquire under budget %v = (lease %v, %v) after %v, want ErrWaitExpired at once", setting, l != nil, err, el)
			}
			if ts := target(t, d); targetSeq(d) != seq || ts.ExternalRejects != (ExternalRejectStats{Predicted: 1}) {
				t.Fatalf("budget %v: seq %d → %d, rejects %+v, want no enqueue and predicted=1", setting, seq, targetSeq(d), ts.ExternalRejects)
			}
		})
	}
}

// TestExternalExpiryVacatesQueuePosition: an expired head leaves the queue
// cleanly — the next external waiter is the one the release hands the slot
// to, and nothing is left behind.
func TestExternalExpiryVacatesQueuePosition(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1))
	holder := mustAcquire(t, d, xctx(context.Background(), "holder", "tenant-a"), externalReq())
	ch := make(chan admission, 2)
	short := externalReq()
	short.MaxWait = 500 * time.Millisecond // room to observe both waiters queued under load
	startWaiter(xctx(context.Background(), "c1", "tenant-a"), d, "short", short, ch)
	waitFor(t, "short waiter queued", func() bool { return waitingExternal(d) == 1 })
	startWaiter(xctx(context.Background(), "c2", "tenant-b"), d, "long", externalReq(), ch)
	waitFor(t, "long waiter queued", func() bool { return waitingExternal(d) == 2 })

	a := recvAdmission(t, ch, "the short waiter's expiry")
	if a.label != "short" || !errors.Is(a.err, ErrWaitExpired) {
		t.Fatalf("first result = %s (%v), want short with ErrWaitExpired", a.label, a.err)
	}
	holder.Release()
	b := recvAdmission(t, ch, "the long waiter's admission")
	if b.label != "long" || b.err != nil || b.lease == nil {
		t.Fatalf("after the release = %s (%v), want long admitted", b.label, b.err)
	}
	defer b.lease.Release()
	if ts := target(t, d); ts.External.Waiting != 0 || externalQueueResidue(d) != 0 || ts.HeldExternal != 1 {
		t.Fatalf("after the handoff: waiting %d residue %d heldExt %d, want 0/0/1", ts.External.Waiting, externalQueueResidue(d), ts.HeldExternal)
	}
}

// TestInHouseClassesIgnoreWaitBudget is the V3 negative: interactive and
// background waiters carrying MaxWait/ServeBy wait unchanged until admission
// — the scheduler doctrine rejects a wait-abort budget for ctx' own work.
func TestInHouseClassesIgnoreWaitBudget(t *testing.T) {
	d, _ := newTestDispatcher(t, budgetSettings(50*time.Millisecond), onSlotPolicy(1))
	holder, _, err := d.Acquire(ictx(principal("a")), interactiveReq())
	if err != nil {
		t.Fatalf("holder: %v", err)
	}
	ch := make(chan admission, 2)
	ireq := interactiveReq()
	ireq.MaxWait = 50 * time.Millisecond
	ireq.ServeBy = time.Now().Add(10 * time.Millisecond)
	breq := backgroundReq()
	breq.MaxWait = 50 * time.Millisecond
	breq.ServeBy = time.Now().Add(10 * time.Millisecond)
	startWaiter(ictx(principal("b")), d, "interactive", ireq, ch)
	startWaiter(context.Background(), d, "background", breq, ch)
	waitFor(t, "both in-house waiters queued", func() bool {
		ts := target(t, d)
		return ts.Interactive.Waiting == 1 && ts.Background.Waiting == 1
	})
	time.Sleep(300 * time.Millisecond) // six budgets and far past ServeBy
	select {
	case a := <-ch:
		t.Fatalf("in-house waiter %s ended before its admission: %v", a.label, a.err)
	default:
	}
	holder.Release()
	first := recvAdmission(t, ch, "interactive admission")
	if first.label != "interactive" || first.err != nil {
		t.Fatalf("first = %s (%v), want interactive admitted", first.label, first.err)
	}
	if w := first.lease.WaitDur(); w < 250*time.Millisecond {
		t.Fatalf("interactive waited %v, want ≥ 250ms (until the release)", w)
	}
	first.lease.Release()
	second := recvAdmission(t, ch, "background admission")
	if second.label != "background" || second.err != nil {
		t.Fatalf("second = %s (%v), want background admitted", second.label, second.err)
	}
	second.lease.Release()
	if ts := target(t, d); ts.ExternalRejects != (ExternalRejectStats{}) {
		t.Fatalf("in-house waits touched the external counters: %+v", ts.ExternalRejects)
	}
}

// gateGoroutines counts the live goroutines that the race gate answers for:
// all of them except those started by OTHER tests of this package — a waiter
// an earlier test left behind ends on its own (its caller ctx, or now its
// 20-s wait budget) and may do so in the middle of this gate. Goroutines
// started by production code or by the gate itself all count.
func gateGoroutines(self string) int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	count := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.TrimSpace(g) == "" {
			continue
		}
		i := strings.LastIndex(g, "created by ")
		if i >= 0 {
			if lines := strings.SplitN(g[i:], "\n", 3); len(lines) >= 2 &&
				strings.Contains(lines[1], "_test.go:") && !strings.Contains(lines[0], self) {
				continue // started by another test of the package
			}
		}
		count++
	}
	return count
}

// settledGateGoroutines is gateGoroutines once it has been stable for three
// consecutive 10-ms samples (at most 1 s): goroutines still on their way out
// (a closed reaper, a finished test runner) are not counted against the gate.
func settledGateGoroutines(self string) int {
	n := gateGoroutines(self)
	for stable, deadline := 0, time.Now().Add(time.Second); stable < 3 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
		if m := gateGoroutines(self); m == n {
			stable++
		} else {
			n, stable = m, 0
		}
	}
	return n
}

// TestExternalWaitBudgetHandoffRace is the race gate: 1 000 iterations in
// which the release of the occupying external lease (direct handoff) and the
// waiter's 1-ms timer hit the same moment. Every acquire yields exactly one
// lease or ErrWaitExpired; afterwards nothing is held, nothing waits, the
// expired counter matches the refusals, and the goroutine count
// (gateGoroutines) is back to its value before the loop (±0). Run with -race.
func TestExternalWaitBudgetHandoffRace(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1))
	const self = "TestExternalWaitBudgetHandoffRace"
	before := settledGateGoroutines(self)
	var leases, expired int
	for i := 0; i < 1000; i++ {
		holder, _, err := d.Acquire(xctx(context.Background(), "holder", "tenant-a"), externalReq())
		if err != nil || holder == nil {
			t.Fatalf("iteration %d: holder acquire: %v", i, err)
		}
		var wg sync.WaitGroup
		wg.Add(1)
		jitter := 500*time.Microsecond + time.Duration(i%11)*100*time.Microsecond
		go func() {
			defer wg.Done()
			time.Sleep(jitter)
			holder.Release()
		}()
		req := externalReq()
		req.MaxWait = time.Millisecond
		l, _, err := d.Acquire(xctx(context.Background(), "racer", "tenant-b"), req)
		switch {
		case l != nil && err == nil:
			leases++
			l.Release()
		case l == nil && errors.Is(err, ErrWaitExpired):
			expired++
		default:
			t.Fatalf("iteration %d: acquire = (lease %v, %v), want exactly one lease or ErrWaitExpired", i, l != nil, err)
		}
		wg.Wait()
	}
	ts := target(t, d)
	if ts.Held != 0 || ts.Inflight != 0 || ts.HeldExternal != 0 || ts.External.Waiting != 0 || externalQueueResidue(d) != 0 {
		t.Fatalf("after the race: held %d inflight %d heldExt %d waiting %d residue %d, want all 0 (phantom lease or waiter)",
			ts.Held, ts.Inflight, ts.HeldExternal, ts.External.Waiting, externalQueueResidue(d))
	}
	// Since A03-W6 the holder's ~1-ms leases warm the external lease window,
	// and the prediction refuses part of the racers at once (ŵ = L_p50 ≈ the
	// 1-ms budget) — never queued, never on the timer path. Every refusal is
	// still exactly one ErrWaitExpired, counted once as predicted or expired;
	// the race itself needs timer expiries.
	rej := ts.ExternalRejects
	if rej.Cap != 0 || rej.NotOffered != 0 || rej.Predicted+rej.Expired != int64(expired) {
		t.Fatalf("external rejects = %+v, want predicted + expired = %d and nothing else", rej, expired)
	}
	if leases == 0 || rej.Expired == 0 {
		t.Fatalf("race vacuous: %d leases, %d timer expiries — both outcomes must occur", leases, rej.Expired)
	}
	deadline := time.Now().Add(2 * time.Second)
	for gateGoroutines(self) != before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if after := gateGoroutines(self); after != before {
		t.Fatalf("goroutines before %d, after %d — want ±0", before, after)
	}
	t.Logf("1000 iterations: %d leases, %d ErrWaitExpired (predicted %d, expired %d), gate goroutines %d → %d",
		leases, expired, rej.Predicted, rej.Expired, before, gateGoroutines(self))
}

// holdMutexPastExpiry queues one external waiter with a 100-ms budget behind
// an occupying external lease, takes the dispatcher mutex while the waiter
// is still queued (lockQueuedExternal), and holds it well past the budget —
// the waiter's timer fires and its timer branch blocks on the mutex — then
// runs underMutex before unlocking. It returns the waiter's outcome.
func holdMutexPastExpiry(t *testing.T, d *Dispatcher, underMutex func(st *targetState)) admission {
	t.Helper()
	ch := make(chan admission, 1)
	req := externalReq()
	req.MaxWait = 100 * time.Millisecond
	startWaiter(xctx(context.Background(), "racer", "tenant-b"), d, "racer", req, ch)
	st := lockQueuedExternal(t, d)
	time.Sleep(150 * time.Millisecond)
	underMutex(st)
	d.mu.Unlock()
	return recvAdmission(t, ch, "racer outcome")
}

// lockQueuedExternal returns with d.mu HELD once exactly one external waiter
// is queued on the test origin — observed under the mutex, so the waiter
// cannot expire between the observation and the hold (a polled snapshot
// could miss a short budget under load).
func lockQueuedExternal(t *testing.T, d *Dispatcher) *targetState {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		d.mu.Lock()
		if st := d.targets[canonicalOrigin(testOrigin)]; st != nil && st.external.total == 1 {
			return st
		}
		d.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for the racer to queue")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestExternalTimerYieldsToHandoff pins the first check of the timer branch:
// when the direct handoff wins under the mutex (w.admitted), the waiter gets
// its lease — no ErrWaitExpired, no expiry count, no slot lost (a timer
// branch that dropped the waiter anyway would leak the handed-off lease).
func TestExternalTimerYieldsToHandoff(t *testing.T) {
	for i := 0; i < 5; i++ {
		d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1))
		holder := mustAcquire(t, d, xctx(context.Background(), "holder", "tenant-a"), externalReq())
		a := holdMutexPastExpiry(t, d, func(*targetState) { d.finishLocked(holder) })
		holder.Release() // settled under the mutex already; frees its runCtx
		if a.err != nil || a.lease == nil {
			t.Fatalf("round %d: handoff before the timer branch = (lease %v, %v), want the lease", i, a.lease != nil, a.err)
		}
		if ts := target(t, d); ts.HeldExternal != 1 || ts.ExternalRejects != (ExternalRejectStats{}) || ts.External.Samples != 2 {
			t.Fatalf("round %d: heldExt %d rejects %+v samples %d, want 1 / none / 2 admission samples", i, ts.HeldExternal, ts.ExternalRejects, ts.External.Samples)
		}
		a.lease.Release()
		if ts := target(t, d); ts.Held != 0 || ts.Inflight != 0 {
			t.Fatalf("round %d: held %d inflight %d after the release, want 0/0", i, ts.Held, ts.Inflight)
		}
	}
}

// TestExternalTimerYieldsToOfferWithdrawal pins the second check: a waiter
// the dispatcher already ended (w.err — the kill switch, a withdrawn offer)
// gets that error, not ErrWaitExpired, and is counted once (not_offered, no
// expiry, no censored sample, no second removal).
func TestExternalTimerYieldsToOfferWithdrawal(t *testing.T) {
	for i := 0; i < 5; i++ {
		d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1))
		holder := mustAcquire(t, d, xctx(context.Background(), "holder", "tenant-a"), externalReq())
		a := holdMutexPastExpiry(t, d, func(st *targetState) { d.failExternalWaitersLocked(st, ErrExternalNotOffered) })
		if a.lease != nil || !errors.Is(a.err, ErrExternalNotOffered) {
			t.Fatalf("round %d: withdrawal before the timer branch = (lease %v, %v), want ErrExternalNotOffered", i, a.lease != nil, a.err)
		}
		ts := target(t, d)
		if ts.ExternalRejects != (ExternalRejectStats{NotOffered: 1}) || ts.External.Samples != 1 || externalQueueResidue(d) != 0 {
			t.Fatalf("round %d: rejects %+v samples %d residue %d, want not_offered=1 / the holder's sample only / 0", i, ts.ExternalRejects, ts.External.Samples, externalQueueResidue(d))
		}
		holder.Release()
	}
}
