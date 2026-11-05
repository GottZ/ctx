package dispatch

// Review-4 gates of A03-W4 (FR4-1, FR4-2), through the stock API plus the
// fake clock seam: an immediate external admission and an external enqueue
// can change the fairness-first head — the new pick goes to its units at
// once (FR4-1); the fairness order is frozen between events, so the mere
// passing of time never makes a waiting head the pick without a wake, and
// the next event re-reads it (FR4-2).

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// fairXctx is an external principal with an explicit fair weight.
func fairXctx(ctx context.Context, consumer, tenant string, fw int) context.Context {
	return withPrincipal(ctx, Principal{ConsumerID: consumer, TenantID: tenant, ConsumerKind: "app", FairWeight: fw})
}

// TestExternalAdmissionWakesTheNewFairnessFirstHead is FR4-1 trigger 1
// (review 4 P-S, the trace failure "fits and still waits after the wake"):
// S = 7, R = 2 (E = 5), fake clock. f (t0) holds a unit; h's w = E head
// (t1, nothing held, less tenant slot time) is the fairness-first one, does
// not fit and holds the class; f's next request (w = 1) waits behind it.
// A newcomer n of t1 ranks before h (same tenant, no consumer slot time)
// and is admitted at once — now t1 holds a unit, h is over its tenant share
// (1 + 5 > 3), and f's head is the fairness-first one and fits. That same
// admission's wake admits it (≤ 50 ms); before review 4 it waited until
// h fitted or expired.
func TestExternalAdmissionWakesTheNewFairnessFirstHead(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	clk := fakeFairClock(d)
	bg := context.Background()
	lf := mustAcquire(t, d, fairXctx(bg, "f", "t0", 1), weightedReq(1))
	clk.Add(int64(10 * time.Second))
	lf.Release()
	lh := mustAcquire(t, d, fairXctx(bg, "h", "t1", 1), weightedReq(1))
	clk.Add(int64(time.Second))
	lh.Release()
	lf2 := mustAcquire(t, d, fairXctx(bg, "f", "t0", 1), weightedReq(1))
	defer lf2.Release()
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 2)
	rh := weightedReq(weightE)
	rh.MaxWait = 5 * time.Second
	startWaiter(fairXctx(ctx, "h", "t1", 1), d, "H", rh, ch)
	waitQueued(t, d, ch, 1, "H queued (does not fit, holds)")
	rf := weightedReq(1)
	rf.MaxWait = 5 * time.Second
	startWaiter(fairXctx(ctx, "f", "t0", 1), d, "F", rf, ch)
	waitQueued(t, d, ch, 2, "F queued behind the hold")
	start := time.Now()
	ln, _, err := d.TryAcquire(fairXctx(bg, "n", "t1", 1), weightedReq(1))
	if ln == nil {
		t.Fatalf("newcomer n (ranks before the holding head) refused: %v", err)
	}
	defer ln.Release()
	select {
	case got := <-ch:
		lag := time.Since(start)
		t.Logf("%s admitted %v after n's immediate admission (bound 50ms), err=%v", got.label, lag, got.err)
		if got.err != nil || got.label != "F" || lag > 50*time.Millisecond {
			t.Fatalf("after n's admission: %s err=%v after %v, want F admitted ≤ 50ms", got.label, got.err, lag)
		}
		got.lease.Release()
	case <-time.After(150 * time.Millisecond):
		t.Fatalf("F (fits, fairness-first after n's immediate admission) still waits 150ms later — no wake after an external admission")
	}
}

// TestExternalEnqueueWakesTheNewFairnessFirstHead is FR4-1 trigger 2
// (review 4 P-S2): S = 7, R = 2 (E = 5), fake clock. t0 holds 3 units, t2
// one; t2's w = 2 head H waits (4 + 2 > E) and holds. A newcomer n of a NEW
// tenant t1 (100 unit-s of history) is checked with T_act = 2 (tenantShare
// 3, H within it and before n) and queues; its enqueue raises T_act to 3,
// tenantShare drops to 2, H goes over it (1 + 2 > 2) — n is the
// fairness-first head now and fits. The enqueue's wake admits it (≤ 50 ms).
func TestExternalEnqueueWakesTheNewFairnessFirstHead(t *testing.T) {
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
	ch := make(chan admission, 2)
	rh := weightedReq(2)
	rh.MaxWait = 5 * time.Second
	startWaiter(fairXctx(ctx, "c2", "t2", 1), d, "H", rh, ch)
	waitQueued(t, d, ch, 1, "H queued (4 + 2 > E, holds)")
	rn := weightedReq(1)
	rn.MaxWait = 5 * time.Second
	start := time.Now()
	startWaiter(fairXctx(ctx, "n", "t1", 1), d, "N", rn, ch)
	select {
	case got := <-ch:
		lag := time.Since(start)
		t.Logf("%s admitted %v after its acquire (bound 50ms), err=%v", got.label, lag, got.err)
		if got.err != nil || got.label != "N" || lag > 50*time.Millisecond {
			t.Fatalf("N: %s err=%v after %v, want N admitted ≤ 50ms", got.label, got.err, lag)
		}
		got.lease.Release()
	case <-time.After(150 * time.Millisecond):
		t.Fatalf("N (fits, fairness-first after its own enqueue) still waits 150ms later — no wake after an external enqueue")
	}
}

// TestExternalFairnessViewFreezesBetweenEvents is FR4-2 (review 4 P-F):
// S = 7, R = 2 (E = 5), fake clock. Interactive holds 3; consumer a (fair
// weight 100) holds a unit and its w = 4 head is the fairness-first one and
// holds the class (3 + 4 ≤ S, it does not fit); consumer b (fair weight 1,
// 1 unit-s of history) waits with w = 1. 200 s pass on the clock without an
// event: a's slot time per fair weight would now exceed b's, but the order
// is frozen between events — the pick is unchanged (nil) and b keeps
// waiting. The next event (an interactive release) re-reads the order: b is
// the pick, fits, and is admitted by that release's wake (≤ 50 ms).
func TestExternalFairnessViewFreezesBetweenEvents(t *testing.T) {
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
	pickNow := func() string {
		d.mu.Lock()
		defer d.mu.Unlock()
		w, _ := d.externalPickLocked(d.targetLocked(testOrigin), ClassExternal, weightSlots)
		return waiterLabel(w)
	}
	before := pickNow()
	clk.Add(int64(200 * time.Second))
	after := pickNow()
	t.Logf("pick before %s, after 200 s on the clock without an event %s", before, after)
	if before != "nobody" || after != before {
		t.Fatalf("pick %s → %s across 200 s without an event, want nobody → nobody — the order moved with the clock alone", before, after)
	}
	select {
	case a := <-ch:
		t.Fatalf("%s admitted without an event (err %v)", a.label, a.err)
	case <-time.After(20 * time.Millisecond):
	}
	start := time.Now()
	il[0].Release()
	il = il[1:]
	got := recvAdmission(t, ch, "b after the next event")
	lag := time.Since(start)
	t.Logf("%s admitted %v after the next event (an interactive release), err=%v", got.label, lag, got.err)
	if got.err != nil || got.label != "b-head" || lag > 50*time.Millisecond {
		t.Fatalf("after the next event: %s err=%v after %v, want b admitted ≤ 50ms", got.label, got.err, lag)
	}
	got.lease.Release()
}

// TestExternalWakeOrderAfterAnAdmissionInTheSameWake pins the view clock at
// a lease start (review 4 FR4-2, mutant "a lease start does not move the
// view"): on the production clock (no seam) a wake that hands several units
// re-reads the order after each handoff; the just-admitted consumer must not
// look as if it had served less than nothing. E = 4 (S = 6, R = 2), one
// tenant: z holds all of E with one lease; a1, b1, a2, b2 (consumers a and b,
// w = 1, no history, shares 2 each) queue in this order. z's release frees
// four units in ONE wake: a1 first (arrival), then b1 — after a1's start a
// and b both have slot time 0 and b1 arrived first — never a2 (a's running
// time read at a view before a1's start would be negative). The last two
// follow the running time (b2 before a2 when the clock moved between the
// two starts).
func TestExternalWakeOrderAfterAnAdmissionInTheSameWake(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(6, 2))
	bg := context.Background()
	occ := mustAcquire(t, d, xctx(bg, "z", "tenant-a"), weightedReq(4))
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 4)
	for i, c := range []string{"a", "b", "a", "b"} {
		startWaiter(xctx(ctx, c, "tenant-a"), d, fmt.Sprintf("%s%d", c, 1+i/2), weightedReq(1), ch)
		waitQueued(t, d, ch, i+1, "waiter queued")
	}
	occ.Release()
	got := make([]admission, 0, 4)
	for i := 0; i < 4; i++ {
		got = append(got, recvAdmission(t, ch, "admission after z's release"))
	}
	for i := 1; i < len(got); i++ {
		for j := i; j > 0 && got[j].lease.admitted.Before(got[j-1].lease.admitted); j-- {
			got[j], got[j-1] = got[j-1], got[j]
		}
	}
	order := make([]string, 0, 4)
	for _, a := range got {
		if a.err != nil {
			t.Fatalf("%s: %v", a.label, a.err)
		}
		order = append(order, a.label)
		a.lease.Release()
	}
	t.Logf("admission order in one wake: %v", order)
	if order[0] != "a1" || order[1] != "b1" {
		t.Fatalf("admission order %v, want a1, b1 first — the just-admitted consumer ranked before b", order)
	}
}

// TestExternalFairnessViewNeverMovesBack pins the monotone touch of the
// view clock (review 5 FR5-5): on the production clock an instant read
// before the mutex can meet a later event of another path; the view keeps
// the later instant (no negative running time of a lease).
func TestExternalFairnessViewNeverMovesBack(t *testing.T) {
	f := newExtFair()
	at := time.Unix(1000, 0)
	f.touch(at)
	f.touch(at.Add(-time.Second))
	if !f.viewAt.Equal(at) {
		t.Fatalf("view at %v after touch(t) and touch(t − 1 s), want %v", f.viewAt, at)
	}
	f.touch(at.Add(time.Second))
	if !f.viewAt.Equal(at.Add(time.Second)) {
		t.Fatalf("view at %v after touch(t + 1 s), want %v", f.viewAt, at.Add(time.Second))
	}
}
