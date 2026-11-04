package dispatch

// A03-W17 gates (masterplan K21, design/02 §9.2 A03-6): the continuation
// acquire. A continuation (Request.Continuation, external class only) is the
// next sub-block of an external request whose earlier sub-block already held
// a lease. It waits at the HEAD of its class — before every waiter that is
// not a continuation, in every bucket — and is exempt from the external cap
// ladder and from the wait budget. It ends only with an admission, its ctx, a
// withdrawn offer or the emergency stop (ErrExternalNotOffered, X9), or the
// overweight drain after a swap (A03-W7). Rank order and partition still
// hold: a waiting, admissible interactive head goes first; in o1 the
// continuation goes before background, in o2 after it. The in-house classes
// ignore the field.

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

// contReq is a continuation acquire of weight w (A03-W17): the next
// sub-block of an external request that already held a lease.
func contReq(w int) Request {
	r := weightedReq(w)
	r.Continuation = true
	return r
}

// waitQueued waits until the external queue holds n waiters, failing at once
// when a waiter reports on ch instead (a refused or expired acquire must not
// read as "still queuing").
func waitQueued(t *testing.T, d *Dispatcher, ch chan admission, n int, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case a := <-ch:
			t.Fatalf("%s: %s ended before it queued (lease %v, err %v)", what, a.label, a.lease != nil, a.err)
		default:
		}
		if waitingExternal(d) == n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s (external waiting %d, want %d)", what, waitingExternal(d), n)
}

// TestContinuationHeadOfClass is the design/02 A03-6 gate: S = 4, R = 2
// (E = 2), external_max_wait 100 ms. K0 and K1 (Block 1) hold the partition;
// K2 and K3 need all of it (w = 2) and queue under the 20 s budget they
// started with. K1 releases Block 1 and asks for Block 2 (w = 2) while K0
// keeps the partition 300 ms. With the ladder full (queue_max 2) an ordinary
// acquire is ErrTargetSaturated, with room (queue_max 3) it is ErrWaitExpired
// after 100 ms; the continuation waits past both and is admitted at K0's
// release BEFORE K2 and K3 — the refusal counters stay unchanged, every lease
// is released. Under every Settings.Fairness mode (A03-W17 review 2 F-6:
// since A03-W4 the external class head is the fairness pick, and the mode
// must not reach it).
func TestContinuationHeadOfClass(t *testing.T) {
	for _, mode := range []FairnessMode{FairnessFIFO, FairnessPrincipalRR} {
		t.Run(string(mode), func(t *testing.T) { continuationHeadOfClass(t, mode) })
	}
}

func continuationHeadOfClass(t *testing.T, mode FairnessMode) {
	for _, tc := range []struct {
		name     string
		queueMax int
	}{
		{name: "cap ladder full", queueMax: 2},
		{name: "wait budget spent", queueMax: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := DefaultSettings()
			s.Fairness = mode
			s.ExternalQueueMax = tc.queueMax
			d, _ := newTestDispatcher(t, s, partitionPolicy(4, 2))
			bg := context.Background()
			k0 := mustAcquire(t, d, xctx(bg, "k0", "tenant-a"), weightedReq(1))
			block1 := mustAcquire(t, d, xctx(bg, "k1", "tenant-a"), weightedReq(1))
			ctx, cancel := context.WithCancel(bg)
			defer cancel()
			ch := make(chan admission, 3)
			startWaiter(xctx(ctx, "k2", "tenant-a"), d, "k2", weightedReq(2), ch)
			waitQueued(t, d, ch, 1, "k2 queued")
			startWaiter(xctx(ctx, "k3", "tenant-a"), d, "k3", weightedReq(2), ch)
			waitQueued(t, d, ch, 2, "k3 queued")
			// A waiting acquire keeps the budget it started with: the 100 ms
			// swap binds only acquires from here on.
			s.ExternalMaxWait = 100 * time.Millisecond
			d.UpdateSettings(s)
			before := target(t, d).ExternalRejects

			block1.Release() // heldExt 1: the w = 2 heads stay inadmissible
			startWaiter(xctx(ctx, "k1", "tenant-a"), d, "block2", contReq(2), ch)
			waitQueued(t, d, ch, 3, "block 2 queued")
			// The continuation is a waiter like any other in the views: the
			// bucket view sums to the queue depth.
			bucketWaiting := 0
			for _, bs := range target(t, d).Buckets {
				bucketWaiting += bs.Waiting
			}
			if bucketWaiting != 3 {
				t.Fatalf("bucket view waiting %d, want 3 (K2, K3 and the continuation)", bucketWaiting)
			}
			time.Sleep(300 * time.Millisecond) // the partition stays occupied past the budget
			select {
			case a := <-ch:
				t.Fatalf("%s ended while the partition was occupied: (lease %v, err %v) — a continuation has no cap and no wait budget", a.label, a.lease != nil, a.err)
			default:
			}

			k0.Release() // the next partition release
			order := make([]string, 0, 3)
			for i := 0; i < 3; i++ {
				a := recvAdmission(t, ch, "admission after the partition release")
				if a.err != nil {
					t.Fatalf("%s: err %v, want an admission", a.label, a.err)
				}
				if a.label == "block2" && a.lease.WaitDur() < 300*time.Millisecond {
					t.Fatalf("block 2 waited %v, want past the 300 ms hold", a.lease.WaitDur())
				}
				order = append(order, a.label)
				a.lease.Release()
			}
			if fmt.Sprint(order) != "[block2 k2 k3]" {
				t.Fatalf("admission order %v, want [block2 k2 k3] — the continuation is the class head", order)
			}
			ts := target(t, d)
			if ts.ExternalRejects != before {
				t.Fatalf("external rejects %+v → %+v, want unchanged (no cap, no budget for a continuation)", before, ts.ExternalRejects)
			}
			if ts.Inflight != 0 || ts.HeldWeight != 0 || ts.External.Waiting != 0 {
				t.Fatalf("after all releases: inflight %d held weight %d waiting %d, want 0/0/0 (Σacquire == Σrelease)", ts.Inflight, ts.HeldWeight, ts.External.Waiting)
			}
		})
	}
}

// TestContinuationRankOrder pins gate a-i: the continuation is the head of the
// EXTERNAL class, not a class of its own — a waiting, admissible interactive
// head is served first at a release; in o1 the continuation goes before a
// waiting background acquire, in o2 the background acquire goes first.
func TestContinuationRankOrder(t *testing.T) {
	bg := context.Background()
	t.Run("interactive first", func(t *testing.T) {
		d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(4, 2))
		i1 := mustAcquire(t, d, ictx(principal("i1")), interactiveReq())
		i2 := mustAcquire(t, d, ictx(principal("i2")), interactiveReq())
		x0 := mustAcquire(t, d, xctx(bg, "k0", "tenant-a"), weightedReq(1))
		block1 := mustAcquire(t, d, xctx(bg, "k1", "tenant-a"), weightedReq(1))
		ctx, cancel := context.WithCancel(bg)
		defer cancel()
		ch := make(chan admission, 2)
		startWaiter(xctx(ctx, "k1", "tenant-a"), d, "block2", contReq(1), ch)
		waitQueued(t, d, ch, 1, "continuation queued")
		startWaiter(withPrincipal(ctx, principal("i3")), d, "i3", interactiveReq(), ch)
		waitFor(t, "interactive queued", func() bool { return waitingInteractive(d) == 1 })

		x0.Release() // one slot and one unit of E free: both heads admissible
		if a := recvAdmission(t, ch, "first admission"); a.label != "i3" || a.err != nil {
			t.Fatalf("first admission %s (err %v), want i3 — interactive ranks first", a.label, a.err)
		} else {
			defer a.lease.Release()
		}
		if n := waitingExternal(d); n != 1 {
			t.Fatalf("continuation waiting %d, want 1 (S full again)", n)
		}
		i1.Release()
		if a := recvAdmission(t, ch, "continuation"); a.label != "block2" || a.err != nil {
			t.Fatalf("second admission %s (err %v), want block2", a.label, a.err)
		} else {
			a.lease.Release()
		}
		i2.Release()
		block1.Release()
	})
	for _, tc := range []struct {
		order string
		first string
	}{
		{order: ExternalOrderO1, first: "block2"},
		{order: ExternalOrderO2, first: "b3"},
	} {
		t.Run(tc.order, func(t *testing.T) {
			s := DefaultSettings()
			s.RankOrder = mustRankOrder(t, tc.order)
			d, _ := newTestDispatcher(t, s, partitionPolicy(4, 2))
			b1 := mustAcquire(t, d, bg, backgroundReq())
			b2 := mustAcquire(t, d, bg, backgroundReq())
			x0 := mustAcquire(t, d, xctx(bg, "k0", "tenant-a"), weightedReq(1))
			block1 := mustAcquire(t, d, xctx(bg, "k1", "tenant-a"), weightedReq(1))
			ctx, cancel := context.WithCancel(bg)
			defer cancel()
			ch := make(chan admission, 2)
			startWaiter(xctx(ctx, "k1", "tenant-a"), d, "block2", contReq(1), ch)
			waitQueued(t, d, ch, 1, "continuation queued")
			startWaiter(ctx, d, "b3", backgroundReq(), ch)
			waitFor(t, "background queued", func() bool { return waitingBackground(d) == 1 })

			x0.Release()
			a := recvAdmission(t, ch, "first admission")
			if a.label != tc.first || a.err != nil {
				t.Fatalf("%s: first admission %s (err %v), want %s", tc.order, a.label, a.err, tc.first)
			}
			a.lease.Release() // the other head follows
			second := recvAdmission(t, ch, "second admission")
			if second.err != nil {
				t.Fatalf("%s: second admission %s: %v", tc.order, second.label, second.err)
			}
			second.lease.Release()
			for _, l := range []*Lease{b1, b2, block1} {
				l.Release()
			}
		})
	}
}

// TestContinuationEndsWithTheOffer pins gate a-ii: a waiting continuation ends
// with ErrExternalNotOffered within the wake of the emergency stop, of a
// withdrawn offer and of a policy swap that shrank E below its weight — it
// never hangs behind a target that cannot admit it, and it is never a
// pass-through lease (X9). A canceled continuation vacates its place.
func TestContinuationEndsWithTheOffer(t *testing.T) {
	bg := context.Background()
	stop := func(d *Dispatcher) {
		s := DefaultSettings()
		s.Enabled = false
		d.UpdateSettings(s)
	}
	for _, tc := range []struct {
		name   string
		weight int
		end    func(*Dispatcher)
	}{
		{name: "emergency stop", weight: 1, end: stop},
		{name: "offer withdrawn", weight: 1, end: func(d *Dispatcher) { d.UpdatePolicy(onSlotPolicy(4)) }},
		{name: "partition shrank below the weight", weight: 2, end: func(d *Dispatcher) { d.UpdatePolicy(partitionPolicy(4, 3)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(4, 2))
			x0 := mustAcquire(t, d, xctx(bg, "k0", "tenant-a"), weightedReq(1))
			x1 := mustAcquire(t, d, xctx(bg, "k1", "tenant-a"), weightedReq(1))
			ctx, cancel := context.WithCancel(bg)
			defer cancel()
			ch := make(chan admission, 1)
			startWaiter(xctx(ctx, "k1", "tenant-a"), d, "block2", contReq(tc.weight), ch)
			waitQueued(t, d, ch, 1, "continuation queued")
			before := target(t, d).ExternalRejects.NotOffered

			tc.end(d)
			// The drain runs inside the swap's wake: the waiter is out of the
			// queue when the swap returns.
			if n := waitingExternal(d); n != 0 {
				t.Fatalf("external waiting %d right after the swap, want 0 — the continuation ends within the wake", n)
			}
			a := recvAdmission(t, ch, "continuation after the swap")
			if !errors.Is(a.err, ErrExternalNotOffered) || a.lease != nil {
				t.Fatalf("(lease %v, err %v), want ErrExternalNotOffered — never a hang, never a pass-through lease", a.lease != nil, a.err)
			}
			if got := target(t, d).ExternalRejects.NotOffered; got != before+1 {
				t.Fatalf("not_offered %d → %d, want +1", before, got)
			}
			x0.Release()
			x1.Release()
		})
	}
	t.Run("ctx cancel vacates", func(t *testing.T) {
		s := DefaultSettings()
		s.ExternalQueueMax = 1
		d, _ := newTestDispatcher(t, s, partitionPolicy(4, 2))
		x0 := mustAcquire(t, d, xctx(bg, "k0", "tenant-a"), weightedReq(1))
		x1 := mustAcquire(t, d, xctx(bg, "k1", "tenant-a"), weightedReq(1))
		ctx, cancel := context.WithCancel(bg)
		ch := make(chan admission, 1)
		startWaiter(xctx(ctx, "k1", "tenant-a"), d, "block2", contReq(1), ch)
		waitQueued(t, d, ch, 1, "continuation queued")
		cancel()
		if a := recvAdmission(t, ch, "canceled continuation"); !errors.Is(a.err, context.Canceled) || a.lease != nil {
			t.Fatalf("(lease %v, err %v), want context.Canceled", a.lease != nil, a.err)
		}
		ts := target(t, d)
		bucketWaiting := 0
		for _, bs := range ts.Buckets {
			bucketWaiting += bs.Waiting
		}
		if ts.External.Waiting != 0 || bucketWaiting != 0 {
			t.Fatalf("after the cancel: waiting %d, bucket waiting %d, want 0/0", ts.External.Waiting, bucketWaiting)
		}
		// The vacated place counts: an ordinary acquire fits under queue_max 1.
		ch2 := make(chan admission, 1)
		ctx2, cancel2 := context.WithCancel(bg)
		defer cancel2()
		startWaiter(xctx(ctx2, "k2", "tenant-a"), d, "k2", weightedReq(1), ch2)
		waitQueued(t, d, ch2, 1, "ordinary acquire queued after the cancel")
		x0.Release()
		if a := recvAdmission(t, ch2, "k2"); a.err != nil {
			t.Fatalf("k2: %v", a.err)
		} else {
			a.lease.Release()
		}
		x1.Release()
	})
}

// TestContinuationCanceledHeavyHeadWakes (review F3, M21): a heavy waiting
// continuation blocks its class like any head; canceling it wakes the target,
// so a lighter waiter behind it that fits is admitted at once — not stranded
// until the next release (the A03-W7 drop wake holds for the head list too).
func TestContinuationCanceledHeavyHeadWakes(t *testing.T) {
	bg := context.Background()
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(4, 2))
	x0 := mustAcquire(t, d, xctx(bg, "k0", "tenant-a"), weightedReq(1))
	cctx, ccancel := context.WithCancel(bg)
	defer ccancel()
	ch := make(chan admission, 2)
	startWaiter(xctx(cctx, "k1", "tenant-a"), d, "cont", contReq(2), ch)
	waitQueued(t, d, ch, 1, "heavy continuation queued")
	startWaiter(xctx(bg, "k2", "tenant-a"), d, "k2", weightedReq(1), ch)
	waitQueued(t, d, ch, 2, "light waiter queued behind the continuation")
	ccancel()
	got := map[string]admission{}
	deadline := time.After(time.Second)
	for len(got) < 2 {
		select {
		case a := <-ch:
			got[a.label] = a
		case <-deadline:
			t.Fatalf("after the cancel of the heavy continuation: %d of 2 ended — the light waiter stranded until the next release", len(got))
		}
	}
	if !errors.Is(got["cont"].err, context.Canceled) || got["k2"].lease == nil {
		t.Fatalf("cont err %v, k2 (lease %v, err %v): want the cancel and an admission", got["cont"].err, got["k2"].lease != nil, got["k2"].err)
	}
	got["k2"].lease.Release()
	x0.Release()
}

// TestContinuationCountsInHintDepth (review F3, M23): the Retry-After depth
// of the external class counts waiting continuations — they are served
// before a newcomer.
func TestContinuationCountsInHintDepth(t *testing.T) {
	bg := context.Background()
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(4, 2))
	warmRing(d, testOrigin, ClassExternal, waitRingK, 2*time.Second)
	x0 := mustAcquire(t, d, xctx(bg, "k0", "tenant-a"), weightedReq(1))
	x1 := mustAcquire(t, d, xctx(bg, "k1", "tenant-a"), weightedReq(1))
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(xctx(ctx, "k1", "tenant-a"), d, "cont", contReq(1), ch)
	waitQueued(t, d, ch, 1, "continuation queued")
	if got := d.RetryAfterHint(testOrigin, ClassExternal); got != 2*time.Second {
		t.Fatalf("hint %v with one waiting continuation, want 2s (depth 1 × p95 2s)", got)
	}
	cancel()
	recvAdmission(t, ch, "canceled continuation")
	x0.Release()
	x1.Release()
}

// TestContinuationTryDoor: TryAcquire shares Acquire's immediate half, so a
// continuation try is admitted when it would be the class head — past waiting
// non-continuations whose own weight does not fit — and would-blocks behind a
// waiting continuation. An ordinary try never overtakes a waiter of its own
// consumer (MW9; A03-W4 d1 lets it pass another consumer's head that does
// not fit).
func TestContinuationTryDoor(t *testing.T) {
	bg := context.Background()
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(4, 2))
	x0 := mustAcquire(t, d, xctx(bg, "k0", "tenant-a"), weightedReq(1))
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 2)
	startWaiter(xctx(ctx, "k2", "tenant-a"), d, "k2", weightedReq(2), ch)
	waitQueued(t, d, ch, 1, "heavy head queued")

	// A03-W4 (d1, hardened by review 2 F-1): the head's own consumer never
	// passes its waiting head; an ordinary try of ANOTHER consumer takes a
	// unit the head cannot use when it ranks before the head in the
	// fairness order — here k2's tenant is over its share (k0 holds one of
	// its units: 1 + 2 > tenantShare 2), the try is within it.
	if l, _, err := d.TryAcquire(xctx(bg, "k2", "tenant-a"), weightedReq(1)); !errors.Is(err, ErrWouldBlock) || l != nil {
		t.Fatalf("ordinary try behind its own consumer's waiting head: (lease %v, err %v), want ErrWouldBlock", l != nil, err)
	}
	if l, _, err := d.TryAcquire(xctx(bg, "k5", "tenant-a"), weightedReq(1)); err != nil || l == nil {
		t.Fatalf("ordinary try of another consumer past a waiting head that does not fit: (lease %v, err %v), want an admission", l != nil, err)
	} else {
		l.Release()
	}
	l, _, err := d.TryAcquire(xctx(bg, "k1", "tenant-a"), contReq(1))
	if err != nil || l == nil {
		t.Fatalf("continuation try with a free unit: (lease %v, err %v), want an admission", l != nil, err)
	}
	// E is full now; a waiting continuation is ahead of a second try.
	startWaiter(xctx(ctx, "k3", "tenant-a"), d, "k3-cont", contReq(1), ch)
	waitQueued(t, d, ch, 2, "continuation queued")
	if l2, _, err := d.TryAcquire(xctx(bg, "k4", "tenant-a"), contReq(1)); !errors.Is(err, ErrWouldBlock) || l2 != nil {
		t.Fatalf("continuation try behind a waiting continuation: (lease %v, err %v), want ErrWouldBlock", l2 != nil, err)
	}
	if n := waitingExternal(d); n != 2 {
		t.Fatalf("a try queued: external waiting %d, want 2", n)
	}
	l.Release()
	if a := recvAdmission(t, ch, "waiting continuation"); a.label != "k3-cont" || a.err != nil {
		t.Fatalf("after the release: %s (err %v), want k3-cont", a.label, a.err)
	} else {
		a.lease.Release()
	}
	x0.Release()
	if a := recvAdmission(t, ch, "heavy head"); a.err != nil {
		t.Fatalf("k2: %v", a.err)
	} else {
		a.lease.Release()
	}
}

// TestContinuationBeforeEveryBucketPick is the queue-level invariant a bucket
// pick must keep (A03-W4 rebases its fairness pick onto it): a continuation is
// the class head before every bucket — the same consumer's older ordinary
// waiter included — under every fairness mode, in the class head as in the
// pick, and continuations among themselves keep their arrival order. (The
// head view of the external class is the fairness pick since A03-W4,
// externalPickLocked — pinned dispatcher-level by
// TestContinuationBeforeFairnessPick.)
func TestContinuationBeforeEveryBucketPick(t *testing.T) {
	for _, mode := range []FairnessMode{FairnessFIFO, FairnessPrincipalRR} {
		t.Run(string(mode), func(t *testing.T) {
			q := newExternalQueue()
			mk := func(consumer string, seq uint64, cont bool) *waiter {
				return &waiter{seq: seq, class: ClassExternal, principal: xuPrincipal(consumer, "tenant-a"), weight: 1, cont: cont}
			}
			old := mk("k1", 1, false)
			other := mk("k2", 2, false)
			c1 := mk("k1", 3, true)
			c2 := mk("k3", 4, true)
			q.push(old)
			q.push(other)
			q.pushCont(c1)
			q.pushCont(c2)
			if q.total != 4 || q.byKey[xuPrincipal("k1", "").identityKey()] != 2 {
				t.Fatalf("counters total %d byKey(k1) %d, want 4/2 — a continuation is counted like every waiter", q.total, q.byKey[xuPrincipal("k1", "").identityKey()])
			}
			if q.headMinWeight() != c1.weight || q.cont[0] != c1 {
				t.Fatalf("class head = seq %d, want the first continuation (seq 3)", q.cont[0].seq)
			}
			var got []uint64
			for w := q.pick(mode); w != nil; w = q.pick(mode) {
				got = append(got, w.seq)
			}
			if len(got) != 4 || fmt.Sprint(got[:2]) != "[3 4]" {
				t.Fatalf("%s pick order %v, want both continuations [3 4] first, then the buckets", mode, got)
			}
			if q.total != 0 || len(q.byKey) != 0 || len(q.byTenant) != 0 || q.maxWeight != 0 {
				t.Fatalf("drained queue: total %d byKey %d byTenant %d maxWeight %d, want all 0", q.total, len(q.byKey), len(q.byTenant), q.maxWeight)
			}
		})
	}
}

// TestContinuationEligibleNowKeepsRank pins the immediate half white-box, on
// the window between a policy store and its wake (free capacity next to a
// queued waiter — TestRankOrderSteersEligibleNow's fixture): a continuation
// newcomer is eligible past waiting ordinary external acquires, but never
// past a waiting continuation and never past a waiting, admissible
// higher-ranked head (interactive; background under o2) — the work
// conservation rule still lets it pass an inadmissible one (herald closed).
func TestContinuationEligibleNowKeepsRank(t *testing.T) {
	for _, tc := range []struct {
		name   string
		order  string
		queued Class
		cont   bool
		demand bool
		want   bool
	}{
		{name: "ordinary external waiting", order: ExternalOrderO1, queued: ClassExternal, want: true},
		{name: "continuation waiting", order: ExternalOrderO1, queued: ClassExternal, cont: true},
		{name: "interactive waiting", order: ExternalOrderO1, queued: ClassInteractive},
		{name: "o1 background waiting", order: ExternalOrderO1, queued: ClassBackground, want: true},
		{name: "o2 background waiting", order: ExternalOrderO2, queued: ClassBackground},
		{name: "o2 background waiting, herald closed", order: ExternalOrderO2, queued: ClassBackground, demand: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := DefaultSettings()
			s.RankOrder = mustRankOrder(t, tc.order)
			d, _ := newTestDispatcher(t, s, partitionPolicy(4, 2))
			if tc.demand {
				done := d.InteractiveArrived()
				defer done()
			}
			p := principal("w")
			if tc.queued == ClassExternal {
				p = xuPrincipal("k9", "tenant-a")
			}
			w := &waiter{class: tc.queued, principal: p, role: "test", weight: 1, cont: tc.cont, cancel: func(error) {}, admitCh: make(chan struct{})}
			d.mu.Lock()
			defer d.mu.Unlock()
			st := d.targetLocked(testOrigin)
			if err := d.enqueueLocked(st, w); err != nil {
				t.Fatalf("enqueue %s: %v", tc.queued, err)
			}
			if got := d.continuationEligibleNowLocked(st, 1, d.slotsLocked(st.origin)); got != tc.want {
				t.Fatalf("continuation eligible with a waiting %s (cont %v) under %s = %v, want %v", tc.queued, tc.cont, tc.order, got, tc.want)
			}
		})
	}
}

// inHouseTrace runs a fixed script of interactive and background acquires,
// waits and releases on S = 2 — cap ladder refusals, a full background queue
// and the herald term included — and records every admission, refusal and the
// queue view after each step. cont sets Request.Continuation on every request.
func inHouseTrace(t *testing.T, cont bool) []string {
	t.Helper()
	s := DefaultSettings()
	s.InteractiveQueueMax = 2
	s.BackgroundQueueMax = 1
	d, _ := newTestDispatcher(t, s, onSlotPolicy(2))
	mark := func(r Request) Request { r.Continuation = cont; return r }
	var trace []string
	step := func(what string) {
		ts := target(t, d)
		trace = append(trace, fmt.Sprintf("%s: held=%d i=%d b=%d", what, ts.Held, ts.Interactive.Waiting, ts.Background.Waiting))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 8)
	recv := func() {
		a := recvAdmission(t, ch, "in-house admission")
		trace = append(trace, fmt.Sprintf("admit %s err=%v", a.label, a.err))
		if a.lease != nil {
			a.lease.Release()
		}
	}
	i1 := mustAcquire(t, d, ictx(principal("a")), mark(interactiveReq()))
	b1 := mustAcquire(t, d, context.Background(), mark(backgroundReq()))
	step("full")
	startWaiter(withPrincipal(ctx, principal("a")), d, "i2", mark(interactiveReq()), ch)
	waitFor(t, "i2", func() bool { return waitingInteractive(d) == 1 })
	startWaiter(withPrincipal(ctx, principal("b")), d, "i3", mark(interactiveReq()), ch)
	waitFor(t, "i3", func() bool { return waitingInteractive(d) == 2 })
	_, _, err := d.Acquire(ictx(principal("c")), mark(interactiveReq()))
	trace = append(trace, fmt.Sprintf("i4 err=%v", err))
	startWaiter(ctx, d, "b2", mark(backgroundReq()), ch)
	waitFor(t, "b2", func() bool { return waitingBackground(d) == 1 })
	_, _, err = d.Acquire(context.Background(), mark(backgroundReq()))
	trace = append(trace, fmt.Sprintf("b3 err=%v", err))
	step("queued")
	done := d.InteractiveArrived()
	b1.Release()
	recv() // i2 before i3 (FIFO), the herald keeps b2 out
	step("after b1")
	i1.Release()
	recv()
	step("after i1")
	done()
	waitFor(t, "slot free", func() bool { return target(t, d).Held == 0 || waitingBackground(d) == 0 })
	recv()
	step("end")
	// The try door shares the immediate half: on a free target a marked try
	// is admitted like an unmarked one, behind a full one it would-blocks.
	l, _, err := d.TryAcquire(ictx(principal("a")), mark(interactiveReq()))
	trace = append(trace, fmt.Sprintf("try i free err=%v", err))
	l2, _, err := d.TryAcquire(context.Background(), mark(backgroundReq()))
	trace = append(trace, fmt.Sprintf("try b free err=%v", err))
	_, _, err = d.TryAcquire(ictx(principal("b")), mark(interactiveReq()))
	trace = append(trace, fmt.Sprintf("try i full err=%v", err))
	for _, held := range []*Lease{l, l2} {
		if held != nil {
			held.Release()
		}
	}
	step("after tries")
	return trace
}

// TestContinuationIgnoredInHouse is gate a-iii: Continuation on an interactive or
// background request changes nothing — the in-house trace with the field set
// is byte-equal to the trace without it (no head placement, no cap exemption,
// no herald bypass).
func TestContinuationIgnoredInHouse(t *testing.T) {
	plain := inHouseTrace(t, false)
	marked := inHouseTrace(t, true)
	if fmt.Sprint(plain) != fmt.Sprint(marked) {
		t.Fatalf("in-house trace differs with Continuation set:\nplain:  %q\nmarked: %q", plain, marked)
	}
	t.Logf("in-house trace (%d steps): %q", len(plain), plain)
}

// TestContinuationRaceEmergencyStopRelease is the race gate: 1 000 rounds of
// a continuation, a release of the lease that fills E and the emergency stop,
// started in the same instant. Every continuation ends with exactly one lease
// or one error, Held is 0 after the releases, and no goroutine stays behind.
func TestContinuationRaceEmergencyStopRelease(t *testing.T) {
	bg := context.Background()
	d, _ := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1))
	on, off := DefaultSettings(), DefaultSettings()
	off.Enabled = false
	settle := func() int {
		runtime.GC()
		n := runtime.NumGoroutine()
		for i := 0; i < 50; i++ {
			time.Sleep(2 * time.Millisecond)
			if m := runtime.NumGoroutine(); m == n {
				return n
			} else {
				n = m
			}
		}
		return n
	}
	g0 := settle()
	var leases, refused int
	for round := 0; round < 1000; round++ {
		d.UpdateSettings(on)
		hold := mustAcquire(t, d, xctx(bg, "k0", "tenant-a"), weightedReq(1))
		start := make(chan struct{})
		res := make(chan admission, 1)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			<-start
			l, _, err := d.Acquire(xctx(bg, "k1", "tenant-a"), contReq(1))
			res <- admission{label: "cont", lease: l, err: err}
		}()
		go func() { defer wg.Done(); <-start; hold.Release() }()
		go func() { defer wg.Done(); <-start; d.UpdateSettings(off) }()
		close(start)
		wg.Wait()
		var a admission
		select {
		case a = <-res:
		case <-time.After(3 * time.Second):
			t.Fatalf("round %d: continuation neither admitted nor ended after release + emergency stop", round)
		}
		switch {
		case a.err == nil && a.lease != nil:
			leases++
			a.lease.Release()
		case errors.Is(a.err, ErrExternalNotOffered) && a.lease == nil:
			refused++
		default:
			t.Fatalf("round %d: (lease %v, err %v), want exactly one lease or ErrExternalNotOffered", round, a.lease != nil, a.err)
		}
		if ts := target(t, d); ts.Held != 0 || ts.HeldWeight != 0 || ts.Inflight != 0 || ts.External.Waiting != 0 {
			t.Fatalf("round %d: held %d weight %d inflight %d waiting %d, want all 0", round, ts.Held, ts.HeldWeight, ts.Inflight, ts.External.Waiting)
		}
	}
	if g1 := settle(); g1 != g0 {
		t.Fatalf("goroutines %d → %d after 1 000 rounds, want ±0", g0, g1)
	}
	t.Logf("1 000 rounds: %d leases, %d ErrExternalNotOffered", leases, refused)
}
