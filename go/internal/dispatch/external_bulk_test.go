package dispatch

// A03-W9 gates (design/03 §4.1 C7 column XB, §4.2, §4.5 no. 2, §4.6, §4.9;
// DECISIONS E2: o1 I ≻ XU ≻ B ≻ XB, o2 I ≻ B ≻ XU ≻ XB): the external bulk
// class XB — its class row, the rank at the handoff in both orders, the one
// external partition with XU, its own cap counters, the hold and d2 of a
// waiting XU head binding it (externalClaimLocked), the escape of an aged
// background head against it, I-03-3 (never a victim, reaper-only cancel,
// the kill-switch drain), the refusal counters per class and the prediction
// per class (lead decision P1: the waiting XU weight ahead plus the own
// position, the XB window). The principal travels ONLY in the ctx through
// the TestMain hook, like every gate of this package.

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// xbReq is an external bulk request of weight 1 on the test origin.
func xbReq() Request {
	r := externalReq()
	r.Class = ClassExternalBulk
	return r
}

// xbWeighted is an external bulk request of weight w.
func xbWeighted(w int) Request {
	r := xbReq()
	r.Weight = w
	return r
}

func waitingBulk(d *Dispatcher) int {
	for _, ts := range d.Snapshot().Targets {
		if ts.Origin == testOrigin {
			return ts.ExternalBulk.Waiting
		}
	}
	return 0
}

// TestExternalBulkLeaseClass is the dispatcher half of gate (a): an XB
// acquire of a consumer principal is admitted as an XB lease inside the
// external partition — Class() is ClassExternalBulk, it holds external units
// (HeldExternal, HeldExternalWeight), never internal ones — through both
// doors; without a consumer principal both doors refuse it (K-03-1).
func TestExternalBulkLeaseClass(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(4, 2))
	bg := context.Background()
	l := mustAcquire(t, d, xctx(bg, "b1", "tenant-a"), xbWeighted(2))
	if l.Class() != ClassExternalBulk || !l.slotHeld || l.weight != 2 {
		t.Fatalf("XB lease: class %s slotHeld %v weight %d, want external_bulk/true/2", l.Class(), l.slotHeld, l.weight)
	}
	ts := target(t, d)
	if ts.HeldExternal != 1 || ts.HeldExternalWeight != 2 || ts.HeldInternal != 0 || ts.HeldWeight != 2 {
		t.Fatalf("held external %d (Σw %d) internal %d Σw %d, want 1/2/0/2", ts.HeldExternal, ts.HeldExternalWeight, ts.HeldInternal, ts.HeldWeight)
	}
	l.Release()
	tl, _, err := d.TryAcquire(xctx(bg, "b2", "tenant-a"), xbReq())
	if err != nil || tl.Class() != ClassExternalBulk {
		t.Fatalf("TryAcquire XB = (%v, %v), want an external_bulk lease", tl, err)
	}
	tl.Release()
	for name, door := range map[string]func(*Dispatcher, context.Context, Request) (*Lease, context.Context, error){
		"Acquire": (*Dispatcher).Acquire, "TryAcquire": (*Dispatcher).TryAcquire,
	} {
		for label, ctx := range map[string]context.Context{
			"knowledge key": ictx(principal("k")),
			"zero":          bg,
		} {
			if l, _, err := door(d, ctx, xbReq()); !errors.Is(err, ErrClassUnauthorized) || l != nil {
				t.Errorf("%s XB with a %s principal = (lease %v, %v), want ErrClassUnauthorized", name, label, l != nil, err)
			}
		}
	}
}

// TestExternalBulkRankAtHandoff is gate (d), the order: the target is full
// with interactive leases, one waiter per class queued in reverse rank order
// (XB first). Each release frees one unit; the handoffs follow the rank
// table — o1 I, XU, B, XB; o2 I, B, XU, XB — XB last in both, behind a
// background head with demand 0.
func TestExternalBulkRankAtHandoff(t *testing.T) {
	for order, want := range map[string][]string{
		ExternalOrderO1: {"I", "XU", "B", "XB"},
		ExternalOrderO2: {"I", "B", "XU", "XB"},
	} {
		t.Run(order, func(t *testing.T) {
			s := DefaultSettings()
			s.RankOrder = mustRankOrder(t, order)
			d, _ := newTestDispatcher(t, s, partitionPolicy(4, 2))
			var occ []*Lease
			for i := 0; i < 4; i++ {
				occ = append(occ, mustAcquire(t, d, ictx(principal(fmt.Sprintf("o%d", i))), interactiveReq()))
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch := make(chan admission, 4)
			startWaiter(xctx(ctx, "xb", "tenant-a"), d, "XB", xbReq(), ch)
			waitFor(t, "XB queued", func() bool { return waitingBulk(d) == 1 })
			startWaiter(ctx, d, "B", backgroundReq(), ch)
			waitFor(t, "B queued", func() bool { return waitingBackground(d) == 1 })
			startWaiter(xctx(ctx, "xu", "tenant-a"), d, "XU", externalReq(), ch)
			waitFor(t, "XU queued", func() bool { return waitingExternal(d) == 1 })
			startWaiter(withPrincipal(ctx, principal("i")), d, "I", interactiveReq(), ch)
			waitFor(t, "I queued", func() bool { return waitingInteractive(d) == 1 })
			var got []string
			for _, l := range occ {
				l.Release()
				a := recvAdmission(t, ch, "handoff")
				if a.err != nil {
					t.Fatalf("%s: %v", a.label, a.err)
				}
				got = append(got, a.label)
				defer a.lease.Release()
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("%s handoff order %v, want %v", order, got, want)
			}
		})
	}
}

// TestExternalBulkTakesOnlyExternalUnitsBehindTheHerald is gate (d), the
// partition: S = 4, R = 2 (E = 2), demand > 0, one interactive lease, a
// background waiter behind the herald. XB takes the two external units and
// no more — a third XB waits although an internal unit is free — and the
// internal side is untouched (heldInt = 1); when the demand ends, the
// background waiter gets the free internal unit, not the waiting XB.
func TestExternalBulkTakesOnlyExternalUnitsBehindTheHerald(t *testing.T) {
	for _, order := range []string{ExternalOrderO1, ExternalOrderO2} {
		t.Run(order, func(t *testing.T) {
			s := DefaultSettings()
			s.RankOrder = mustRankOrder(t, order)
			d, _ := newTestDispatcher(t, s, partitionPolicy(4, 2))
			done := d.InteractiveArrived()
			il := mustAcquire(t, d, ictx(principal("i")), interactiveReq())
			defer il.Release()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch := make(chan admission, 2)
			startWaiter(ctx, d, "B", backgroundReq(), ch)
			waitFor(t, "B queued behind the herald", func() bool { return waitingBackground(d) == 1 })
			bg := context.Background()
			x1 := mustAcquire(t, d, xctx(bg, "x1", "tenant-a"), xbReq())
			defer x1.Release()
			x2 := mustAcquire(t, d, xctx(bg, "x2", "tenant-a"), xbReq())
			defer x2.Release()
			startWaiter(xctx(ctx, "x3", "tenant-a"), d, "XB", xbReq(), ch)
			waitFor(t, "third XB queued", func() bool { return waitingBulk(d) == 1 })
			ts := target(t, d)
			if ts.HeldExternalWeight != 2 || ts.HeldInternal != 1 || ts.HeldWeight != 3 {
				t.Fatalf("heldExt %d heldInt %d held %d, want 2/1/3 — XB beyond E or on an internal unit", ts.HeldExternalWeight, ts.HeldInternal, ts.HeldWeight)
			}
			done()
			a := recvAdmission(t, ch, "handoff after the demand ended")
			if a.label != "B" || a.err != nil {
				t.Fatalf("handoff %s (%v), want the background waiter on the free internal unit", a.label, a.err)
			}
			defer a.lease.Release()
			if got := waitingBulk(d); got != 1 {
				t.Fatalf("XB waiting %d, want 1 — the partition is full", got)
			}
		})
	}
}

// TestExternalBulkSeparateCaps is gate (e): the cap counters are per class.
// A consumer at its XB cap (external_queue_per_consumer = 1) still queues an
// XU acquire, and its second acquire of either class is refused at its own
// class's cap — counted in that class.
func TestExternalBulkSeparateCaps(t *testing.T) {
	s := DefaultSettings()
	s.ExternalQueuePerConsumer = 1
	d, _ := newTestDispatcher(t, s, partitionPolicy(3, 1))
	bg := context.Background()
	o1 := mustAcquire(t, d, xctx(bg, "occ", "tenant-o"), externalReq())
	defer o1.Release()
	o2 := mustAcquire(t, d, xctx(bg, "occ", "tenant-o"), externalReq())
	defer o2.Release()
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 2)
	startWaiter(xctx(ctx, "c1", "tenant-a"), d, "XB", xbReq(), ch)
	waitFor(t, "c1 XB queued", func() bool { return waitingBulk(d) == 1 })
	startWaiter(xctx(ctx, "c1", "tenant-a"), d, "XU", externalReq(), ch)
	waitFor(t, "c1 XU queued", func() bool { return waitingExternal(d) == 1 })
	for _, req := range []Request{xbReq(), externalReq()} {
		if l, _, err := acquireWithin(t, d, xctx(bg, "c1", "tenant-a"), req, time.Second); !errors.Is(err, ErrPrincipalSaturated) {
			t.Fatalf("second %s acquire of c1: (lease %v, %v), want ErrPrincipalSaturated", req.Class, l != nil, err)
		}
	}
	ts := target(t, d)
	if ts.ExternalRejects != (ExternalRejectStats{Cap: 1}) || ts.ExternalBulkRejects != (ExternalRejectStats{Cap: 1}) {
		t.Fatalf("rejects XU %+v XB %+v, want cap 1 each", ts.ExternalRejects, ts.ExternalBulkRejects)
	}
}

// TestExternalBulkRejectCountersPerClass is gate (h): ten XU and seven XB
// refusals on a target that does not offer the external classes, then three
// XU and two XB refusals by a wait budget already spent on a full partition
// — the counters stay apart per class and reason (A03-W5 review 2 F-5).
func TestExternalBulkRejectCountersPerClass(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(2))
	bg := context.Background()
	for i := 0; i < 10; i++ {
		if _, _, err := d.Acquire(xctx(bg, "c", "tenant-a"), externalReq()); !errors.Is(err, ErrExternalNotOffered) {
			t.Fatalf("XU on a target without offer: %v", err)
		}
	}
	for i := 0; i < 7; i++ {
		if _, _, err := d.Acquire(xctx(bg, "c", "tenant-a"), xbReq()); !errors.Is(err, ErrExternalNotOffered) {
			t.Fatalf("XB on a target without offer: %v", err)
		}
	}
	ts := target(t, d)
	if ts.ExternalRejects != (ExternalRejectStats{NotOffered: 10}) || ts.ExternalBulkRejects != (ExternalRejectStats{NotOffered: 7}) {
		t.Fatalf("rejects XU %+v XB %+v, want not_offered 10 / 7", ts.ExternalRejects, ts.ExternalBulkRejects)
	}
	d2, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1))
	occ := mustAcquire(t, d2, xctx(bg, "occ", "tenant-o"), externalReq())
	defer occ.Release()
	spent := func(r Request) Request { r.ServeBy = time.Now().Add(-time.Second); return r }
	for i, req := range []Request{spent(externalReq()), spent(externalReq()), spent(externalReq()), spent(xbReq()), spent(xbReq())} {
		if _, _, err := d2.Acquire(xctx(bg, fmt.Sprintf("p%d", i), "tenant-a"), req); !errors.Is(err, ErrWaitExpired) {
			t.Fatalf("acquire %d (%s) with a spent budget: %v, want ErrWaitExpired", i, req.Class, err)
		}
	}
	ts = target(t, d2)
	if ts.ExternalRejects != (ExternalRejectStats{Predicted: 3}) || ts.ExternalBulkRejects != (ExternalRejectStats{Predicted: 2}) {
		t.Fatalf("rejects XU %+v XB %+v, want predicted 3 / 2", ts.ExternalRejects, ts.ExternalBulkRejects)
	}
	short := xbReq()
	short.MaxWait = 20 * time.Millisecond
	if _, _, err := d2.Acquire(xctx(bg, "e", "tenant-a"), short); !errors.Is(err, ErrWaitExpired) {
		t.Fatalf("XB with a 20 ms budget on a full partition: %v, want ErrWaitExpired (expired)", err)
	}
	ts = target(t, d2)
	if ts.ExternalRejects != (ExternalRejectStats{Predicted: 3}) || ts.ExternalBulkRejects != (ExternalRejectStats{Predicted: 2, Expired: 1}) {
		t.Fatalf("rejects XU %+v XB %+v, want the expiry in the XB row", ts.ExternalRejects, ts.ExternalBulkRejects)
	}
}

// TestExternalBulkHoldBindsXB is gate (j): S = 7, R = 2 (E = 5). XU c0 holds
// one unit; the XU head H (w = E) does not fit and holds its class (nothing
// above XU keeps it out). An XB newcomer gets no unit, and neither does a
// waiting XB on the wake of an unrelated release; H is admitted at c0's
// release, the XB waiter only after H.
func TestExternalBulkHoldBindsXB(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	bg := context.Background()
	c0 := mustAcquire(t, d, xctx(bg, "c0", "tenant-a"), weightedReq(1))
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 2)
	startWaiter(xctx(ctx, "h", "tenant-b"), d, "H", weightedReq(weightE), ch)
	waitQueued(t, d, ch, 1, "H queued (w = E, holds)")
	if l, _, err := d.TryAcquire(xctx(bg, "x1", "tenant-c"), xbReq()); !errors.Is(err, ErrWouldBlock) {
		if l != nil {
			l.Release()
		}
		t.Fatalf("XB newcomer while an XU head holds the partition: (lease %v, %v), want ErrWouldBlock", l != nil, err)
	}
	startWaiter(xctx(ctx, "x2", "tenant-c"), d, "XB", xbReq(), ch)
	waitFor(t, "XB queued", func() bool { return waitingBulk(d) == 1 })
	mustAcquire(t, d, ictx(principal("i")), interactiveReq()).Release() // a wake without a freed external unit
	select {
	case a := <-ch:
		t.Fatalf("%s admitted (%v) while the XU head holds", a.label, a.err)
	case <-time.After(60 * time.Millisecond):
	}
	c0.Release()
	a := recvAdmission(t, ch, "handoff after c0's release")
	if a.label != "H" || a.err != nil {
		t.Fatalf("handoff %s (%v), want the holding XU head", a.label, a.err)
	}
	if got := waitingBulk(d); got != 1 {
		t.Fatalf("XB waiting %d after H's admission, want 1 (E full)", got)
	}
	a.lease.Release()
	b := recvAdmission(t, ch, "XB after H")
	if b.label != "XB" || b.err != nil {
		t.Fatalf("handoff %s (%v), want XB", b.label, b.err)
	}
	b.lease.Release()
}

// TestExternalBulkAdmissionAboveEndsTheHold pins the wake after an
// immediate admission for XB (the A03-W4 review 3/4 rule, extended over the
// rank order): S = 7, R = 2 (E = 5), two interactive leases, an XB lease of
// c0. The XB head H (w = E) does not fit E and holds its class (2 + 5 ≤ S),
// so the light XB head L waits although it fits. A third interactive
// newcomer is admitted at once — now interactive alone keeps H out (3 + 5 >
// S), the hold ends, and that admission's wake hands L its unit (≤ 50 ms),
// not the next release.
func TestExternalBulkAdmissionAboveEndsTheHold(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	bg := context.Background()
	for i := 0; i < 2; i++ {
		defer mustAcquire(t, d, ictx(principal(fmt.Sprintf("i%d", i))), interactiveReq()).Release()
	}
	defer mustAcquire(t, d, xctx(bg, "c0", "tenant-a"), xbReq()).Release()
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 2)
	startWaiter(xctx(ctx, "h", "tenant-b"), d, "H", xbWeighted(weightE), ch)
	waitFor(t, "H queued", func() bool { return waitingBulk(d) == 1 })
	startWaiter(xctx(ctx, "l", "tenant-c"), d, "L", xbReq(), ch)
	waitFor(t, "L queued behind the hold", func() bool { return waitingBulk(d) == 2 })
	select {
	case a := <-ch:
		t.Fatalf("%s admitted (%v) while H holds the class", a.label, a.err)
	case <-time.After(30 * time.Millisecond):
	}
	start := time.Now()
	il := mustAcquire(t, d, ictx(principal("i2")), interactiveReq())
	defer il.Release()
	select {
	case a := <-ch:
		if a.label != "L" || a.err != nil || time.Since(start) > 50*time.Millisecond {
			t.Fatalf("after the interactive admission: %s (%v) after %v, want L within 50ms", a.label, a.err, time.Since(start))
		}
		a.lease.Release()
	case <-time.After(150 * time.Millisecond):
		t.Fatal("L (fits, the hold ended with the interactive admission) still waits 150ms later — no wake after an admission ranked above XB")
	}
}

// TestExternalBulkHeadKeptOutByXUDoesNotHold is review 1 F-2, the other
// side of the claim: XU ranks above XB, so units XU holds keep an XB head
// out on their own. S = 7, R = 2 (E = 5), XU holds 3 units, the XB head H
// (w = 3) does not fit E beside them (3 + 3 > 5) — it does not hold its
// class (no idle units for a head XU keeps out), and light XB newcomers take
// the two free external units at once.
func TestExternalBulkHeadKeptOutByXUDoesNotHold(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	bg := context.Background()
	defer mustAcquire(t, d, xctx(bg, "u", "tenant-a"), weightedReq(3)).Release()
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(xctx(ctx, "h", "tenant-b"), d, "H", xbWeighted(3), ch)
	waitFor(t, "H queued", func() bool { return waitingBulk(d) == 1 })
	for i := 0; i < 2; i++ {
		l, _, err := d.TryAcquire(xctx(bg, fmt.Sprintf("x%d", i), "tenant-c"), xbReq())
		if err != nil {
			t.Fatalf("light XB newcomer %d beside an XB head kept out by XU units: %v, want an admission", i, err)
		}
		defer l.Release()
	}
	if ts := target(t, d); ts.HeldExternalWeight != 5 {
		t.Fatalf("heldExt %d, want 5 (E used)", ts.HeldExternalWeight)
	}
}

// TestExternalBulkViewsPerClass is review 1 F-3: every per-class view of XB
// is its own — the wait window and waiting count of the snapshot, the
// admin buckets (class external_bulk, waiting and inflight), the
// overweight drain's not_offered counter, and the class formula of
// RetryAfterHint (the XB queue depth times the XB window's p95).
func TestExternalBulkViewsPerClass(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	bg := context.Background()
	defer mustAcquire(t, d, xctx(bg, "b1", "tenant-a"), xbWeighted(4)).Release()
	b2 := mustAcquire(t, d, xctx(bg, "b2", "tenant-a"), xbReq())
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 4)
	startWaiter(xctx(ctx, "heavy", "tenant-b"), d, "heavy", xbWeighted(4), ch)
	waitFor(t, "heavy XB queued", func() bool { return waitingBulk(d) == 1 })
	for i := 0; i < 2; i++ {
		startWaiter(xctx(ctx, fmt.Sprintf("w%d", i), "tenant-b"), d, "light", xbReq(), ch)
	}
	waitFor(t, "light XB queued", func() bool { return waitingBulk(d) == 3 })
	ts := target(t, d)
	if ts.ExternalBulk.Waiting != 3 || ts.ExternalBulk.Samples != 2 || ts.External.Samples != 0 || ts.External.Waiting != 0 {
		t.Fatalf("wait views XB %+v XU %+v, want XB waiting 3 with 2 samples, XU empty", ts.ExternalBulk, ts.External)
	}
	classes := map[string]Class{}
	for _, b := range ts.Buckets {
		classes[b.FairKey] = b.Class
		if b.FairKey == "_consumer:b1" && b.Inflight != 1 || b.FairKey == "_consumer:heavy" && b.Waiting != 1 {
			t.Fatalf("bucket %+v, want b1 inflight 1 / heavy waiting 1", b)
		}
	}
	for _, key := range []string{"_consumer:b1", "_consumer:b2", "_consumer:heavy", "_consumer:w0"} {
		if c, ok := classes[key]; !ok || c != ClassExternalBulk {
			t.Fatalf("bucket %s class %v (present %v), want external_bulk", key, c, ok)
		}
	}
	d.SeedWaitSampleForTest(testOrigin, ClassExternalBulk, 4*time.Second)
	d.SeedWaitSampleForTest(testOrigin, ClassExternal, time.Millisecond)
	if got := d.RetryAfterHint(testOrigin, ClassExternalBulk); got != 20*time.Second {
		t.Fatalf("RetryAfterHint(XB) = %v, want 20s (3 waiting × the XB p95 4 s, quantized)", got)
	}
	d.UpdatePolicy(partitionPolicy(weightSlots, weightReserved+2)) // E 5 → 3: the w = 4 waiter can never fit
	a := recvAdmission(t, ch, "heavy XB after E shrank")
	if a.label != "heavy" || !errors.Is(a.err, ErrExternalNotOffered) {
		t.Fatalf("%s: %v, want the heavy XB ended with ErrExternalNotOffered", a.label, a.err)
	}
	if ts := target(t, d); ts.ExternalBulkRejects != (ExternalRejectStats{NotOffered: 1}) || ts.ExternalRejects != (ExternalRejectStats{}) {
		t.Fatalf("rejects XB %+v XU %+v, want not_offered 1 in the XB row", ts.ExternalBulkRejects, ts.ExternalRejects)
	}
	b2.Release()
}

// TestExternalBulkSelfAdmittingClaimAndWake is review 1 F-8: on an offered
// self-admitting target (no slots, external_max_inflight 3) the wake's
// external loop serves the classes in rank order — a waiting XU head
// (w = 2) beside an XU lease (w = 2) claims its units against XB (an XB
// newcomer is refused), and when the XU lease ends the same wake admits the
// XU head and then the waiting XB acquire.
func TestExternalBulkSelfAdmittingClaimAndWake(t *testing.T) {
	pol := Policy{Targets: map[string]TargetPolicy{testOrigin: {ExternalOffer: true, ExternalMaxInflight: 3}}}
	d, _ := newTestDispatcher(t, DefaultSettings(), pol)
	bg := context.Background()
	u := mustAcquire(t, d, xctx(bg, "u", "tenant-a"), weightedReq(2))
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 2)
	startWaiter(xctx(ctx, "h", "tenant-b"), d, "H", weightedReq(2), ch)
	waitQueued(t, d, ch, 1, "XU head queued")
	if l, _, err := d.TryAcquire(xctx(bg, "x", "tenant-c"), xbReq()); !errors.Is(err, ErrWouldBlock) {
		if l != nil {
			l.Release()
		}
		t.Fatalf("XB newcomer beside a waiting XU head on a self-admitting target: (lease %v, %v), want ErrWouldBlock", l != nil, err)
	}
	startWaiter(xctx(ctx, "y", "tenant-c"), d, "XB", xbReq(), ch)
	waitFor(t, "XB queued", func() bool { return waitingBulk(d) == 1 })
	u.Release()
	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		a := recvAdmission(t, ch, "admissions after the XU release")
		if a.err != nil {
			t.Fatalf("%s: %v", a.label, a.err)
		}
		got[a.label] = true
		defer a.lease.Release()
	}
	if !got["H"] || !got["XB"] {
		t.Fatalf("admitted %v, want the XU head and the XB waiter", got)
	}
}

// TestExternalBulkContinuationOrder pins the continuation term per class
// (review 1 F-3, R23): an XB continuation waits behind the waiting XB
// continuation of its own class — a lighter one that would fit is not
// admitted past it — while an XU continuation of the same weight, whose
// class has none waiting, is.
func TestExternalBulkContinuationOrder(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	bg := context.Background()
	defer mustAcquire(t, d, xctx(bg, "o", "tenant-a"), xbWeighted(4)).Release()
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 1)
	big := xbWeighted(2)
	big.Continuation = true
	startWaiter(xctx(ctx, "a", "tenant-b"), d, "A", big, ch)
	waitFor(t, "XB continuation A queued", func() bool { return waitingBulk(d) == 1 })
	small := xbReq()
	small.Continuation = true
	if l, _, err := d.TryAcquire(xctx(bg, "b", "tenant-b"), small); !errors.Is(err, ErrWouldBlock) {
		if l != nil {
			l.Release()
		}
		t.Fatalf("XB continuation past the waiting XB continuation: (lease %v, %v), want ErrWouldBlock", l != nil, err)
	}
	l, _, err := d.TryAcquire(xctx(bg, "c", "tenant-c"), contReq(1))
	if err != nil {
		t.Fatalf("XU continuation (its class has none waiting): %v, want an admission", err)
	}
	l.Release()
}

// TestExternalBulkReservationBindsXB is gate (k): o1, S = 4, R = 2 (E = 2),
// interactive holds one unit, background two. The XU head H (w = 2) fits E,
// is not kept out by interactive alone (1 + 2 ≤ 4) and does not fit the one
// free unit: d2 reserves it against background — and the claim against XB:
// neither a background nor an XB newcomer takes it; H is admitted at the
// next background release. Control in the other direction (o2): background
// ranks above XU, so interactive and background keep H out on their own —
// no claim, the XB newcomer takes the free unit (work conservation).
func TestExternalBulkReservationBindsXB(t *testing.T) {
	setup := func(t *testing.T, order string) (*Dispatcher, chan admission, []*Lease) {
		s := DefaultSettings()
		s.RankOrder = mustRankOrder(t, order)
		d, _ := newTestDispatcher(t, s, partitionPolicy(4, 2))
		il := mustAcquire(t, d, ictx(principal("i")), interactiveReq())
		b1 := mustAcquire(t, d, context.Background(), backgroundReq())
		b2 := mustAcquire(t, d, context.Background(), backgroundReq())
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		ch := make(chan admission, 2)
		startWaiter(xctx(ctx, "h", "tenant-a"), d, "H", weightedReq(2), ch)
		waitQueued(t, d, ch, 1, "H queued")
		return d, ch, []*Lease{il, b1, b2}
	}
	t.Run("o1", func(t *testing.T) {
		d, ch, held := setup(t, ExternalOrderO1)
		defer held[0].Release()
		defer held[2].Release()
		if l, _, err := d.TryAcquire(context.Background(), backgroundReq()); !errors.Is(err, ErrWouldBlock) {
			if l != nil {
				l.Release()
			}
			t.Fatalf("background newcomer on the reserved unit: %v, want ErrWouldBlock (d2)", err)
		}
		if l, _, err := d.TryAcquire(xctx(context.Background(), "x", "tenant-b"), xbReq()); !errors.Is(err, ErrWouldBlock) {
			if l != nil {
				l.Release()
			}
			t.Fatalf("XB newcomer on the unit reserved for the XU head: (lease %v, %v), want ErrWouldBlock", l != nil, err)
		}
		held[1].Release()
		a := recvAdmission(t, ch, "H after a background release")
		if a.label != "H" || a.err != nil {
			t.Fatalf("handoff %s (%v), want H", a.label, a.err)
		}
		a.lease.Release()
	})
	t.Run("o2 control", func(t *testing.T) {
		d, _, held := setup(t, ExternalOrderO2)
		for _, l := range held {
			defer l.Release()
		}
		l, _, err := d.TryAcquire(xctx(context.Background(), "x", "tenant-b"), xbReq())
		if err != nil {
			t.Fatalf("o2: XB newcomer refused while interactive and background keep the XU head out: %v", err)
		}
		l.Release()
	})
}

// TestExternalBulkNeverVictimReaperOnlyAndDrained is gate (g), I-03-3 for
// XB: a target full with XB and an interactive waiter on a preempt target ⇒
// no victim, the interactive waiter gets the unit XB releases; a leaked XB
// lease is canceled with ErrReaped past admitted + external_lease_max +
// grace; the emergency stop ends waiting XB with ErrExternalNotOffered,
// counted in the XB row only.
func TestExternalBulkNeverVictimReaperOnlyAndDrained(t *testing.T) {
	pol := Policy{Targets: map[string]TargetPolicy{testOrigin: {Slots: 2, InternalReserved: 1, PreemptBackground: true}}}
	d, h := newTestDispatcher(t, DefaultSettings(), pol)
	xl, xrun, err := acquireWithin(t, d, xctx(context.Background(), "b1", "tenant-a"), xbReq(), 3*time.Second)
	if err != nil {
		t.Fatalf("XB occupier: %v", err)
	}
	il := mustAcquire(t, d, ictx(principal("i1")), interactiveReq())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(withPrincipal(ctx, principal("i2")), d, "interactive", interactiveReq(), ch)
	waitFor(t, "interactive waiter queued", func() bool { return waitingInteractive(d) == 1 })
	if xrun.Err() != nil || target(t, d).Preempt.PreemptsTotal != 0 {
		t.Fatalf("XB lease canceled by a scheduling decision: runCtx=%v preempts=%d", context.Cause(xrun), target(t, d).Preempt.PreemptsTotal)
	}
	if !h.contains("without cancelable background lease") {
		t.Fatal("expected the no-victim WARN on a target full with interactive and XB leases")
	}
	xl.Release()
	recvAdmission(t, ch, "interactive after the XB release").lease.Release()
	il.Release()

	s := DefaultSettings()
	s.ExternalLeaseMax = time.Minute
	s.LeaseMaxAge = time.Hour
	s.LeaseReapGrace = time.Second
	d2, _ := newTestDispatcher(t, s, partitionPolicy(3, 1))
	l2, run2, err := acquireWithin(t, d2, xctx(context.Background(), "b1", "tenant-a"), xbReq(), 3*time.Second)
	if err != nil {
		t.Fatalf("XB: %v", err)
	}
	d2.ReapForTest(l2.admitted.Add(30 * time.Second))
	if run2.Err() != nil {
		t.Fatalf("XB lease reaped before external_lease_max: %v", context.Cause(run2))
	}
	d2.ReapForTest(l2.admitted.Add(time.Minute + 2*time.Second))
	if !errors.Is(context.Cause(run2), ErrReaped) {
		t.Fatalf("leaked XB lease past external_lease_max: cause=%v, want ErrReaped", context.Cause(run2))
	}
	if ts := target(t, d2); ts.HeldExternal != 0 || ts.Held != 0 {
		t.Fatalf("after the reap heldExt=%d held=%d, want 0/0", ts.HeldExternal, ts.Held)
	}

	d3, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1))
	occ := mustAcquire(t, d3, xctx(context.Background(), "occ", "tenant-a"), externalReq())
	defer occ.Release()
	xch := make(chan admission, 1)
	startWaiter(xctx(ctx, "b2", "tenant-a"), d3, "XB", xbReq(), xch)
	waitFor(t, "XB queued", func() bool { return waitingBulk(d3) == 1 })
	off := DefaultSettings()
	off.Enabled = false
	d3.UpdateSettings(off)
	a := recvAdmission(t, xch, "XB after the emergency stop")
	if !errors.Is(a.err, ErrExternalNotOffered) || a.lease != nil {
		t.Fatalf("XB waiter after the emergency stop: (lease %v, %v), want ErrExternalNotOffered — never a pass-through handoff", a.lease != nil, a.err)
	}
	if ts := target(t, d3); ts.ExternalBulkRejects != (ExternalRejectStats{NotOffered: 1}) || ts.ExternalRejects != (ExternalRejectStats{}) || ts.ExternalBulk.Waiting != 0 {
		t.Fatalf("rejects XB %+v XU %+v XB waiting %d, want not_offered 1 / none / 0", ts.ExternalBulkRejects, ts.ExternalRejects, ts.ExternalBulk.Waiting)
	}
}

// TestExternalBulkEscapeBeforeXB is gate (f) for XB: background_aging_after
// > 0, demand > 0, the background head aged past it with a unit free (no
// wake since): an XB newcomer does not take that unit — the escape-capable
// head counts as admissible against every class ranked below background —
// and its enqueue wake admits the aged head; the newcomer follows once a
// unit frees. Both orders (XB ranks below background in both).
func TestExternalBulkEscapeBeforeXB(t *testing.T) {
	for _, order := range []string{ExternalOrderO1, ExternalOrderO2} {
		t.Run(order, func(t *testing.T) {
			escapeVsNewcomer(t, order, xbReq(), "XB", true)
		})
	}
}

// TestExternalBulkEscapeYieldsOnRefusal is review 1 F-1 for XB: an XB
// newcomer refused without an enqueue (TryAcquire, a spent wait budget)
// beside an escape-capable aged background head leaves no unit idle — the
// head is admitted at once — in both orders.
func TestExternalBulkEscapeYieldsOnRefusal(t *testing.T) {
	spent := xbReq()
	spent.ServeBy = time.Now().Add(-time.Second)
	for _, order := range []string{ExternalOrderO1, ExternalOrderO2} {
		t.Run(order+" TryAcquire", func(t *testing.T) { escapeYieldOnRefusal(t, order, xbReq(), true) })
		t.Run(order+" spent budget", func(t *testing.T) { escapeYieldOnRefusal(t, order, spent, false) })
	}
}

// TestExternalBulkPredictionPerClass is gate (l): XB lease samples land in
// the XB window only; the XU prediction reads the XU window — an XU acquire
// with a short budget is queued on its 1 s window while an XB acquire with
// the same budget is refused at once (predicted) on its 60 s window,
// counted in the XB row; and the Retry-After of the XB refusal is that of
// its position behind the waiting XU weight (P1).
func TestExternalBulkPredictionPerClass(t *testing.T) {
	t.Run("own ring", func(t *testing.T) {
		d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(4, 2))
		bg := context.Background()
		for i := 0; i < 3; i++ {
			mustAcquire(t, d, xctx(bg, "b", "tenant-a"), xbReq()).Release()
		}
		for i := 0; i < 2; i++ {
			mustAcquire(t, d, xctx(bg, "u", "tenant-a"), externalReq()).Release()
		}
		ts := target(t, d)
		if ts.LeaseDur[ClassExternalBulk].Samples != 3 || ts.LeaseDur[ClassExternal].Samples != 2 {
			t.Fatalf("lease samples XB %d XU %d, want 3/2 — one window per class", ts.LeaseDur[ClassExternalBulk].Samples, ts.LeaseDur[ClassExternal].Samples)
		}
	})
	t.Run("own window", func(t *testing.T) {
		s := DefaultSettings()
		s.ExternalRetryAfterSteps = []time.Duration{5 * time.Second, time.Minute, 2 * time.Minute}
		d, _ := newTestDispatcher(t, s, partitionPolicy(4, 2))
		d.SeedLeaseSamplesForTest(testOrigin, ClassExternal, 2*leaseColdSamples, time.Second)
		d.SeedLeaseSamplesForTest(testOrigin, ClassExternalBulk, 2*leaseColdSamples, time.Minute)
		bg := context.Background()
		for i := 0; i < 2; i++ {
			defer mustAcquire(t, d, xctx(bg, fmt.Sprintf("o%d", i), "tenant-o"), externalReq()).Release()
		}
		ctx, cancel := context.WithCancel(bg)
		defer cancel()
		ch := make(chan admission, 1)
		// The XU waiter weighs E: the XB newcomer's position is ⌈(2 + 1) /
		// 2⌉ = 2 rounds — r̂ of the XU holders at their own 1 s window (H-1,
		// review 2 F2-1) plus one round at the 60 s round length of the XB
		// window. Read with the XU window (mutant X08) ŵ ≈ 2 s would queue.
		xu := externalReq()
		xu.Weight = 2
		xu.MaxWait = 5 * time.Second
		startWaiter(xctx(ctx, "u", "tenant-a"), d, "XU", xu, ch)
		waitQueued(t, d, ch, 1, "XU queued on its 1 s window")
		xb := xbReq()
		xb.MaxWait = 5 * time.Second
		_, _, err := d.Acquire(xctx(bg, "b", "tenant-a"), xb)
		var re *RejectionError
		if !errors.Is(err, ErrWaitExpired) || !errors.As(err, &re) {
			t.Fatalf("XB on its 60 s window with a 5 s budget: %v, want a predicted ErrWaitExpired", err)
		}
		ts := target(t, d)
		if ts.ExternalBulkRejects != (ExternalRejectStats{Predicted: 1}) || ts.ExternalRejects != (ExternalRejectStats{}) {
			t.Fatalf("rejects XB %+v XU %+v, want predicted 1 / none", ts.ExternalBulkRejects, ts.ExternalRejects)
		}
		if re.RetryAfter != 2*time.Minute {
			t.Fatalf("XB Retry-After %v, want 2m0s (Q of r̂ ≈ 1 s + one 60 s round of the XB window)", re.RetryAfter)
		}
	})
	t.Run("own class tenants", func(t *testing.T) {
		// Review 1 F-3 (R17): the XB position counts the tenants waiting in
		// XB, not in XU. Three XU tenants wait one unit each (ahead 3); the
		// XB newcomer's tenant is the only one in XB: ⌈(3 + 1) / 2⌉ = 2
		// rounds ⇒ ŵ = r̂ + 10 s ≈ 20 s (with XU's tenant count, 4 rounds).
		s := DefaultSettings()
		s.ExternalMaxWait = time.Minute
		s.ExternalRetryAfterSteps = []time.Duration{5 * time.Second, 10 * time.Second, 15 * time.Second, 20 * time.Second, 30 * time.Second, 40 * time.Second, 60 * time.Second}
		d, _ := newTestDispatcher(t, s, partitionPolicy(4, 2))
		d.SeedLeaseSamplesForTest(testOrigin, ClassExternal, 2*leaseColdSamples, 10*time.Second)
		d.SeedLeaseSamplesForTest(testOrigin, ClassExternalBulk, 2*leaseColdSamples, 10*time.Second)
		bg := context.Background()
		for i := 0; i < 2; i++ {
			defer mustAcquire(t, d, xctx(bg, fmt.Sprintf("o%d", i), "tenant-o"), externalReq()).Release()
		}
		ctx, cancel := context.WithCancel(bg)
		defer cancel()
		ch := make(chan admission, 3)
		for i := 0; i < 3; i++ {
			startWaiter(xctx(ctx, fmt.Sprintf("u%d", i), fmt.Sprintf("tenant-u%d", i)), d, "XU", externalReq(), ch)
			waitQueued(t, d, ch, i+1, "XU queued")
		}
		xb := xbReq()
		xb.MaxWait = 5 * time.Second
		_, _, err := d.Acquire(xctx(bg, "x", "tenant-x"), xb)
		var re *RejectionError
		if !errors.As(err, &re) || !errors.Is(err, ErrWaitExpired) {
			t.Fatalf("XB behind three XU tenants: %v, want a predicted ErrWaitExpired", err)
		}
		if re.RetryAfter != 20*time.Second {
			t.Fatalf("XB Retry-After %v, want 20s — the XB position counts the XB tenants (1), not XU's (3)", re.RetryAfter)
		}
		// The newcomer position (a would-block, never queued) counts the same.
		_, _, err = d.TryAcquire(xctx(bg, "y", "tenant-y"), xbReq())
		if !errors.As(err, &re) || !errors.Is(err, ErrWouldBlock) || re.RetryAfter != 20*time.Second {
			t.Fatalf("XB would-block behind three XU tenants: %v (Retry-After %v), want ErrWouldBlock with 20s", err, re.RetryAfter)
		}
	})
	t.Run("first head behind the XU weight", func(t *testing.T) {
		// Review 1 F-3 (R21): the fairness-first XB head waits its first
		// round only behind the waiting XU weight — ⌈(ahead + w) / E⌉
		// rounds, not one. L_p50 = 100 ms, the occupants older than that
		// (r̂ = 0), 4 XU units waiting on E = 2: the lone XB head needs 3
		// rounds = 200 ms > its 150 ms budget ⇒ predicted at once; with one
		// round (r̂ = 0 ≤ 150 ms) it would queue and expire.
		d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(4, 2))
		d.SeedLeaseSamplesForTest(testOrigin, ClassExternal, 2*leaseColdSamples, 100*time.Millisecond)
		d.SeedLeaseSamplesForTest(testOrigin, ClassExternalBulk, 2*leaseColdSamples, 100*time.Millisecond)
		bg := context.Background()
		for i := 0; i < 2; i++ {
			defer mustAcquire(t, d, xctx(bg, fmt.Sprintf("o%d", i), "tenant-o"), externalReq()).Release()
		}
		time.Sleep(120 * time.Millisecond) // the occupants run past L_p50: r̂ = 0
		ctx, cancel := context.WithCancel(bg)
		defer cancel()
		ch := make(chan admission, 2)
		startWaiter(xctx(ctx, "a", "tenant-a"), d, "XUa", weightedReq(2), ch)
		waitQueued(t, d, ch, 1, "XU a queued")
		startWaiter(xctx(ctx, "b", "tenant-a"), d, "XUb", weightedReq(2), ch)
		waitQueued(t, d, ch, 2, "XU b queued")
		xb := xbReq()
		xb.MaxWait = 150 * time.Millisecond
		start := time.Now()
		_, _, err := d.Acquire(xctx(bg, "x", "tenant-x"), xb)
		if !errors.Is(err, ErrWaitExpired) || time.Since(start) > 50*time.Millisecond {
			t.Fatalf("lone XB head behind 4 XU units: %v after %v, want a predicted ErrWaitExpired at once", err, time.Since(start))
		}
		if rej := target(t, d).ExternalBulkRejects; rej != (ExternalRejectStats{Predicted: 1}) {
			t.Fatalf("XB rejects %+v, want predicted 1", rej)
		}
	})
	t.Run("behind the XU weight", func(t *testing.T) {
		s := DefaultSettings()
		s.ExternalMaxWait = time.Minute
		s.ExternalRetryAfterSteps = []time.Duration{5 * time.Second, 10 * time.Second, 15 * time.Second, 20 * time.Second, 25 * time.Second, 30 * time.Second, 60 * time.Second}
		d, _ := newTestDispatcher(t, s, partitionPolicy(4, 2))
		d.SeedLeaseSamplesForTest(testOrigin, ClassExternal, 2*leaseColdSamples, 10*time.Second)
		d.SeedLeaseSamplesForTest(testOrigin, ClassExternalBulk, 2*leaseColdSamples, 10*time.Second)
		bg := context.Background()
		for i := 0; i < 2; i++ {
			defer mustAcquire(t, d, xctx(bg, fmt.Sprintf("o%d", i), "tenant-o"), externalReq()).Release()
		}
		ctx, cancel := context.WithCancel(bg)
		defer cancel()
		ch := make(chan admission, 2)
		startWaiter(xctx(ctx, "a", "tenant-a"), d, "XUa", weightedReq(2), ch)
		waitQueued(t, d, ch, 1, "XU a queued")
		startWaiter(xctx(ctx, "b", "tenant-a"), d, "XUb", weightedReq(2), ch)
		waitQueued(t, d, ch, 2, "XU b queued")
		xb := xbReq()
		xb.MaxWait = 5 * time.Second
		_, _, err := d.Acquire(xctx(bg, "x", "tenant-x"), xb)
		var re *RejectionError
		if !errors.Is(err, ErrWaitExpired) || !errors.As(err, &re) {
			t.Fatalf("XB behind 4 waiting XU units: %v, want a predicted ErrWaitExpired", err)
		}
		// ahead 4, own 1, E 2: ⌈(4 + 1) / 2⌉ = 3 rounds ⇒ ŵ = r̂ + 2 · 10 s ≈ 30 s
		// (without the XU weight ahead it would be one round, r̂ ≈ 10 s).
		if re.RetryAfter != 30*time.Second {
			t.Fatalf("XB Retry-After %v, want 30s — its position behind the waiting XU weight (P1)", re.RetryAfter)
		}
	})
}

// TestExternalBulkMixedFloodBookkeeping is the race/goroutine gate: a mixed
// flood of interactive, background, XU and XB acquires (weights 1–2, the
// same consumers in both external classes, random caller cancels and hold
// times) on S = 6, R = 2 under o1 and o2. Throughout, held ≤ S and heldExt ≤
// E; at the end every lease is released, the bookkeeping audit (both queues,
// the shared records, classHeld, heldExt as recomputation) is clean, held,
// heldExt and every queue are 0, and the goroutine count is back (± the
// dispatcher's own loops, which the test does not close mid-run).
func TestExternalBulkMixedFloodBookkeeping(t *testing.T) {
	for _, order := range []string{ExternalOrderO1, ExternalOrderO2} {
		t.Run(order, func(t *testing.T) {
			before := runtime.NumGoroutine()
			s := DefaultSettings()
			s.RankOrder = mustRankOrder(t, order)
			d, _ := newTestDispatcher(t, s, partitionPolicy(6, 2))
			var wg sync.WaitGroup
			var mu sync.Mutex
			violation := ""
			check := func() {
				ts := target(t, d)
				if ts.HeldWeight > 6 || ts.HeldExternalWeight > 4 {
					mu.Lock()
					violation = fmt.Sprintf("held %d heldExt %d past S 6 / E 4", ts.HeldWeight, ts.HeldExternalWeight)
					mu.Unlock()
				}
			}
			for g := 0; g < 48; g++ {
				wg.Add(1)
				go func(g int) {
					defer wg.Done()
					rng := rand.New(rand.NewSource(int64(g)))
					for i := 0; i < 20; i++ {
						ctx, cancel := context.WithTimeout(context.Background(), time.Duration(1+rng.Intn(15))*time.Millisecond)
						var req Request
						switch g % 4 {
						case 0:
							ctx, req = withPrincipal(ctx, principal(fmt.Sprintf("p%d", g%3))), interactiveReq()
						case 1:
							req = backgroundReq()
						case 2:
							ctx, req = xctx(ctx, fmt.Sprintf("c%d", g%5), fmt.Sprintf("t%d", g%3)), weightedReq(1+rng.Intn(2))
						default:
							ctx, req = xctx(ctx, fmt.Sprintf("c%d", g%5), fmt.Sprintf("t%d", g%3)), xbWeighted(1+rng.Intn(2))
						}
						if l, _, err := d.Acquire(ctx, req); err == nil {
							time.Sleep(time.Duration(rng.Intn(3)) * time.Millisecond)
							check()
							l.Release()
						}
						cancel()
					}
				}(g)
			}
			wg.Wait()
			if violation != "" {
				t.Fatal(violation)
			}
			waitFor(t, "flood settled", func() bool {
				ts := target(t, d)
				return ts.Held == 0 && ts.Inflight == 0 && ts.External.Waiting == 0 && ts.ExternalBulk.Waiting == 0 &&
					ts.Interactive.Waiting == 0 && ts.Background.Waiting == 0
			})
			if diff, _ := d.fairAuditForTest(testOrigin); diff != "" {
				t.Fatalf("bookkeeping after the flood: %s", diff)
			}
			d.mu.Lock()
			st := d.targetLocked(testOrigin)
			held, heldExt, ext, bulk := st.held, st.heldExt, st.external.weight, st.externalBulk.weight
			d.mu.Unlock()
			if held != 0 || heldExt != 0 || ext != 0 || bulk != 0 {
				t.Fatalf("end state held %d heldExt %d waiting weight XU %d XB %d, want all 0", held, heldExt, ext, bulk)
			}
			d.Close()
			waitFor(t, "goroutines back", func() bool { return runtime.NumGoroutine() <= before })
		})
	}
}

// TestExternalBulkPickCostScales is the complexity gate of the XB pick
// (A03-W9, the A03-W4 gate (i) form, target scale: dozens of consumers per
// instance, N tenants): n XU and n XB buckets, every head of weight 1; the
// XU head fits, so its claim is one unit and every XB head fits beside it —
// each XB pick resolves the claim (one pass over the XU buckets) and orders
// every XB bucket. One pass per queue: the median grows linearly from 64 to
// 512 buckets per class (factor 8), a pick quadratic in either grows by 64;
// the bound is the gate's 22. The absolute bound (the small median far below
// opWarnThreshold) is load-sensitive: a shared CI runner put it at 1.06 ms
// once (nightly 2026-11-06, run 42154383471) with the growth at 7.5 — so both
// medians are measured up to xbPickCostPasses times and the gate fails only
// when every pass exceeds a bound (FOLGE-P hardening 4's rule: the bounds
// stay, one pass that holds every bound is the gate's answer).
func TestExternalBulkPickCostScales(t *testing.T) {
	med := func(n int) time.Duration {
		s := DefaultSettings()
		s.ExternalQueuePerConsumer, s.ExternalQueuePerTenant, s.ExternalQueueMax = 0, 0, 0
		d, _ := newTestDispatcher(t, s, partitionPolicy(3, 1))
		d.mu.Lock()
		defer d.mu.Unlock()
		st := d.targetLocked(testOrigin)
		for _, c := range []Class{ClassExternal, ClassExternalBulk} {
			for i := 0; i < n; i++ {
				w := &waiter{class: c, principal: xuPrincipal(fmt.Sprintf("cost-%s-%d", c, i), fmt.Sprintf("tenant-%d", i%8)),
					weight: 1, admitCh: make(chan struct{}), cancel: func(error) {}}
				if err := d.enqueueLocked(st, w); err != nil {
					t.Fatalf("enqueue %s %d: %v", c, i, err)
				}
			}
		}
		samples := make([]time.Duration, 2000)
		for i := range samples {
			start := time.Now()
			if w, _ := d.externalPickLocked(st, ClassExternalBulk, 3); w == nil {
				t.Fatalf("no XB head picked although every head fits beside the XU claim")
			}
			samples[i] = time.Since(start)
		}
		for _, q := range st.extQueues() {
			for q.total > 0 {
				q.pick(FairnessFIFO)
			}
		}
		slices.Sort(samples)
		return samples[len(samples)/2]
	}
	var over []string
	for pass := 1; pass <= xbPickCostPasses; pass++ {
		small, large := med(64), med(512)
		ratio := float64(large) / float64(small)
		t.Logf("pass %d: median XB pick: %v at 64+64 buckets, %v at 512+512 buckets — growth %.1f (linear 8, quadratic 64)", pass, small, large, ratio)
		var bad []string
		if ratio >= 22 {
			bad = append(bad, fmt.Sprintf("XB pick cost grows by %.1f from 64 to 512 buckets per class, want < 22 (one pass per queue)", ratio))
		}
		if small >= opWarnThreshold {
			bad = append(bad, fmt.Sprintf("median XB pick %v at 64+64 buckets, want far below %v", small, opWarnThreshold))
		}
		if len(bad) == 0 {
			return
		}
		over = append(over, fmt.Sprintf("pass %d: %s", pass, strings.Join(bad, "; ")))
		if pass < xbPickCostPasses {
			t.Logf("XB pick cost retry pass=%d of %d: %s — measuring again", pass, xbPickCostPasses, strings.Join(bad, "; "))
		}
	}
	t.Fatalf("each of %d passes exceeds a bound — %s", xbPickCostPasses, strings.Join(over, " | "))
}

// xbPickCostPasses is how often TestExternalBulkPickCostScales measures
// before it fails: a single pass under runner load is a sample, not a
// verdict; three passes that all exceed a bound are.
const xbPickCostPasses = 3
