package dispatch

// A03-W6 hardening after review 2 (reports/review/A03-W6-fremdreview-2.md):
// the prediction never refuses the pick of the enqueue wake and runs before
// that wake (F-1, review 3 C1), the first round of ŵ is
// the remaining time of the oldest held external lease (F-2 (b)), the
// fairness-first newcomer waits (F-2 (a)), an expired waiter's Retry-After
// is floored at the time it waited (F-4), the lease sample is per lease
// (F-5) and covers pass-through leases (R18b), and the step list of Settings
// is whole seconds ≥ 1 s (F-7).

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ageLease moves the admission instant of a held lease by back: the lease has
// been running back longer (a fixture of the remaining-time estimator r̂).
func ageLease(d *Dispatcher, l *Lease, back time.Duration) {
	d.mu.Lock()
	l.admitted = l.admitted.Add(-back)
	d.mu.Unlock()
}

// TestPredictionSparesTheEnqueueWakesPick is review 2 F-1 (P5; the fixture
// of TestExternalEnqueueWakesTheNewFairnessFirstHead, FR4-1 trigger 2): S =
// 7, R = 2 (E = 5), 4 units held, H (w 2) waits and does not fit. The
// newcomer n (w 1) is not admissible at once (H holds the class), but its
// own enqueue makes it the fairness-first head, and it fits: the enqueue
// wake admits it. The prediction never refuses the wake's pick — with L_p50
// 10 s > MaxWait 5 s (ŵ = 10 s) n is admitted ≤ 50 ms all the same (red
// before the hardening: predicted at once, 4 of 5 units held, H waits on).
func TestPredictionSparesTheEnqueueWakesPick(t *testing.T) {
	for _, l50 := range []time.Duration{time.Second, 10 * time.Second} {
		t.Run(l50.String(), func(t *testing.T) {
			d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
			clk := fakeFairClock(d)
			bg := context.Background()
			ln := mustAcquire(t, d, fairXctx(bg, "n", "t1", 1), weightedReq(1))
			clk.Add(int64(100 * time.Second))
			ln.Release()
			var held []*Lease
			for i := 0; i < 3; i++ {
				held = append(held, mustAcquire(t, d, fairXctx(bg, fmt.Sprint("a", i), "t0", 1), weightedReq(1)))
			}
			held = append(held, mustAcquire(t, d, fairXctx(bg, "c2", "t2", 1), weightedReq(1)))
			defer func() {
				for _, l := range held {
					l.Release()
				}
			}()
			ctx, cancel := context.WithCancel(bg)
			defer cancel()
			ch := make(chan admission, 1)
			rh := weightedReq(2)
			rh.MaxWait = 5 * time.Second
			startWaiter(fairXctx(ctx, "c2", "t2", 1), d, "H", rh, ch)
			waitQueued(t, d, ch, 1, "H queued (4 + 2 > E, holds)")
			seedLeases(d, testOrigin, waitRingK, l50)
			rn := weightedReq(1)
			rn.MaxWait = 5 * time.Second
			start := time.Now()
			l, _, err := d.Acquire(fairXctx(ctx, "n", "t1", 1), rn)
			el := time.Since(start)
			ts := target(t, d)
			t.Logf("L_p50 %v: n ⇒ lease %v err %v after %v; held external %d (E 5), waiting %d, rejects %+v",
				l50, l != nil, err, el, ts.HeldExternal, ts.External.Waiting, ts.ExternalRejects)
			if l == nil || err != nil || el > 50*time.Millisecond {
				t.Fatalf("n = (lease %v, %v) after %v, want admitted by its enqueue wake ≤ 50ms", l != nil, err, el)
			}
			if ts.HeldExternal != 5 || ts.External.Waiting != 1 || ts.ExternalRejects != (ExternalRejectStats{}) {
				t.Fatalf("held external %d, waiting %d, rejects %+v, want 5 / 1 (H) / none", ts.HeldExternal, ts.External.Waiting, ts.ExternalRejects)
			}
			l.Release()
			cancel()
			recvAdmission(t, ch, "H ends with its ctx")
		})
	}
}

// TestFairnessFirstNewcomerWaits is review 2 F-2 (a) (P6): E = 2 held by a
// holder, six tenants with 10 s of slot-time history wait once each, warm
// window L_p50 5 s. A fresh tenant N (MaxWait 10 s) is not admissible at
// once, and its share alone would predict ⌈7/2⌉ rounds (r̂ + 3 · 5 s > 10 s)
// — but after its enqueue it is the fairness-first head (least tenant slot
// time), so its first round is all it waits: it queues and the first release
// admits it before the six (red before the hardening: predicted at once).
// A newcomer of a tenant with history (x1 of t1) is not the fairness-first
// head and stays predicted at once.
func TestFairnessFirstNewcomerWaits(t *testing.T) {
	s := DefaultSettings()
	s.ExternalMaxWait = 30 * time.Second
	d, _ := newTestDispatcher(t, s, partitionPolicy(3, 1)) // E = 2
	clk := fakeFairClock(d)
	bg := context.Background()
	for i := 1; i <= 6; i++ {
		l := mustAcquire(t, d, fairXctx(bg, fmt.Sprintf("h%d", i), fmt.Sprintf("t%d", i), 1), externalReq())
		clk.Add(int64(10 * time.Second))
		l.Release()
	}
	var held []*Lease
	for i := 0; i < 2; i++ {
		held = append(held, mustAcquire(t, d, fairXctx(bg, "holder", "tenant-h", 1), externalReq()))
	}
	defer func() {
		for _, l := range held[1:] {
			l.Release()
		}
	}()
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 8)
	for i := 1; i <= 6; i++ {
		queueAt(t, d, ctx, testOrigin, fmt.Sprintf("h%d", i), fmt.Sprintf("t%d", i), 1, ch)
	}
	seedLeases(d, testOrigin, waitRingK, 5*time.Second)
	r := externalReq()
	r.MaxWait = 10 * time.Second
	nch := make(chan admission, 1)
	startWaiter(fairXctx(ctx, "n", "tN", 1), d, "N", r, nch)
	waitQueued(t, d, nch, 7, "fresh tenant N queued")

	start := time.Now()
	_, _, err := d.Acquire(fairXctx(ctx, "x1", "t1", 1), r)
	if el := time.Since(start); !errors.Is(err, ErrWaitExpired) || el > 50*time.Millisecond {
		t.Fatalf("x1 of t1 (not fairness-first) = %v after %v, want predicted at once", err, el)
	}
	if rej := target(t, d).ExternalRejects; rej != (ExternalRejectStats{Predicted: 1}) {
		t.Fatalf("rejects %+v, want predicted=1 (x1 only)", rej)
	}

	held[0].Release()
	select {
	case a := <-nch:
		t.Logf("first release admits N (lease %v, err %v)", a.lease != nil, a.err)
		if a.lease == nil || a.err != nil {
			t.Fatalf("N = (lease %v, %v), want the lease of the first release", a.lease != nil, a.err)
		}
		a.lease.Release()
	case a := <-ch:
		t.Fatalf("first release admitted %s before the fairness-first N", a.label)
	case <-time.After(2 * time.Second):
		t.Fatalf("N not admitted within 2 s of the first release")
	}
	cancel()
	for i := 0; i < 6; i++ {
		if a := recvAdmission(t, ch, "waiter ends"); a.lease != nil {
			a.lease.Release()
		}
	}
}

// TestFirstRoundIsTheRemainingTime is review 2 F-2 (b) (P7): E = 8 held,
// warm window L_p50 30 s, MaxWait 20 s. The first round of ŵ is r̂ =
// max(0, L_p50 − the age of the oldest held external lease), not a full
// L_p50:
//   - aged: the oldest lease has run 29.9 s and ends 100 ms later ⇒ r̂ =
//     100 ms: the lone newcomer queues and that release admits it (red
//     before the hardening: ŵ = 30 s > 20 s, predicted at once);
//   - fresh: every lease just started ⇒ r̂ ≈ 30 s > 20 s: predicted at once
//     — the estimator sees no remaining time below L_p50 (the fail-closed
//     side named in prediction.go and docs/operations.md; green before and
//     after, a pin of the semantics, not a gate);
//   - no external lease held (in-house holds the slots) ⇒ r̂ = 0: the lone
//     newcomer queues; the next in-house release admits it (red before the
//     hardening: predicted at once).
func TestFirstRoundIsTheRemainingTime(t *testing.T) {
	run := func(t *testing.T, age time.Duration) (*Lease, time.Duration, error) {
		t.Helper()
		d, _ := newTestDispatcher(t, budgetSettings(20*time.Second), partitionPolicy(9, 1)) // E = 8
		bg := context.Background()
		var held []*Lease
		for i := 0; i < 8; i++ {
			held = append(held, mustAcquire(t, d, fairXctx(bg, "holder", "tenant-h", 1), externalReq()))
		}
		ageLease(d, held[0], age)
		seedLeases(d, testOrigin, waitRingK, 30*time.Second)
		release := time.AfterFunc(100*time.Millisecond, held[0].Release)
		defer release.Stop()
		start := time.Now()
		l, _, err := d.Acquire(fairXctx(bg, "x", "X", 1), externalReq())
		el := time.Since(start)
		held[0].Release()
		for _, h := range held[1:] {
			h.Release()
		}
		if l != nil {
			l.Release()
		}
		t.Logf("oldest lease aged %v: lease %v err %v after %v, rejects %+v", age, l != nil, err, el, target(t, d).ExternalRejects)
		return l, el, err
	}
	t.Run("aged", func(t *testing.T) {
		l, el, err := run(t, 29*time.Second+900*time.Millisecond)
		if l == nil || err != nil || el < 90*time.Millisecond || el > 300*time.Millisecond {
			t.Fatalf("lease %v err %v after %v, want queued and admitted by the release at 100 ms", l != nil, err, el)
		}
	})
	t.Run("fresh", func(t *testing.T) {
		l, el, err := run(t, 0)
		if l != nil || !errors.Is(err, ErrWaitExpired) || el > 50*time.Millisecond {
			t.Fatalf("lease %v err %v after %v, want predicted at once (r̂ ≈ 30 s > 20 s)", l != nil, err, el)
		}
	})
	t.Run("no external lease", func(t *testing.T) {
		d, _ := newTestDispatcher(t, budgetSettings(20*time.Second), partitionPolicy(3, 1)) // E = 2
		var il []*Lease
		for i := 0; i < 3; i++ {
			il = append(il, mustAcquire(t, d, ictx(principal(fmt.Sprint("i", i))), interactiveReq()))
		}
		seedLeases(d, testOrigin, waitRingK, 30*time.Second)
		release := time.AfterFunc(100*time.Millisecond, il[0].Release)
		defer release.Stop()
		start := time.Now()
		l, _, err := d.Acquire(fairXctx(context.Background(), "x", "X", 1), externalReq())
		el := time.Since(start)
		il[0].Release()
		for _, h := range il[1:] {
			h.Release()
		}
		t.Logf("no external lease: lease %v err %v after %v, rejects %+v", l != nil, err, el, target(t, d).ExternalRejects)
		if l == nil || err != nil || el < 90*time.Millisecond || el > 300*time.Millisecond {
			t.Fatalf("lease %v err %v after %v, want queued (r̂ = 0) and admitted by the in-house release", l != nil, err, el)
		}
		l.Release()
	})
}

// TestExpiredRetryAfterFloorsAtTheWaitedTime is review 2 F-4 (P8): a waiter
// whose lease never comes waits its budget out; its Retry-After is
// Q(max(estimate, waited)), never the smallest step a stale estimate gives:
//   - warm: a window of short leases (100 ms) that has gone stale, budget
//     2 s ⇒ RA ≥ Q(2 s) (red before the hardening: RA 1 s after 2 s);
//   - cold: no lease window, budget 1.5 s ⇒ the class formula reads an empty
//     queue (0) after the waiter left it ⇒ RA = Q(1.5 s) = 2 s (red before
//     the hardening: 1 s) — the floor holds on the cold rule too.
func TestExpiredRetryAfterFloorsAtTheWaitedTime(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget time.Duration
		warm   bool
		min    time.Duration
	}{{"warm", 2 * time.Second, true, 2 * time.Second}, {"cold", 1500 * time.Millisecond, false, 2 * time.Second}} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newTestDispatcher(t, budgetSettings(tc.budget), partitionPolicy(2, 1)) // E = 1
			holdExternal(t, d, testOrigin, 1)
			if tc.warm {
				seedLeases(d, testOrigin, waitRingK, 100*time.Millisecond)
			}
			start := time.Now()
			_, _, err := d.Acquire(xctx(context.Background(), "x", "X"), externalReq())
			el := time.Since(start)
			if !errors.Is(err, ErrWaitExpired) || el < tc.budget {
				t.Fatalf("lone waiter = %v after %v, want ErrWaitExpired after its %v budget", err, el, tc.budget)
			}
			if rej := target(t, d).ExternalRejects; rej != (ExternalRejectStats{Expired: 1}) {
				t.Fatalf("rejects %+v, want expired=1 (it queued)", rej)
			}
			ra := retryAfterOf(t, err)
			t.Logf("%s: expired after %v: Retry-After %v", tc.name, el, ra)
			if ra < tc.min || !isStep(ra, DefaultSettings().ExternalRetryAfterSteps) {
				t.Fatalf("expired Retry-After %v, want a step ≥ %v — at least the time it waited", ra, tc.min)
			}
		})
	}
}

// TestLeaseSampleIsPerLease is review 2 F-5 (lead decision 5 of the build:
// the sample is per LEASE, not per weight unit): an external lease of
// weight 3 that ran D leaves one sample D in LeaseDur[external], not D/3
// (mutant R19 of review 2 survived without this pin).
func TestLeaseSampleIsPerLease(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(4, 1)) // E = 3
	l := mustAcquire(t, d, xctx(context.Background(), "x", "tenant-a"), weightedReq(3))
	const dur = 300 * time.Millisecond
	time.Sleep(dur)
	l.Release()
	ls := target(t, d).LeaseDur[ClassExternal]
	t.Logf("w 3 lease of %v: window %+v", dur, ls)
	if ls.Samples != 1 || ls.P50 < dur || ls.P50 > dur+100*time.Millisecond {
		t.Fatalf("lease window %+v, want one sample ≈ %v (per lease, not per unit: %v)", ls, dur, dur/3)
	}
}

// TestPassThroughLeaseIsSampled pins the build report's claim (review 2
// R18b): a pass-through lease (no declared policy, slotHeld false) is one
// lease-duration sample of its class like a slot-holding one.
func TestPassThroughLeaseIsSampled(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), Policy{})
	l := mustAcquire(t, d, ictx(principal("p")), interactiveReq())
	if l.slotHeld {
		t.Fatalf("lease holds a slot without a policy, want pass-through")
	}
	l.Release()
	mustAcquire(t, d, context.Background(), backgroundReq()).Release()
	ts := target(t, d)
	if ts.LeaseDur[ClassInteractive].Samples != 1 || ts.LeaseDur[ClassBackground].Samples != 1 {
		t.Fatalf("pass-through lease samples interactive %d background %d, want 1/1",
			ts.LeaseDur[ClassInteractive].Samples, ts.LeaseDur[ClassBackground].Samples)
	}
}

// TestRetryAfterStepsAreWholeSeconds is review 2 F-7: a step list set on
// Settings directly (past the config grammar) must be whole seconds ≥ 1 s,
// strictly ascending, like the Settings doc says — else the default steps
// are in effect (one WARN).
func TestRetryAfterStepsAreWholeSeconds(t *testing.T) {
	d, h := newTestDispatcher(t, DefaultSettings(), Policy{})
	def := DefaultSettings().ExternalRetryAfterSteps
	for _, in := range [][]time.Duration{
		{500 * time.Millisecond},
		{time.Second, 1500 * time.Millisecond},
		{999 * time.Millisecond, 2 * time.Second},
	} {
		if got := d.resolveRetryAfterSteps(in); fmt.Sprint(got) != fmt.Sprint(def) {
			t.Fatalf("resolve %v = %v, want the default %v", in, got, def)
		}
	}
	if got := d.resolveRetryAfterSteps([]time.Duration{time.Second, 3 * time.Second}); fmt.Sprint(got) != "[1s 3s]" {
		t.Fatalf("resolve [1s 3s] = %v, want it kept", got)
	}
	if n := h.count("invalid external retry-after steps"); n != 3 {
		t.Fatalf("%d WARN lines, want 3 (one per invalid list)", n)
	}
}

// TestPredictedRefusalAfterThePushLeavesNoTrace is the target gate of the
// wave under lead decision 1 of the hardening (review 2 P9, ported): with
// the prediction behind the push a predicted refusal is a push and a
// removal inside ONE dispatcher mutex section, before any wake. E = 2 held, one waiter, warm
// window L_p50 60 s (r̂ > MaxWait 5 s through the run), 10 000 acquires of 8
// parallel callers, 13 tenants, 37 consumers, fair weights 1–10: every one
// is predicted (a *RejectionError), no waiter is ever visible beyond its
// section (a concurrent sampler sees the one waiter only), each refusal is
// one mutex section (OpsTotal +10 000) and one transient push (seq +10 000,
// removed in place), no goroutine and no timer is left (goroutines ±0; the
// refusals answer at once — p99 < 50 ms — and none waits anywhere near a
// timer of its 5-s budget — the slowest < 1 s: the maximum of 10 000 samples
// under -race on a loaded host is scheduling noise, review 3 delta D-2; the
// per-refusal 50-ms bound stays gate (a)'s), and the bookkeeping equals its
// recomputation.
func TestPredictedRefusalAfterThePushLeavesNoTrace(t *testing.T) {
	s := budgetSettings(5 * time.Second)
	s.ExternalQueuePerConsumer, s.ExternalQueuePerTenant = 0, 0
	d, _ := newTestDispatcher(t, s, partitionPolicy(3, 1)) // E = 2
	bg := context.Background()
	holdExternal(t, d, testOrigin, 2)
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 1)
	queueAt(t, d, ctx, testOrigin, "q0", "tenant-q", 1, ch) // cold: queues
	seedLeases(d, testOrigin, waitRingK, 60*time.Second)
	d.Close() // no publication pass may add sections while the gate counts
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	g0 := runtime.NumGoroutine()
	seq0, ops0 := targetSeq(d), d.Snapshot().OpsTotal

	var peak atomic.Int64
	stop := make(chan struct{})
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if w := int64(target(t, d).External.Waiting); w > peak.Load() {
				peak.Store(w)
			}
		}
	}()
	const n, par = 10000, 8
	lat := make([][]time.Duration, par)
	var wg sync.WaitGroup
	var bad sync.Map
	for p := 0; p < par; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < n/par; i++ {
				st := time.Now()
				l, _, err := d.Acquire(fairXctx(bg, fmt.Sprintf("c%d-%d", p, i%37), fmt.Sprintf("t%d", i%13), 1+i%10), externalReq())
				lat[p] = append(lat[p], time.Since(st))
				if l != nil {
					l.Release()
				}
				var re *RejectionError
				if !errors.As(err, &re) || !errors.Is(err, ErrWaitExpired) {
					bad.Store(fmt.Sprint(err), true)
				}
			}
		}(p)
	}
	wg.Wait()
	close(stop)
	sampler.Wait()
	var all []time.Duration
	for _, l := range lat {
		all = append(all, l...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	ts := target(t, d)
	pushes, ops := targetSeq(d)-seq0, d.Snapshot().OpsTotal-ops0
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	g1 := runtime.NumGoroutine()
	t.Logf("%d refusals: pushes %d, ops %d, waiting peak %d / after %d, goroutines %d → %d, latency p50 %v p99 %v max %v, rejects %+v",
		len(all), pushes, ops, peak.Load(), ts.External.Waiting, g0, g1, all[len(all)/2], all[len(all)*99/100], all[len(all)-1], ts.ExternalRejects)
	bad.Range(func(k, _ any) bool { t.Errorf("not a predicted *RejectionError: %v", k); return true })
	if ts.ExternalRejects != (ExternalRejectStats{Predicted: n}) || pushes != n || ops != n {
		t.Fatalf("rejects %+v, pushes %d, ops %d, want predicted=%d, one transient push and one section per refusal", ts.ExternalRejects, pushes, ops, n)
	}
	if peak.Load() != 1 || ts.External.Waiting != 1 || g1 > g0 || all[len(all)*99/100] > 50*time.Millisecond || all[len(all)-1] > time.Second {
		t.Fatalf("residue: waiting peak %d after %d, goroutines %d → %d, p99 %v max %v", peak.Load(), ts.External.Waiting, g0, g1, all[len(all)*99/100], all[len(all)-1])
	}
	if diff, _ := d.fairAuditForTest(testOrigin); diff != "" {
		t.Fatalf("fairness bookkeeping differs from its recomputation: %s", diff)
	}
	cancel()
	recvAdmission(t, ch, "q0 ends with its ctx")
}

// TestRefusedNewcomerLeavesTheHoldIntact is review 3 C1 (the reviewer's
// construction): S 7, R 2 (E 5); 5 interactive units and 1 external unit
// (consumer h of tenant tH) held, 1 slot free. H (w 2, tH) waits, does not
// fit and holds the class (fairness-first: tH has no slot time). X (w 1,
// tenant tX with 10 s of slot time) waits behind it. A newcomer W of tenant
// tW (20 s of slot time, no activity) arrives on a warm window (L_p50 10 s,
// its MaxWait 5 s): its push raises T_act 2 → 3 and shrinks the tenant
// share 3 → 2, so H would be over its share and X the fairness-first head
// that fits. W is predicted (r̂ ≈ 10 s > 5 s) — and removed before any wake:
// X keeps waiting behind the holding H (red with the prediction after the
// wake: the wake of W's transient push admitted X). Control: when W really
// queues (MaxWait 60 s), the same shares admit X — the FR4-1 wake of a
// waiter that stays.
func TestRefusedNewcomerLeavesTheHoldIntact(t *testing.T) {
	for _, tc := range []struct {
		name      string
		maxWait   time.Duration
		xAdmitted bool
	}{{"refused", 5 * time.Second, false}, {"queued", 60 * time.Second, true}} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newTestDispatcher(t, budgetSettings(60*time.Second), partitionPolicy(7, 2))
			clk := fakeFairClock(d)
			bg := context.Background()
			lx := mustAcquire(t, d, fairXctx(bg, "x", "tX", 1), weightedReq(1))
			clk.Add(int64(10 * time.Second))
			lx.Release()
			lw := mustAcquire(t, d, fairXctx(bg, "w", "tW", 1), weightedReq(1))
			clk.Add(int64(20 * time.Second))
			lw.Release()
			var il []*Lease
			for i := 0; i < 5; i++ {
				il = append(il, mustAcquire(t, d, ictx(principal(fmt.Sprint("i", i))), interactiveReq()))
			}
			lh := mustAcquire(t, d, fairXctx(bg, "h", "tH", 1), weightedReq(1))
			defer func() {
				for _, l := range il {
					l.Release()
				}
				lh.Release()
			}()
			ctx, cancel := context.WithCancel(bg)
			defer cancel()
			chH, chX, chW := make(chan admission, 1), make(chan admission, 1), make(chan admission, 1)
			startWaiter(fairXctx(ctx, "h", "tH", 1), d, "H", weightedReq(2), chH)
			waitQueued(t, d, chH, 1, "H queued (holds the class)")
			startWaiter(fairXctx(ctx, "x", "tX", 1), d, "X", weightedReq(1), chX)
			waitQueued(t, d, chX, 2, "X queued behind the holding H")
			seedLeases(d, testOrigin, waitRingK, 10*time.Second)
			r := weightedReq(1)
			r.MaxWait = tc.maxWait
			startWaiter(fairXctx(ctx, "w", "tW", 1), d, "W", r, chW)
			xAdmitted, wRefused := false, false
			deadline := time.After(200 * time.Millisecond)
		watch:
			for {
				select {
				case a := <-chX:
					xAdmitted = a.lease != nil
					if a.lease != nil {
						defer a.lease.Release()
					}
				case a := <-chW:
					wRefused = errors.Is(a.err, ErrWaitExpired)
					if a.lease != nil {
						a.lease.Release()
					}
				case <-deadline:
					break watch
				}
			}
			ts := target(t, d)
			t.Logf("%s: W refused %v, X admitted %v; held external %d, waiting %d, rejects %+v", tc.name, wRefused, xAdmitted, ts.HeldExternal, ts.External.Waiting, ts.ExternalRejects)
			if xAdmitted != tc.xAdmitted || wRefused == tc.xAdmitted {
				t.Fatalf("%s: W refused %v, X admitted %v, want refused %v / admitted %v", tc.name, wRefused, xAdmitted, !tc.xAdmitted, tc.xAdmitted)
			}
			if !tc.xAdmitted {
				if diff, _ := d.fairAuditForTest(testOrigin); diff != "" {
					t.Fatalf("fairness bookkeeping after the refusal: %s", diff)
				}
			}
			cancel()
			recvAdmission(t, chH, "H ends")
			if !xAdmitted {
				recvAdmission(t, chX, "X ends")
			}
			if tc.xAdmitted {
				recvAdmission(t, chW, "W ends")
			}
		})
	}
}

// TestRefusedNewcomerIsNoEvent pins the other half of review 3 C1: a refused
// newcomer leaves the queue without a wake, so it is no event of the
// fairness order either (FR4-2: the order is re-read at events only). The
// fixture of TestExternalFairnessViewFreezesBetweenEvents: E 5, interactive
// holds 3, consumer a (fair weight 100) holds a unit and its w 4 head holds
// the class, b (1 unit-s of history) waits with w 1; 200 s pass on the clock
// without an event. A newcomer n (w 4, fresh tenant: the fairness-first
// head, does not fit, holds; warm window L_p50 10 s, MaxWait 1 s) is
// predicted — and b keeps waiting until the next real event (an interactive
// release) admits it (a wake at the refusal would re-read the order and
// admit b at once), and the order stays frozen at the last event: the pick
// is nobody before and after the refusal (the decision reads the order at
// now, review 3 delta D-1, but does not persist that view — b would be the
// pick without the wake that hands it its units).
func TestRefusedNewcomerIsNoEvent(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	clk := fakeFairClock(d)
	bg := context.Background()
	var il []*Lease
	for i := 0; i < 3; i++ {
		il = append(il, mustAcquire(t, d, ictx(principal(fmt.Sprint("i", i))), interactiveReq()))
	}
	defer func() {
		for _, l := range il {
			l.Release()
		}
	}()
	lb := mustAcquire(t, d, fairXctx(bg, "b", "ta", 1), weightedReq(1))
	clk.Add(int64(time.Second))
	lb.Release()
	la := mustAcquire(t, d, fairXctx(bg, "a", "ta", 100), weightedReq(1))
	defer la.Release()
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 2)
	ra := weightedReq(4)
	ra.MaxWait = 5 * time.Second
	startWaiter(fairXctx(ctx, "a", "ta", 100), d, "a-head", ra, ch)
	waitQueued(t, d, ch, 1, "a's head queued (holds)")
	rb := weightedReq(1)
	rb.MaxWait = 5 * time.Second
	startWaiter(fairXctx(ctx, "b", "ta", 1), d, "b-head", rb, ch)
	waitQueued(t, d, ch, 2, "b queued behind the hold")
	clk.Add(int64(200 * time.Second))
	seedLeases(d, testOrigin, waitRingK, 10*time.Second)
	pickNow := func() string {
		d.mu.Lock()
		defer d.mu.Unlock()
		w, _ := d.externalPickLocked(d.targetLocked(testOrigin), weightSlots)
		return waiterLabel(w)
	}
	before := pickNow()
	rn := weightedReq(4)
	rn.MaxWait = time.Second
	if l, _, err := d.Acquire(fairXctx(bg, "n", "tn", 1), rn); l != nil || !errors.Is(err, ErrWaitExpired) {
		t.Fatalf("newcomer n = (lease %v, %v), want predicted", l != nil, err)
	}
	if after := pickNow(); before != "nobody" || after != before {
		t.Fatalf("frozen pick %s → %s across the refusal, want nobody → nobody (the refusal moved the order)", before, after)
	}
	select {
	case a := <-ch:
		t.Fatalf("%s admitted at the refused newcomer (err %v) — the refusal acted as an event", a.label, a.err)
	case <-time.After(20 * time.Millisecond):
	}
	if rej := target(t, d).ExternalRejects; rej != (ExternalRejectStats{Predicted: 1}) {
		t.Fatalf("rejects %+v, want predicted=1", rej)
	}
	il[0].Release()
	il = il[1:]
	got := recvAdmission(t, ch, "b after the next event")
	if got.err != nil || got.label != "b-head" {
		t.Fatalf("after the next event: %s err=%v, want b admitted", got.label, got.err)
	}
	got.lease.Release()
}

// TestQueuedPositionDecides pins the terms of the decision after the push
// (review 3 mutants on r̂ and the queued position): E = 2 held, MaxWait 5 s,
// two older tenants tX and tY waiting (no slot time — they win the arrival
// tie, the newcomer is no fairness-first head), so the newcomer of a third
// tenant needs ⌈3/2⌉ = 2 rounds:
//   - overdue oldest lease: it has run 40 s, past L_p50 10 s ⇒ r̂ = 0, never
//     negative: ŵ = 0 + 10 s > 5 s ⇒ predicted, Retry-After 10 s (a negative
//     r̂ would let it wait);
//   - waiting tenants count: L_p50 5 s, fresh leases ⇒ ŵ = r̂ + 5 s ≈ 10 s >
//     5 s ⇒ predicted, Retry-After 10 s (without the waiting tenants, T_w = 1,
//     it would be one round, ŵ = r̂ < 5 s, and wait);
//   - boundary: overdue oldest lease (r̂ = 0), L_p50 5 s ⇒ ŵ = 0 + 5 s =
//     MaxWait exactly ⇒ it waits (ŵ ≤ budget, not <).
func TestQueuedPositionDecides(t *testing.T) {
	for _, tc := range []struct {
		name string
		age  time.Duration
		l50  time.Duration
		ra   time.Duration // 0: the newcomer waits
	}{
		{"overdue oldest lease", 40 * time.Second, 10 * time.Second, 10 * time.Second},
		{"waiting tenants count", 0, 5 * time.Second, 10 * time.Second},
		{"boundary", 40 * time.Second, 5 * time.Second, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := budgetSettings(5 * time.Second)
			d, _ := newTestDispatcher(t, s, partitionPolicy(3, 1)) // E = 2
			bg := context.Background()
			var held []*Lease
			for i := 0; i < 2; i++ {
				held = append(held, mustAcquire(t, d, fairXctx(bg, "holder", "tenant-h", 1), externalReq()))
			}
			defer func() {
				for _, l := range held {
					l.Release()
				}
			}()
			ageLease(d, held[0], tc.age)
			ctx, cancel := context.WithCancel(bg)
			defer cancel()
			ch := make(chan admission, 2)
			startWaiter(fairXctx(ctx, "x", "tX", 1), d, "X", externalReq(), ch)
			waitQueued(t, d, ch, 1, "X queued (cold)")
			startWaiter(fairXctx(ctx, "y", "tY", 1), d, "Y", externalReq(), ch)
			waitQueued(t, d, ch, 2, "Y queued (cold)")
			seedLeases(d, testOrigin, waitRingK, tc.l50)
			if tc.ra == 0 {
				nch := make(chan admission, 1)
				startWaiter(fairXctx(ctx, "n", "tN", 1), d, "N", externalReq(), nch)
				waitQueued(t, d, nch, 3, "N waits at ŵ = budget")
				if rej := target(t, d).ExternalRejects; rej != (ExternalRejectStats{}) {
					t.Fatalf("rejects %+v, want none", rej)
				}
				cancel()
				for i := 0; i < 2; i++ {
					recvAdmission(t, ch, "waiter ends")
				}
				recvAdmission(t, nch, "N ends")
				return
			}
			start := time.Now()
			l, _, err := d.Acquire(fairXctx(bg, "n", "tN", 1), externalReq())
			el := time.Since(start)
			if l != nil {
				l.Release()
				t.Fatalf("newcomer admitted, want predicted")
			}
			ra := retryAfterOf(t, err)
			ts := target(t, d)
			t.Logf("%s: %v after %v, Retry-After %v, waiting %d, rejects %+v", tc.name, err, el, ra, ts.External.Waiting, ts.ExternalRejects)
			if !errors.Is(err, ErrWaitExpired) || el > 50*time.Millisecond || ra != tc.ra || ts.ExternalRejects != (ExternalRejectStats{Predicted: 1}) || ts.External.Waiting != 2 {
				t.Fatalf("%s: %v after %v, Retry-After %v, rejects %+v, want predicted at once with %v", tc.name, err, el, ra, ts.ExternalRejects, tc.ra)
			}
			cancel()
			recvAdmission(t, ch, "X ends")
			recvAdmission(t, ch, "Y ends")
		})
	}
}

// TestContinuationHeadIsNotTheNewcomers is the fairness-first term "no
// continuation waits" (review 3 mutant): a waiting continuation is the head
// of the class (A03-W17), so a newcomer that would rank first among the
// bucket heads is not the fairness-first head and keeps its full rounds.
// E = 2 held (the oldest lease 8 s into L_p50 10 s ⇒ r̂ ≈ 2 s), MaxWait 5 s,
// X of tX (10 s of slot time) waits; the fresh tenant N needs ⌈T_w/2⌉
// rounds:
//   - continuation waiting (T_w 3: the holder's tenant, tX, tN) ⇒ ŵ ≈ 12 s
//     ⇒ predicted;
//   - no continuation (T_w 2) ⇒ ŵ = r̂ ≈ 2 s by its rounds alone, and it is
//     the fairness-first head anyway ⇒ it waits.
func TestContinuationHeadIsNotTheNewcomers(t *testing.T) {
	for _, withCont := range []bool{true, false} {
		t.Run(fmt.Sprintf("continuation=%v", withCont), func(t *testing.T) {
			d, _ := newTestDispatcher(t, budgetSettings(5*time.Second), partitionPolicy(3, 1)) // E = 2
			clk := fakeFairClock(d)
			bg := context.Background()
			lx := mustAcquire(t, d, fairXctx(bg, "x", "tX", 1), externalReq())
			clk.Add(int64(10 * time.Second))
			lx.Release()
			var held []*Lease
			for i := 0; i < 2; i++ {
				held = append(held, mustAcquire(t, d, fairXctx(bg, "holder", "tenant-h", 1), externalReq()))
			}
			defer func() {
				for _, l := range held {
					l.Release()
				}
			}()
			ageLease(d, held[0], 8*time.Second)
			ctx, cancel := context.WithCancel(bg)
			defer cancel()
			ch := make(chan admission, 3)
			n := 0
			if withCont {
				startWaiter(fairXctx(ctx, "holder", "tenant-h", 1), d, "C", contReq(1), ch)
				n++
				waitQueued(t, d, ch, n, "continuation queued at the class head")
			}
			startWaiter(fairXctx(ctx, "x", "tX", 1), d, "X", externalReq(), ch)
			n++
			waitQueued(t, d, ch, n, "X queued (cold)")
			seedLeases(d, testOrigin, waitRingK, 10*time.Second)
			nch := make(chan admission, 1)
			startWaiter(fairXctx(ctx, "n", "tN", 1), d, "N", externalReq(), nch)
			var a admission
			got := false
			select {
			case a = <-nch:
				got = true
			case <-time.After(50 * time.Millisecond):
			}
			ts := target(t, d)
			t.Logf("continuation=%v: N ended %v (lease %v, err %v), waiting %d, rejects %+v", withCont, got, a.lease != nil, a.err, ts.External.Waiting, ts.ExternalRejects)
			if withCont && (!got || !errors.Is(a.err, ErrWaitExpired)) {
				t.Fatalf("N behind a waiting continuation: ended %v err %v, want predicted at once", got, a.err)
			}
			if !withCont && got {
				t.Fatalf("N as the fairness-first head: ended (lease %v, err %v), want it waiting", a.lease != nil, a.err)
			}
			cancel()
			for i := 0; i < n; i++ {
				recvAdmission(t, ch, "waiter ends")
			}
			if !withCont {
				recvAdmission(t, nch, "N ends")
			}
		})
	}
}

// TestRemovedTailLeavesNoReference pins the queue hygiene of review 3 F3-5:
// a refusal predicted after its push removes the newest waiter of a bucket
// that still holds older waiters — the vacated slot of the backing array is
// cleared, as in the continuation path, so no reference to the refused
// waiter outlives its section.
func TestRemovedTailLeavesNoReference(t *testing.T) {
	q := newExternalQueue()
	p := xuPrincipal("c", "tenant-a")
	a, b := &waiter{class: ClassExternal, principal: p, weight: 1}, &waiter{class: ClassExternal, principal: p, weight: 1}
	q.push(a)
	q.push(b)
	q.remove(b)
	bucket := q.buckets[q.bucketKey(p)]
	if len(bucket) != 1 || bucket[0] != a {
		t.Fatalf("bucket %v after removing its tail, want [a]", bucket)
	}
	if tail := bucket[:cap(bucket)][1]; tail != nil {
		t.Fatalf("backing array still references the removed waiter")
	}
}

// TestRefusalDecidesOnTheWakesView is review 3 delta D-1 (the reviewer's C2):
// the decision reads the fairness order the enqueue wake would read — at
// now, not at the last event. S 7, R 2 (E 5); five interactive leases and an
// external one of a2 (tenant tA) hold 6 of 7 slots; H (a1 of tA, w 2) waits,
// does not fit and holds (tA's running slot time 0 at the last event < tB's
// 5 s of history). The fair clock moves 10 s without an event: at now tA has
// 10 s, so the newcomer n of tB (w 1) is the fairness-first head and fits
// the free unit — its enqueue wake admits it although L_p50 10 s > MaxWait
// 5 s and r̂ ≈ 10 s (red with the pick read at the last event's view).
func TestRefusalDecidesOnTheWakesView(t *testing.T) {
	d, _ := newTestDispatcher(t, budgetSettings(60*time.Second), partitionPolicy(7, 2))
	clk := fakeFairClock(d)
	bg := context.Background()
	lb := mustAcquire(t, d, fairXctx(bg, "b", "tB", 1), weightedReq(1))
	clk.Add(int64(5 * time.Second))
	lb.Release()
	var il []*Lease
	for i := 0; i < 5; i++ {
		il = append(il, mustAcquire(t, d, ictx(principal(fmt.Sprint("i", i))), interactiveReq()))
	}
	la := mustAcquire(t, d, fairXctx(bg, "a2", "tA", 1), weightedReq(1))
	defer func() {
		for _, l := range il {
			l.Release()
		}
		la.Release()
	}()
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	chH := make(chan admission, 1)
	startWaiter(fairXctx(ctx, "a1", "tA", 1), d, "H", weightedReq(2), chH)
	waitQueued(t, d, chH, 1, "H queued and holding")
	seedLeases(d, testOrigin, waitRingK, 10*time.Second)
	clk.Add(int64(10 * time.Second))
	r := weightedReq(1)
	r.MaxWait = 5 * time.Second
	start := time.Now()
	l, _, err := d.Acquire(fairXctx(ctx, "b", "tB", 1), r)
	el := time.Since(start)
	ts := target(t, d)
	t.Logf("n: lease %v err %v after %v; held external %d, waiting %d, rejects %+v", l != nil, err, el, ts.HeldExternal, ts.External.Waiting, ts.ExternalRejects)
	if l == nil || err != nil || el > 50*time.Millisecond || ts.HeldExternal != 2 || ts.ExternalRejects != (ExternalRejectStats{}) {
		t.Fatalf("n = (lease %v, %v) after %v, want admitted by its enqueue wake ≤ 50ms (the fairness-first head at now, it fits)", l != nil, err, el)
	}
	l.Release()
	cancel()
	recvAdmission(t, chH, "H ends")
}

// TestFairnessFirstReadsTheWakesView is review 3 delta D-3 (the reviewer's
// C3): the fairness-first term of the decision reads the order at now as
// well. The fixture of TestRefusalDecidesOnTheWakesView, but the newcomer n
// of tB weighs 3 (it does not fit the free unit; two rounds at T_w 2 ⇒ ŵ =
// r̂ + 10 s > 5 s) and the held external lease of a2 is 8 s old (r̂ ≈ 2 s ≤
// 5 s). At now n is the fairness-first head (tA 10 s > tB 5 s), so it waits
// its first round; at the last event's view the holding X of tA ranks first
// and n would be refused.
func TestFairnessFirstReadsTheWakesView(t *testing.T) {
	d, _ := newTestDispatcher(t, budgetSettings(60*time.Second), partitionPolicy(7, 2))
	clk := fakeFairClock(d)
	bg := context.Background()
	lb := mustAcquire(t, d, fairXctx(bg, "b", "tB", 1), weightedReq(1))
	clk.Add(int64(5 * time.Second))
	lb.Release()
	var il []*Lease
	for i := 0; i < 5; i++ {
		il = append(il, mustAcquire(t, d, ictx(principal(fmt.Sprint("i", i))), interactiveReq()))
	}
	la := mustAcquire(t, d, fairXctx(bg, "a2", "tA", 1), weightedReq(1))
	ageLease(d, la, 8*time.Second)
	defer func() {
		for _, l := range il {
			l.Release()
		}
		la.Release()
	}()
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	chX := make(chan admission, 1)
	startWaiter(fairXctx(ctx, "a1", "tA", 1), d, "X", weightedReq(2), chX)
	waitQueued(t, d, chX, 1, "X queued and holding")
	seedLeases(d, testOrigin, waitRingK, 10*time.Second)
	clk.Add(int64(10 * time.Second))
	r := weightedReq(3)
	r.MaxWait = 5 * time.Second
	chN := make(chan admission, 1)
	startWaiter(fairXctx(ctx, "b", "tB", 1), d, "n", r, chN)
	waitQueued(t, d, chN, 2, "n waits its first round as the fairness-first head at now")
	if rej := target(t, d).ExternalRejects; rej != (ExternalRejectStats{}) {
		t.Fatalf("rejects %+v, want none", rej)
	}
	cancel()
	recvAdmission(t, chX, "X ends")
	recvAdmission(t, chN, "n ends")
}

// TestPredictedRefusalRestoresRecordWeight is review 4 FR4-2: a refusal
// predicted after the push leaves the fair weight of an active record as it
// was — no event of the order (F3-1), so no share moves without a wake.
// Consumer c (tenant tA) holds a lease at fair weight 2 next to a holder
// (E 2 full); c's next request arrives with fair weight 7 on a warm window
// (L_p50 10 s, MaxWait 5 s, r̂ ≈ 10 s) and is predicted: the record keeps
// fw 2 and its tenant Σ fair_weight 2 (red with the push's reweigh kept:
// 2 → 7), the bookkeeping equals its recomputation.
func TestPredictedRefusalRestoresRecordWeight(t *testing.T) {
	d, _ := newTestDispatcher(t, budgetSettings(5*time.Second), partitionPolicy(3, 1)) // E = 2
	bg := context.Background()
	lc := mustAcquire(t, d, fairXctx(bg, "c", "tA", 2), externalReq())
	defer lc.Release()
	lh := mustAcquire(t, d, fairXctx(bg, "holder", "tenant-h", 1), externalReq())
	defer lh.Release()
	seedLeases(d, testOrigin, waitRingK, 10*time.Second)
	record := func() (int, int) {
		d.mu.Lock()
		defer d.mu.Unlock()
		c := d.targets[canonicalOrigin(testOrigin)].external.fair.consumers[fairPrincipalKey("c", "tA")]
		return c.fw, c.tenant.sumFW
	}
	fw0, sum0 := record()
	seq0 := targetSeq(d)
	_, _, err := d.Acquire(fairXctx(bg, "c", "tA", 7), externalReq())
	fw1, sum1 := record()
	t.Logf("refused request at fw 7 of c (holds at fw 2): %v, pushes %d; record fw %d → %d, tenant Σfw %d → %d", err, targetSeq(d)-seq0, fw0, fw1, sum0, sum1)
	if !errors.Is(err, ErrWaitExpired) || targetSeq(d)-seq0 != 1 {
		t.Fatalf("request = %v after %d pushes, want predicted after its push", err, targetSeq(d)-seq0)
	}
	if fw0 != 2 || fw1 != fw0 || sum1 != sum0 {
		t.Fatalf("record fw %d → %d, Σfw %d → %d, want 2 → 2 (a refusal moves no share)", fw0, fw1, sum0, sum1)
	}
	if diff, _ := d.fairAuditForTest(testOrigin); diff != "" {
		t.Fatalf("fairness bookkeeping after the refusal: %s", diff)
	}
}

// fairPrincipalKey is the record key of consumer of tenant.
func fairPrincipalKey(consumer, tenant string) string {
	return Principal{ConsumerID: consumer, TenantID: tenant, ConsumerKind: "app"}.identityKey()
}
