package dispatch

// Hardening of A03-W9 after review 2: r̂ per held lease with the L_p50 of
// the lease's own external class (F2-1), the four gaps of the hardening and
// P1 delta (F2-9: C04, C05, C19, C21), and the named starvation bound of XB
// (F2-2).

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ageLeases sets the admission instant of each held lease to now − its age,
// under the mutex (r̂ reads the admission instants of the held external
// leases).
func ageLeases(d *Dispatcher, ages map[*Lease]time.Duration) {
	d.mu.Lock()
	now := time.Now()
	for l, age := range ages {
		l.admitted = now.Add(-age)
	}
	d.mu.Unlock()
}

// secondSteps is a Retry-After ladder in whole seconds 1..60, so Q(ŵ) reads
// ŵ to the second (rounded up).
func secondSteps() []time.Duration {
	steps := make([]time.Duration, 0, 60)
	for s := 1; s <= 60; s++ {
		steps = append(steps, time.Duration(s)*time.Second)
	}
	return steps
}

func retryAfterOfErr(t *testing.T, err error, what string) time.Duration {
	t.Helper()
	var re *RejectionError
	if !errors.As(err, &re) {
		t.Fatalf("%s: %v, want a RejectionError", what, err)
	}
	return re.RetryAfter
}

// TestExternalFirstRoundPerLeaseClass pins r̂ per held lease with the L_p50
// of the lease's OWN class (F2-1, lead decision H-1): r̂ = min over the held
// external leases of max(0, p50(class of the lease) − its age). E = 2 (S 3,
// R 1), both windows warm.
func TestExternalFirstRoundPerLeaseClass(t *testing.T) {
	bg := context.Background()

	// (ii) Long bulk leases in the XB window (L_p50 60 s), short XU leases
	// (L_p50 1 s, admitted 500 ms ago) hold E: the first unit frees in about
	// 0.5 s. An XB acquire with the default 20 s budget queues and is served
	// at the next XU release — never predicted (r̂ with the XB p50 would be
	// 59.5 s, past the budget).
	t.Run("short XU holders, long XB window: XB queues and is served", func(t *testing.T) {
		d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(3, 1))
		a := mustAcquire(t, d, xctx(bg, "h0", "tH"), externalReq())
		b := mustAcquire(t, d, xctx(bg, "h1", "tH"), externalReq())
		defer b.Release()
		ageLeases(d, map[*Lease]time.Duration{a: 500 * time.Millisecond, b: 500 * time.Millisecond})
		d.SeedLeaseSamplesForTest(testOrigin, ClassExternal, 2*leaseColdSamples, time.Second)
		d.SeedLeaseSamplesForTest(testOrigin, ClassExternalBulk, 2*leaseColdSamples, time.Minute)
		ctx, cancel := context.WithCancel(bg)
		defer cancel()
		ch := make(chan admission, 1)
		startWaiter(xctx(ctx, "x", "tX"), d, "XB", xbReq(), ch)
		waitFor(t, "XB queued or refused", func() bool {
			return waitingBulk(d) == 1 || target(t, d).ExternalBulkRejects != (ExternalRejectStats{})
		})
		if rej := target(t, d).ExternalBulkRejects; rej.Predicted != 0 {
			t.Fatalf("XB rejects %+v behind short XU holders (r̂ ≈ 0.5 s), want predicted 0 — r̂ read the XU leases with the XB L_p50", rej)
		}
		a.Release()
		got := recvAdmission(t, ch, "XB after the XU release")
		if got.err != nil || got.lease == nil || got.lease.Class() != ClassExternalBulk {
			t.Fatalf("XB after the XU release: (lease %v, %v), want an XB lease", got.lease != nil, got.err)
		}
		got.lease.Release()
	})

	// (i) The other direction: long XU leases (L_p50 100 s, admitted 5 s
	// ago) hold E, the XB window is short (L_p50 10 s). The XB rejection
	// carries the Retry-After of the long remaining time (r̂ = 95 s ⇒ Q =
	// 120 s), not 10 − 5 = 5 s; and at scale 1:100 (XU 1 s admitted 50 ms
	// ago, XB 100 ms, budget 300 ms) the XB acquire is predicted at once
	// instead of queued until its budget expires.
	t.Run("long XU holders, short XB window: XB refused with the long remaining time", func(t *testing.T) {
		s := DefaultSettings()
		s.ExternalRetryAfterSteps = []time.Duration{time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, 60 * time.Second, 120 * time.Second}
		d, _ := newTestDispatcher(t, s, partitionPolicy(3, 1))
		a := mustAcquire(t, d, xctx(bg, "h0", "tH"), externalReq())
		b := mustAcquire(t, d, xctx(bg, "h1", "tH"), externalReq())
		defer a.Release()
		defer b.Release()
		ageLeases(d, map[*Lease]time.Duration{a: 5 * time.Second, b: 5 * time.Second})
		d.SeedLeaseSamplesForTest(testOrigin, ClassExternal, 2*leaseColdSamples, 100*time.Second)
		d.SeedLeaseSamplesForTest(testOrigin, ClassExternalBulk, 2*leaseColdSamples, 10*time.Second)
		_, _, err := d.TryAcquire(xctx(bg, "x", "tX"), xbReq())
		if !errors.Is(err, ErrWouldBlock) {
			t.Fatalf("XB TryAcquire on a full E: %v, want ErrWouldBlock", err)
		}
		if ra := retryAfterOfErr(t, err, "XB TryAcquire"); ra != 120*time.Second {
			t.Fatalf("XB Retry-After %v behind XU leases with 95 s left, want 120s (Q(95 s)) — r̂ read the XU leases with the XB L_p50", ra)
		}
	})
	t.Run("long XU holders, short XB window, scale 1:100: XB predicted at once", func(t *testing.T) {
		s := DefaultSettings()
		s.ExternalMaxWait = 300 * time.Millisecond
		d, _ := newTestDispatcher(t, s, partitionPolicy(3, 1))
		a := mustAcquire(t, d, xctx(bg, "h0", "tH"), externalReq())
		b := mustAcquire(t, d, xctx(bg, "h1", "tH"), externalReq())
		defer a.Release()
		defer b.Release()
		ageLeases(d, map[*Lease]time.Duration{a: 50 * time.Millisecond, b: 50 * time.Millisecond})
		d.SeedLeaseSamplesForTest(testOrigin, ClassExternal, 2*leaseColdSamples, time.Second)
		d.SeedLeaseSamplesForTest(testOrigin, ClassExternalBulk, 2*leaseColdSamples, 100*time.Millisecond)
		start := time.Now()
		_, _, err := d.Acquire(xctx(bg, "x", "tX"), xbReq())
		el := time.Since(start)
		if !errors.Is(err, ErrWaitExpired) || el > 50*time.Millisecond {
			t.Fatalf("XB Acquire (300 ms budget, XU leases with 950 ms left): %v after %v, want a predicted ErrWaitExpired at once", err, el)
		}
		if rej := target(t, d).ExternalBulkRejects; rej != (ExternalRejectStats{Predicted: 1}) {
			t.Fatalf("XB rejects %+v, want predicted 1 (queued and expired: r̂ read the XU leases with the XB L_p50)", rej)
		}
	})

	// (iii) XU behind held XB leases (XB L_p50 1 s, admitted 50 ms ago; XU
	// L_p50 100 ms, budget 300 ms): before A03-W9 only XU leases held E; the
	// XU acquire is predicted at once instead of queued too optimistically
	// (r̂ = 100 − 50 ms with the XU p50) — the fast path of the rounds bound
	// included (it bounds r̂ by the largest L_p50 of a class that holds).
	t.Run("XU behind XB holders: XU predicted", func(t *testing.T) {
		s := DefaultSettings()
		s.ExternalMaxWait = 300 * time.Millisecond
		d, _ := newTestDispatcher(t, s, partitionPolicy(3, 1))
		a := mustAcquire(t, d, xctx(bg, "h0", "tH"), xbReq())
		b := mustAcquire(t, d, xctx(bg, "h1", "tH"), xbReq())
		defer a.Release()
		defer b.Release()
		ageLeases(d, map[*Lease]time.Duration{a: 50 * time.Millisecond, b: 50 * time.Millisecond})
		d.SeedLeaseSamplesForTest(testOrigin, ClassExternalBulk, 2*leaseColdSamples, time.Second)
		d.SeedLeaseSamplesForTest(testOrigin, ClassExternal, 2*leaseColdSamples, 100*time.Millisecond)
		start := time.Now()
		_, _, err := d.Acquire(xctx(bg, "u", "tU"), externalReq())
		el := time.Since(start)
		if !errors.Is(err, ErrWaitExpired) || el > 50*time.Millisecond {
			t.Fatalf("XU Acquire (300 ms budget, XB leases with 950 ms left): %v after %v, want a predicted ErrWaitExpired at once", err, el)
		}
		if rej := target(t, d).ExternalRejects; rej != (ExternalRejectStats{Predicted: 1}) {
			t.Fatalf("XU rejects %+v, want predicted 1 (queued and expired: r̂ read the XB leases with the XU L_p50)", rej)
		}
	})
}

// TestExternalFirstRoundFairnessFirstHead covers the third caller of r̂
// (H-1: prediction after the push, Retry-After of a rejection, and the
// fairness-first exception): E = 2 held by XU leases (L_p50 1 s, admitted
// 500 ms ago), XB window 60 s. Two XB tenants with slot time queue (one
// round each, ŵ = r̂ ≈ 0.5 s); a fresh third tenant's XB acquire needs ⌈3 /
// 2⌉ = 2 rounds at its share (ŵ ≈ 60.5 s > 20 s) but is the fairness-first
// head — it waits its first round only, r̂ ≈ 0.5 s, so it queues and is the
// first XB admission at the next XU release. With the XB p50 for the XU
// leases r̂ would be 59.5 s and all three would be predicted.
func TestExternalFirstRoundFairnessFirstHead(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(3, 1))
	clk := fakeFairClock(d)
	bg := context.Background()
	for _, tn := range []string{"tA", "tB"} {
		l := mustAcquire(t, d, xctx(bg, "c"+tn, tn), xbReq())
		clk.Add(int64(10 * time.Second))
		l.Release()
	}
	a := mustAcquire(t, d, xctx(bg, "h0", "tH"), externalReq())
	b := mustAcquire(t, d, xctx(bg, "h1", "tH"), externalReq())
	defer b.Release()
	ageLeases(d, map[*Lease]time.Duration{a: 500 * time.Millisecond, b: 500 * time.Millisecond})
	d.SeedLeaseSamplesForTest(testOrigin, ClassExternal, 2*leaseColdSamples, time.Second)
	d.SeedLeaseSamplesForTest(testOrigin, ClassExternalBulk, 2*leaseColdSamples, time.Minute)
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 3)
	for i, tn := range []string{"tA", "tB", "tN"} {
		startWaiter(xctx(ctx, "c"+tn, tn), d, tn, xbReq(), ch)
		waitFor(t, tn+" queued or refused", func() bool {
			return waitingBulk(d) == i+1 || target(t, d).ExternalBulkRejects != (ExternalRejectStats{})
		})
		if rej := target(t, d).ExternalBulkRejects; rej != (ExternalRejectStats{}) {
			t.Fatalf("XB %s: rejects %+v, want it queued (r̂ ≈ 0.5 s of the XU holders at their own L_p50)", tn, rej)
		}
	}
	a.Release()
	got := recvAdmission(t, ch, "the first XB admission")
	if got.err != nil || got.label != "tN" {
		t.Fatalf("first XB admission: %s (%v), want tN — the fresh tenant, the fairness-first head", got.label, got.err)
	}
	got.lease.Release()
	for i := 0; i < 2; i++ {
		if a := recvAdmission(t, ch, "the other XB waiters"); a.err == nil {
			a.lease.Release()
		}
	}
}

// TestExternalFirstRoundMixedHolders is the formula gate of H-1 through the
// wire value: E = 4 (S 5, R 1) held by two XU and two XB leases of different
// ages, a refused newcomer's position is one round (ŵ = r̂), and the
// Retry-After ladder in whole seconds reads r̂ rounded up. Hand-computed:
//
//	warm, XU L_p50 10 s, XB L_p50 30 s; XU ages 4 s, 7 s (6 s, 3 s left),
//	XB ages 20 s, 25 s (10 s, 5 s left) ⇒ r̂ = 3 s for XU and XB
//	(before: XB 30 − 25 = 5 s, XU max(0, 10 − 25) = 0 ⇒ 1 s);
//	warm, XU ages 2 s, 4 s (8 s, 6 s left), XB ages 20 s, 26 s (10 s, 4 s
//	left) ⇒ r̂ = 4 s, the minimum from an XB lease (before: XU 0 ⇒ 1 s);
//	XB window cold: the XB leases count with the XU L_p50 10 s, XU ages
//	4 s, 7 s (6 s, 3 s), XB ages 6 s, 8 s (4 s, 2 s) ⇒ r̂ = 2 s for XU — the
//	cold rule (unchanged: one p50 for all leases).
func TestExternalFirstRoundMixedHolders(t *testing.T) {
	bg := context.Background()
	cases := []struct {
		name         string
		xbWarm       bool
		xuAges       [2]time.Duration
		xbAges       [2]time.Duration
		wantXU       time.Duration
		wantXB       time.Duration // 0: XB has no prediction (its window is cold)
	}{
		{"both warm, minimum from an XU lease", true, [2]time.Duration{4 * time.Second, 7 * time.Second}, [2]time.Duration{20 * time.Second, 25 * time.Second}, 3 * time.Second, 3 * time.Second},
		{"both warm, minimum from an XB lease", true, [2]time.Duration{2 * time.Second, 4 * time.Second}, [2]time.Duration{20 * time.Second, 26 * time.Second}, 4 * time.Second, 4 * time.Second},
		{"XB window cold", false, [2]time.Duration{4 * time.Second, 7 * time.Second}, [2]time.Duration{6 * time.Second, 8 * time.Second}, 2 * time.Second, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := DefaultSettings()
			s.ExternalRetryAfterSteps = secondSteps()
			d, _ := newTestDispatcher(t, s, partitionPolicy(5, 1))
			ages := map[*Lease]time.Duration{}
			for i, age := range tc.xuAges {
				l := mustAcquire(t, d, xctx(bg, fmt.Sprintf("u%d", i), "tH"), externalReq())
				defer l.Release()
				ages[l] = age
			}
			for i, age := range tc.xbAges {
				l := mustAcquire(t, d, xctx(bg, fmt.Sprintf("b%d", i), "tH"), xbReq())
				defer l.Release()
				ages[l] = age
			}
			ageLeases(d, ages)
			d.SeedLeaseSamplesForTest(testOrigin, ClassExternal, 2*leaseColdSamples, 10*time.Second)
			if tc.xbWarm {
				d.SeedLeaseSamplesForTest(testOrigin, ClassExternalBulk, 2*leaseColdSamples, 30*time.Second)
			}
			_, _, err := d.TryAcquire(xctx(bg, "nu", "tN"), externalReq())
			if ra := retryAfterOfErr(t, err, "XU TryAcquire"); ra != tc.wantXU {
				t.Errorf("XU Retry-After %v, want %v (r̂ by hand)", ra, tc.wantXU)
			}
			if tc.wantXB == 0 {
				return
			}
			_, _, err = d.TryAcquire(xctx(bg, "nb", "tN"), xbReq())
			if ra := retryAfterOfErr(t, err, "XB TryAcquire"); ra != tc.wantXB {
				t.Errorf("XB Retry-After %v, want %v (r̂ by hand)", ra, tc.wantXB)
			}
		})
	}
}

// TestExternalXUPositionIgnoresWaitingBulk (F2-9 C19): P1 puts ahead = 0
// for XU — waiting XB weight never lengthens an XU position, so it never
// sheds an XU acquire. E = 2 full, the holders past L_p50 (r̂ = 0), 4 XB
// units waiting; an XU acquire with a 150 ms budget queues (one round) and
// expires — never predicted.
func TestExternalXUPositionIgnoresWaitingBulk(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(4, 2))
	d.SeedLeaseSamplesForTest(testOrigin, ClassExternal, 2*leaseColdSamples, 100*time.Millisecond)
	d.SeedLeaseSamplesForTest(testOrigin, ClassExternalBulk, 2*leaseColdSamples, 100*time.Millisecond)
	bg := context.Background()
	for i := 0; i < 2; i++ {
		l := mustAcquire(t, d, xctx(bg, fmt.Sprintf("o%d", i), "tenant-o"), externalReq())
		defer l.Release()
		ageLeases(d, map[*Lease]time.Duration{l: 120 * time.Millisecond})
	}
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 2)
	for i, n := range []string{"a", "b"} {
		r := xbWeighted(2)
		r.MaxWait = 5 * time.Second
		startWaiter(xctx(ctx, n, "tenant-b"+n), d, "XB"+n, r, ch)
		waitFor(t, "XB queued", func() bool { return waitingBulk(d) == i+1 })
	}
	xu := externalReq()
	xu.MaxWait = 150 * time.Millisecond
	_, _, err := d.Acquire(xctx(bg, "u", "tenant-u"), xu)
	if !errors.Is(err, ErrWaitExpired) {
		t.Fatalf("XU: %v, want ErrWaitExpired", err)
	}
	if rej := target(t, d).ExternalRejects; rej.Predicted != 0 || rej.Expired != 1 {
		t.Fatalf("XU rejects %+v, want expired 1 / predicted 0 — waiting XB weight counted ahead of XU (P1: 0 for XU)", rej)
	}
}

// TestExternalBulkHeadRoundsCountItsWeight (F2-9 C21): the fairness-first
// XB head waits ⌈(ahead + w) / E⌉ rounds with its REAL weight: ahead 1 (one
// waiting XU unit), w 2, E 2 ⇒ 2 rounds = 100 ms > 90 ms budget ⇒ predicted
// at once (with w read as 1: one round, r̂ = 0 ≤ 90 ms ⇒ queued, expired at
// the budget). The counter decides; "at once" is the house bound of 50 ms,
// well below the 90 ms budget the mutant waits out.
func TestExternalBulkHeadRoundsCountItsWeight(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(4, 2))
	d.SeedLeaseSamplesForTest(testOrigin, ClassExternal, 2*leaseColdSamples, 100*time.Millisecond)
	d.SeedLeaseSamplesForTest(testOrigin, ClassExternalBulk, 2*leaseColdSamples, 100*time.Millisecond)
	bg := context.Background()
	for i := 0; i < 2; i++ {
		l := mustAcquire(t, d, xctx(bg, fmt.Sprintf("o%d", i), "tenant-o"), externalReq())
		defer l.Release()
		ageLeases(d, map[*Lease]time.Duration{l: 120 * time.Millisecond})
	}
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(xctx(ctx, "a", "tenant-a"), d, "XUa", externalReq(), ch)
	waitQueued(t, d, ch, 1, "XU a queued")
	xb := xbWeighted(2)
	xb.MaxWait = 90 * time.Millisecond
	start := time.Now()
	_, _, err := d.Acquire(xctx(bg, "x", "tenant-x"), xb)
	if el := time.Since(start); !errors.Is(err, ErrWaitExpired) || el > 50*time.Millisecond {
		t.Fatalf("XB w=2 behind 1 XU unit: %v after %v, want a predicted ErrWaitExpired at once", err, el)
	}
	if rej := target(t, d).ExternalBulkRejects; rej != (ExternalRejectStats{Predicted: 1}) {
		t.Fatalf("XB rejects %+v, want predicted 1", rej)
	}
}

// TestEscapeYieldsOnInHouseTryAcquire (F2-9 C04, H-3): the yield covers any
// refusal without an enqueue wake, of every class — the in-house TryAcquire
// too (events/backfill_admission.go is such a caller). A background
// TryAcquire refused under the herald beside an escape-capable aged
// background head hands that head the free unit at once.
func TestEscapeYieldsOnInHouseTryAcquire(t *testing.T) {
	const aging = 20 * time.Millisecond
	s := agingSettings(aging)
	s.RankOrder = mustRankOrder(t, ExternalOrderO1)
	pol := Policy{Targets: map[string]TargetPolicy{testOrigin: {Slots: 2, InternalReserved: 1}}}
	d, _ := newTestDispatcher(t, s, pol)
	il := mustAcquire(t, d, ictx(principal("i")), interactiveReq())
	defer il.Release()
	done := d.InteractiveArrived()
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(ctx, d, "B", backgroundReq(), ch)
	waitFor(t, "B queued behind the herald", func() bool { return waitingBackground(d) == 1 })
	time.Sleep(3 * aging)
	start := time.Now()
	l, _, err := d.TryAcquire(context.Background(), backgroundReq())
	if l != nil || !errors.Is(err, ErrWouldBlock) {
		t.Fatalf("background TryAcquire = (lease %v, %v), want ErrWouldBlock", l != nil, err)
	}
	select {
	case a := <-ch:
		if a.err != nil || time.Since(start) > 50*time.Millisecond {
			t.Fatalf("%s (%v) after %v, want the aged head within 50ms", a.label, a.err, time.Since(start))
		}
		a.lease.Release()
	case <-time.After(150 * time.Millisecond):
		t.Fatalf("background TryAcquire refused and the aged background head still waits 150ms later with a unit free")
	}
}

// TestRefusalBesideABackgroundWaiterIsNoEvent (F2-9 C05): the yield wakes
// only when the background head escapes now (backgroundEscapesLocked) — a
// refusal beside a background waiter that is not escape-capable (aging off,
// behind d2) is no event of the fairness order (FR4-2). The fixture of
// TestRefusedNewcomerIsNoEvent plus one background waiter.
func TestRefusalBesideABackgroundWaiterIsNoEvent(t *testing.T) {
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
	bch := make(chan admission, 1)
	startWaiter(ctx, d, "B", backgroundReq(), bch)
	waitFor(t, "B queued", func() bool { return waitingBackground(d) == 1 })
	clk.Add(int64(200 * time.Second))
	seedLeases(d, testOrigin, waitRingK, 10*time.Second)
	rn := weightedReq(4)
	rn.MaxWait = time.Second
	if l, _, err := d.Acquire(fairXctx(bg, "n", "tn", 1), rn); l != nil || !errors.Is(err, ErrWaitExpired) {
		t.Fatalf("newcomer n = (lease %v, %v), want predicted", l != nil, err)
	}
	select {
	case a := <-ch:
		t.Fatalf("%s admitted at the refused newcomer (err %v) beside a background waiter — the refusal acted as an event", a.label, a.err)
	case <-time.After(20 * time.Millisecond):
	}
}

// TestExternalBulkStarvationEndsAtTheBudget pins the named bound of XB
// (F2-2, H-4): under sustained urgent load above the external capacity bulk
// is never served — a light and a heavy XB acquire end at the wait budget
// with ErrWaitExpired (reason expired) and a Retry-After, no goroutine left
// behind (external_bulk_aging_after is named, not built — design/03 §4.8:
// an escape over background only, never over interactive or urgent). With
// urgent load below E both are served (control).
func TestExternalBulkStarvationEndsAtTheBudget(t *testing.T) {
	const budget = 300 * time.Millisecond
	for _, load := range []string{"below E", "above E"} {
		t.Run(load, func(t *testing.T) {
			s := DefaultSettings()
			s.ExternalMaxWait = budget
			d, _ := newTestDispatcher(t, s, partitionPolicy(3, 1))
			g0 := runtime.NumGoroutine()
			stop := make(chan struct{})
			var wg sync.WaitGroup
			// Below E one urgent looper holds at most one of the two units
			// and frees it between its leases; above E four loopers keep an
			// urgent waiter queued at every release.
			loopers, gap := 1, time.Millisecond
			if load == "above E" {
				loopers, gap = 4, 0
			}
			var xuLeases atomic.Int64
			for i := 0; i < loopers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					for {
						select {
						case <-stop:
							return
						default:
						}
						if l, _, err := d.Acquire(xctx(context.Background(), fmt.Sprintf("u%d", i), "tU"), externalReq()); err == nil {
							xuLeases.Add(1)
							time.Sleep(8 * time.Millisecond)
							l.Release()
						}
						time.Sleep(gap)
					}
				}(i)
			}
			waitFor(t, "XU load running", func() bool { return xuLeases.Load() >= 4 })
			for _, w := range []int{1, 2} {
				ctx, cancel := context.WithTimeout(xctx(context.Background(), fmt.Sprintf("xb%d", w), "tB"), 3*time.Second)
				start := time.Now()
				l, _, err := d.Acquire(ctx, xbWeighted(w))
				el := time.Since(start)
				cancel()
				switch {
				case load == "below E" && (err != nil || l == nil):
					t.Fatalf("below E: XB w=%d: %v after %v, want an XB lease (bulk is served while urgent leaves units)", w, err, el)
				case load == "above E" && (l != nil || !errors.Is(err, ErrWaitExpired) || el < budget-10*time.Millisecond || el > budget+200*time.Millisecond):
					t.Fatalf("above E: XB w=%d: (lease %v, %v) after %v, want ErrWaitExpired at the %v budget", w, l != nil, err, el, budget)
				case load == "above E" && retryAfterOfErr(t, err, "XB at the budget") <= 0:
					t.Fatalf("above E: XB w=%d expired without a Retry-After", w)
				}
				if l != nil {
					l.Release()
				}
			}
			close(stop)
			wg.Wait()
			if load == "above E" {
				if rej := target(t, d).ExternalBulkRejects; rej.Expired != 2 || rej.Predicted != 0 {
					t.Fatalf("XB rejects %+v, want expired 2 / predicted 0", rej)
				}
			}
			waitFor(t, "no goroutine left behind", func() bool { return runtime.NumGoroutine() <= g0 })
		})
	}
}

// TestFirstRoundLockedByHand pins the r̂ term itself at a fixed instant
// (H-1): r̂ = min over the held external leases of max(0, p50[class of the
// lease] − its age), p50 per class from roundLengthsFor (its own window when
// warm, else the acquire's own L_p50), 0 without a held external lease, and
// firstRoundBound ≥ r̂. Every value is computed by hand in the case table.
func TestFirstRoundLockedByHand(t *testing.T) {
	const s = time.Second
	type lease struct {
		class Class
		age   time.Duration
		cont  bool
	}
	cases := []struct {
		name           string
		xuP50, xbP50   time.Duration // 0: the window stays cold
		xbExtra        int           // further XB samples seeded before the second read (window turns warm)
		leases         []lease
		req            Class
		want, wantNext time.Duration // wantNext: r̂ after xbExtra samples (xbExtra > 0)
		wantBound      time.Duration
	}{
		{"both warm, XU requester", 10 * s, 30 * s, 0,
			[]lease{{ClassExternal, 4 * s, false}, {ClassExternal, 7 * s, false}, {ClassExternalBulk, 20 * s, false}, {ClassExternalBulk, 25 * s, false}},
			ClassExternal, 3 * s, 0, 30 * s},
		{"both warm, XB requester", 10 * s, 30 * s, 0,
			[]lease{{ClassExternal, 4 * s, false}, {ClassExternal, 7 * s, false}, {ClassExternalBulk, 20 * s, false}, {ClassExternalBulk, 25 * s, false}},
			ClassExternalBulk, 3 * s, 0, 30 * s},
		{"XB window cold: XB leases at the XU p50", 10 * s, 0, 0,
			[]lease{{ClassExternal, 4 * s, false}, {ClassExternal, 7 * s, false}, {ClassExternalBulk, 6 * s, false}, {ClassExternalBulk, 8 * s, false}},
			ClassExternal, 2 * s, 0, 10 * s},
		{"XU window cold: XU leases at the XB p50", 0, 30 * s, 0,
			[]lease{{ClassExternal, 26 * s, false}, {ClassExternal, 27 * s, false}, {ClassExternalBulk, 20 * s, false}, {ClassExternalBulk, 25 * s, false}},
			ClassExternalBulk, 3 * s, 0, 30 * s},
		{"a lease past its class's p50 clamps at 0", 10 * s, 30 * s, 0,
			[]lease{{ClassExternal, 12 * s, false}, {ClassExternal, 4 * s, false}, {ClassExternalBulk, 20 * s, false}, {ClassExternalBulk, 25 * s, false}},
			ClassExternal, 0, 0, 30 * s},
		{"no external lease held", 10 * s, 30 * s, 0, nil, ClassExternal, 0, 0, 10 * s},
		{"only XU held: the A03-W6 term for XU", 10 * s, 30 * s, 0,
			[]lease{{ClassExternal, 4 * s, false}, {ClassExternal, 7 * s, false}},
			ClassExternal, 3 * s, 0, 10 * s},
		{"an XB continuation lease counts at the XB p50", 10 * s, 30 * s, 0,
			[]lease{{ClassExternal, 4 * s, false}, {ClassExternalBulk, 29 * s, true}},
			ClassExternal, 1 * s, 0, 30 * s},
		{"the XB window turns warm while the leases are held", 10 * s, 0, leaseColdSamples,
			[]lease{{ClassExternal, 4 * s, false}, {ClassExternal, 7 * s, false}, {ClassExternalBulk, 6 * s, false}, {ClassExternalBulk, 8 * s, false}},
			ClassExternal, 2 * s, 3 * s, 10 * s},
	}
	bg := context.Background()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(7, 1))
			var held []*Lease
			for i, l := range tc.leases {
				r := externalReq()
				r.Class, r.Continuation = l.class, l.cont
				held = append(held, mustAcquire(t, d, xctx(bg, fmt.Sprintf("c%d", i), "tH"), r))
			}
			defer func() {
				for _, l := range held {
					l.Release()
				}
			}()
			if tc.xuP50 > 0 {
				d.SeedLeaseSamplesForTest(testOrigin, ClassExternal, 2*leaseColdSamples, tc.xuP50)
			}
			if tc.xbP50 > 0 {
				d.SeedLeaseSamplesForTest(testOrigin, ClassExternalBulk, 2*leaseColdSamples, tc.xbP50)
			}
			read := func() (time.Duration, time.Duration) {
				d.mu.Lock()
				defer d.mu.Unlock()
				st := d.targetLocked(canonicalOrigin(testOrigin))
				now := time.Now()
				for i, l := range held {
					l.admitted = now.Add(-tc.leases[i].age)
				}
				lv := st.externalLeaseView(tc.req)
				if !lv.warm {
					t.Fatalf("requester window of %s cold — the fixture needs it warm", tc.req)
				}
				p50 := st.roundLengthsFor(tc.req, lv.p50)
				return st.firstRoundLocked(p50, now), st.firstRoundBound(p50, lv.p50)
			}
			got, bound := read()
			if got != tc.want || bound != tc.wantBound {
				t.Fatalf("r̂ = %v, bound %v; want %v, bound %v (by hand)", got, bound, tc.want, tc.wantBound)
			}
			if tc.xbExtra == 0 {
				return
			}
			// The window was seeded 0 times: leaseColdSamples − 1 samples keep
			// it cold, the next one makes it warm at 30 s.
			d.SeedLeaseSamplesForTest(testOrigin, ClassExternalBulk, tc.xbExtra-1, 30*s)
			if got, _ := read(); got != tc.want {
				t.Fatalf("one sample short of warm: r̂ = %v, want %v (cold rule)", got, tc.want)
			}
			d.SeedLeaseSamplesForTest(testOrigin, ClassExternalBulk, 1, 30*s)
			if got, bound := read(); got != tc.wantNext || bound != 30*s {
				t.Fatalf("XB window warm: r̂ = %v, bound %v; want %v, bound 30s", got, bound, tc.wantNext)
			}
		})
	}
}

// TestFirstRoundAllocFree: r̂ per lease class (H-1) reads both published
// windows once and makes one pass over the held external leases without an
// allocation — E = 64 held by 32 XU and 32 XB leases, both windows warm.
func TestFirstRoundAllocFree(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(65, 1))
	bg := context.Background()
	for i := 0; i < 64; i++ {
		r := externalReq()
		if i%2 == 1 {
			r = xbReq()
		}
		l := mustAcquire(t, d, xctx(bg, fmt.Sprintf("c%d", i), fmt.Sprintf("t%d", i%8)), r)
		defer l.Release()
	}
	d.SeedLeaseSamplesForTest(testOrigin, ClassExternal, 2*leaseColdSamples, 10*time.Second)
	d.SeedLeaseSamplesForTest(testOrigin, ClassExternalBulk, 2*leaseColdSamples, 30*time.Second)
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.targetLocked(canonicalOrigin(testOrigin))
	now := time.Now()
	for _, c := range []Class{ClassExternal, ClassExternalBulk} {
		own := st.externalLeaseView(c).p50
		var first time.Duration
		allocs := testing.AllocsPerRun(1000, func() {
			p50 := st.roundLengthsFor(c, own)
			first = st.firstRoundLocked(p50, now) + st.firstRoundBound(p50, own)
		})
		if allocs != 0 {
			t.Errorf("r̂ for %s: %v allocations per call, want 0", c, allocs)
		}
		if first <= 0 {
			t.Errorf("r̂ + bound for %s = %v, want > 0 (64 fresh leases)", c, first)
		}
	}
}
