package dispatch

// A03-W7 gates (design/03 §4.7, §4.2/§4.3): the weight of a lease. An
// external request holds Request.Weight units (engine sequences) of the slots
// S and of the external partition E = S − R; every admission term, the wake
// scan, the preempt need and the release count the sum of the weights. An
// in-house request always weighs 1. An external request heavier than E_eff is
// refused at once with ErrExternalNotOffered (a configuration error, never a
// wait). The snapshot keeps Held as the number of slot-holding leases (the
// foreign-load detector contract) and adds HeldWeight / HeldExternalWeight.
//
// Fixture values are deliberately pairwise distinct (S = 7, R = 2, E = 5,
// external_max_inflight 4, weights 3 and 1): a mix-up of S, R, E, the cap or
// a lease count with a weight sum cannot pass by coincidence (review F-A
// discipline of A03-W3).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	weightSlots    = 7
	weightReserved = 2
	weightE        = weightSlots - weightReserved // 5
)

func weightedReq(w int) Request {
	r := externalReq()
	r.Weight = w
	return r
}

// weightPolicy is the slot-managed fixture S = 7, R = 2 (E = 5).
func weightPolicy() Policy { return partitionPolicy(weightSlots, weightReserved) }

// TestWeightHeavyExternalWaitsForCapacity is the masterplan gate: on S = 7,
// R = 2 (E = 5) an external acquire of weight 3 is admitted, a second one of
// weight 3 waits (3 + 3 > 5) and is admitted after the first release. A
// dispatcher that ignores the weight admits both (two leases, six units).
func TestWeightHeavyExternalWaitsForCapacity(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	x1 := mustAcquire(t, d, xctx(context.Background(), "c1", "tenant-a"), weightedReq(3))
	if ts := target(t, d); ts.HeldExternalWeight != 3 || ts.HeldExternal != 1 || ts.HeldWeight != 3 || ts.Held != 1 {
		t.Fatalf("after w=3: HeldExternalWeight=%d HeldExternal=%d HeldWeight=%d Held=%d, want 3/1/3/1",
			ts.HeldExternalWeight, ts.HeldExternal, ts.HeldWeight, ts.Held)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(xctx(ctx, "c2", "tenant-b"), d, "second", weightedReq(3), ch)
	waitFor(t, "second external acquire settled", func() bool {
		ts := target(t, d)
		return ts.HeldExternal+ts.External.Waiting == 2
	})
	if ts := target(t, d); ts.External.Waiting != 1 || ts.HeldExternal != 1 || ts.HeldExternalWeight != 3 {
		t.Fatalf("second w=3 on E=5: waiting=%d HeldExternal=%d HeldExternalWeight=%d, want 1/1/3 — 3+3 > E must wait",
			ts.External.Waiting, ts.HeldExternal, ts.HeldExternalWeight)
	}
	x1.Release()
	got := recvAdmission(t, ch, "second external after the first release")
	if got.err != nil || got.lease == nil {
		t.Fatalf("second external after the release: err=%v", got.err)
	}
	if ts := target(t, d); ts.HeldExternalWeight != 3 || ts.HeldExternal != 1 || ts.External.Waiting != 0 {
		t.Fatalf("after the handoff: HeldExternalWeight=%d HeldExternal=%d waiting=%d, want 3/1/0",
			ts.HeldExternalWeight, ts.HeldExternal, ts.External.Waiting)
	}
	got.lease.Release()
	if ts := target(t, d); ts.HeldWeight != 0 || ts.HeldExternalWeight != 0 || ts.Held != 0 {
		t.Fatalf("after both releases: HeldWeight=%d HeldExternalWeight=%d Held=%d, want 0/0/0", ts.HeldWeight, ts.HeldExternalWeight, ts.Held)
	}
}

// TestWeightAboveCapacityIsNotOffered: an external acquire heavier than E_eff
// (6 > E = 5 on a slot-managed target, 5 > external_max_inflight 4 on a
// self-admitting one) — or with a negative weight — is refused at once with
// ErrExternalNotOffered: no waiter, counted under not_offered, exactly one
// WARN per refusal, through both doors. It is a configuration error, never a
// rejection, even on an idle target.
func TestWeightAboveCapacityIsNotOffered(t *testing.T) {
	const warn = "heavier than its target's external capacity"
	cases := []struct {
		name   string
		pol    Policy
		weight int
	}{
		{name: "slot-managed w=6 > E=5", pol: weightPolicy(), weight: weightE + 1},
		{name: "slot-managed w=S=7", pol: weightPolicy(), weight: weightSlots},
		{name: "slot-managed negative weight", pol: weightPolicy(), weight: -1},
		{name: "self-admitting w=5 > max_inflight 4", weight: 5,
			pol: Policy{Targets: map[string]TargetPolicy{testOrigin: {ExternalOffer: true, ExternalMaxInflight: 4}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, h := newTestDispatcher(t, DefaultSettings(), tc.pol)
			ctx, cancel := context.WithCancel(xctx(context.Background(), "c1", "tenant-a"))
			defer cancel()
			done := make(chan error, 1)
			var took time.Duration
			go func() {
				start := time.Now()
				_, _, err := d.Acquire(ctx, weightedReq(tc.weight))
				took = time.Since(start)
				done <- err
			}()
			var err error
			select {
			case err = <-done:
			case <-time.After(time.Second):
				cancel()
				<-done
				t.Fatalf("w=%d: acquire still blocked after 1s, want ErrExternalNotOffered at once — never waiting", tc.weight)
			}
			if !errors.Is(err, ErrExternalNotOffered) {
				t.Fatalf("w=%d: err=%v, want ErrExternalNotOffered", tc.weight, err)
			}
			// "Not offered" is decided before the enqueue and before W5's budget:
			// no waiter, no timer. The bound only has to sit far below any wait
			// the budget would allow; 50 ms (as in A03-W5) leaves room for
			// scheduler noise on a loaded runner (one 5.31 ms sample at load 40).
			if took >= 50*time.Millisecond {
				t.Fatalf("w=%d: refusal took %v, want < 50ms (no enqueue, no timer)", tc.weight, took)
			}
			if _, _, err := d.TryAcquire(xctx(context.Background(), "c1", "tenant-a"), weightedReq(tc.weight)); !errors.Is(err, ErrExternalNotOffered) {
				t.Fatalf("w=%d try-acquire: err=%v, want ErrExternalNotOffered (not ErrWouldBlock)", tc.weight, err)
			}
			ts := target(t, d)
			if ts.External.Waiting != 0 || ts.Held != 0 || ts.HeldWeight != 0 || ts.ExternalRejects.NotOffered != 2 || ts.ExternalRejects.Cap != 0 {
				t.Fatalf("w=%d: waiting=%d Held=%d HeldWeight=%d not_offered=%d cap=%d, want 0/0/0/2/0",
					tc.weight, ts.External.Waiting, ts.Held, ts.HeldWeight, ts.ExternalRejects.NotOffered, ts.ExternalRejects.Cap)
			}
			if n := h.count(warn); n != 2 {
				t.Fatalf("w=%d: %d WARNs %q, want exactly one per refusal (2)", tc.weight, n, warn)
			}
		})
	}
	t.Run("weight at E is admitted", func(t *testing.T) {
		d, h := newTestDispatcher(t, DefaultSettings(), weightPolicy())
		l := mustAcquire(t, d, xctx(context.Background(), "c1", "tenant-a"), weightedReq(weightE))
		if ts := target(t, d); ts.HeldExternalWeight != weightE || ts.ExternalRejects.NotOffered != 0 || h.count(warn) != 0 {
			t.Fatalf("w=E: HeldExternalWeight=%d not_offered=%d warns=%d, want %d/0/0", ts.HeldExternalWeight, ts.ExternalRejects.NotOffered, h.count(warn), weightE)
		}
		l.Release()
	})
	t.Run("self-admitting without cap admits any weight up to the bound", func(t *testing.T) {
		// The bound is the literal math.MaxInt32, never derived from the
		// constant (review 2 F-B): a wider ExternalWeightUnbounded such as
		// math.MaxInt lets the sum of two such weights wrap, and "one above"
		// computed from the constant itself would wrap to a negative weight
		// that is refused for the wrong reason. Three leases at the bound
		// hold 3 × MaxInt32 units in the 64-bit sums without wrapping.
		const bound = math.MaxInt32
		pol := Policy{Targets: map[string]TargetPolicy{testOrigin: {ExternalOffer: true}}}
		d, _ := newTestDispatcher(t, DefaultSettings(), pol)
		if got := d.ExternalWeightCap(testOrigin); got != bound {
			t.Fatalf("ExternalWeightCap without a cap = %d, want the representation bound %d (math.MaxInt32)", got, bound)
		}
		var leases []*Lease
		for i := 1; i <= 3; i++ {
			leases = append(leases, mustAcquire(t, d, xctx(context.Background(), fmt.Sprintf("c%d", i), "tenant-a"), weightedReq(bound)))
		}
		ts := target(t, d)
		if want := 3 * int64(bound); int64(ts.HeldExternalWeight) != want || int64(ts.HeldWeight) != want || ts.HeldExternal != 3 || ts.Held != 3 {
			t.Fatalf("three leases at the bound: HeldExternalWeight=%d HeldWeight=%d HeldExternal=%d Held=%d, want %d/%d/3/3 — the weight sums must not wrap",
				ts.HeldExternalWeight, ts.HeldWeight, ts.HeldExternal, ts.Held, want, want)
		}
		for _, over := range []int64{int64(bound) + 1, math.MaxInt64} {
			if _, _, err := acquireWithin(t, d, xctx(context.Background(), "c9", "tenant-a"), weightedReq(int(over)), time.Second); !errors.Is(err, ErrExternalNotOffered) {
				t.Fatalf("w=%d above the bound: err=%v, want ErrExternalNotOffered", over, err)
			}
		}
		for _, l := range leases {
			l.Release()
		}
		if ts := target(t, d); ts.HeldWeight != 0 || ts.HeldExternalWeight != 0 || ts.Held != 0 {
			t.Fatalf("after the releases: HeldWeight=%d HeldExternalWeight=%d Held=%d, want 0/0/0", ts.HeldWeight, ts.HeldExternalWeight, ts.Held)
		}
	})
	t.Run("zero weight reads as one", func(t *testing.T) {
		d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
		l := mustAcquire(t, d, xctx(context.Background(), "c1", "tenant-a"), weightedReq(0))
		if ts := target(t, d); ts.HeldExternalWeight != 1 || ts.HeldWeight != 1 {
			t.Fatalf("w=0: HeldExternalWeight=%d HeldWeight=%d, want 1/1", ts.HeldExternalWeight, ts.HeldWeight)
		}
		l.Release()
	})
}

// TestWeightInternalAlwaysOne: an in-house acquire weighs 1 whatever its
// Request.Weight — interactive, background, a downgraded interactive and the
// try door all hold exactly one unit (HeldWeight +1 each), so seven of them
// with Weight 4 fill S = 7 and none waits.
func TestWeightInternalAlwaysOne(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	in := func(r Request) Request { r.Weight = 4; return r }
	var leases []*Lease
	step := func(what string, l *Lease) {
		t.Helper()
		leases = append(leases, l)
		if ts := target(t, d); ts.HeldWeight != len(leases) || ts.Held != len(leases) || ts.HeldInternal != len(leases) || ts.HeldExternalWeight != 0 {
			t.Fatalf("%s with Weight 4: HeldWeight=%d Held=%d HeldInternal=%d HeldExternalWeight=%d, want %d/%d/%d/0 — in-house weighs 1",
				what, ts.HeldWeight, ts.Held, ts.HeldInternal, ts.HeldExternalWeight, len(leases), len(leases), len(leases))
		}
	}
	step("interactive", mustAcquire(t, d, ictx(principal("i1")), in(interactiveReq())))
	step("background", mustAcquire(t, d, context.Background(), in(backgroundReq())))
	step("downgraded interactive", mustAcquire(t, d, context.Background(), in(interactiveReq())))
	l, _, err := d.TryAcquire(context.Background(), in(backgroundReq()))
	if err != nil {
		t.Fatalf("try-acquire background with Weight 4 on a target with free units: %v", err)
	}
	step("try-acquire background", l)
	for len(leases) < weightSlots {
		step(fmt.Sprintf("background %d", len(leases)+1), mustAcquire(t, d, context.Background(), in(backgroundReq())))
	}
	for _, l := range leases {
		l.Release()
	}
	if ts := target(t, d); ts.HeldWeight != 0 || ts.Held != 0 {
		t.Fatalf("after release: HeldWeight=%d Held=%d, want 0/0", ts.HeldWeight, ts.Held)
	}
}

// TestWeightDetectorContract is the detector contract (A-E5 no. 3): Held
// stays the number of slot-holding leases (the engine's http_requests_active
// counts requests), HeldWeight is the sum of the weights (the engine's
// running sequences), HeldExternalWeight its external part; in-house leases
// weigh 1, so HeldWeight − HeldExternalWeight == HeldInternal.
func TestWeightDetectorContract(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	x3 := mustAcquire(t, d, xctx(context.Background(), "c1", "tenant-a"), weightedReq(3))
	x1 := mustAcquire(t, d, xctx(context.Background(), "c2", "tenant-b"), weightedReq(1))
	b := mustAcquire(t, d, context.Background(), backgroundReq())
	ts := target(t, d)
	if ts.Held != 3 || ts.HeldWeight != 5 || ts.HeldExternalWeight != 4 || ts.HeldInternal != 1 ||
		ts.HeldExternal != 2 || ts.Inflight != 3 || ts.InflightInternal != 1 {
		t.Fatalf("Held=%d HeldWeight=%d HeldExternalWeight=%d HeldInternal=%d HeldExternal=%d Inflight=%d InflightInternal=%d, want 3/5/4/1/2/3/1",
			ts.Held, ts.HeldWeight, ts.HeldExternalWeight, ts.HeldInternal, ts.HeldExternal, ts.Inflight, ts.InflightInternal)
	}
	if ts.HeldWeight-ts.HeldExternalWeight != ts.HeldInternal || ts.HeldInternal+ts.HeldExternal != ts.Held {
		t.Fatalf("snapshot identities broken: %+v", ts)
	}
	x3.Release()
	if ts := target(t, d); ts.Held != 2 || ts.HeldWeight != 2 || ts.HeldExternalWeight != 1 {
		t.Fatalf("after the w=3 release: Held=%d HeldWeight=%d HeldExternalWeight=%d, want 2/2/1", ts.Held, ts.HeldWeight, ts.HeldExternalWeight)
	}
	x1.Release()
	b.Release()
	if ts := target(t, d); ts.Held != 0 || ts.HeldWeight != 0 || ts.HeldExternalWeight != 0 || ts.Inflight != 0 {
		t.Fatalf("after all releases: Held=%d HeldWeight=%d HeldExternalWeight=%d Inflight=%d, want 0/0/0/0",
			ts.Held, ts.HeldWeight, ts.HeldExternalWeight, ts.Inflight)
	}
}

// TestExternalWeightCap pins the chain-build read: E_eff of an offered
// target, 0 when it does not offer (no R, no offer, emergency stop, unknown
// origin), the named unbounded constant on an offered self-admitting target
// without a cap — pinned as the literal math.MaxInt32, the overflow bound of
// the weight sum (review 2 F-B) —; raw spellings resolve to the canonical
// origin.
func TestExternalWeightCap(t *testing.T) {
	cases := []struct {
		name string
		tp   TargetPolicy
		want int
	}{
		{name: "slot-managed S=7 R=2", tp: TargetPolicy{Slots: weightSlots, InternalReserved: weightReserved}, want: weightE},
		{name: "slot-managed narrowed by external_max_inflight 3", tp: TargetPolicy{Slots: weightSlots, InternalReserved: weightReserved, ExternalMaxInflight: 3}, want: 3},
		{name: "slot-managed without R", tp: TargetPolicy{Slots: weightSlots}, want: 0},
		{name: "self-admitting with external_max_inflight 4", tp: TargetPolicy{ExternalOffer: true, ExternalMaxInflight: 4}, want: 4},
		{name: "self-admitting without cap", tp: TargetPolicy{ExternalOffer: true}, want: math.MaxInt32},
		{name: "self-admitting without offer", tp: TargetPolicy{ExternalMaxInflight: 4}, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newTestDispatcher(t, DefaultSettings(), Policy{Targets: map[string]TargetPolicy{testOrigin: tc.tp}})
			if got := d.ExternalWeightCap(testOrigin); got != tc.want {
				t.Fatalf("ExternalWeightCap = %d, want %d", got, tc.want)
			}
			if got := d.ExternalWeightCap("http://GPU:8089/v1"); got != tc.want {
				t.Fatalf("ExternalWeightCap(raw spelling) = %d, want %d", got, tc.want)
			}
			if got := d.ExternalWeightCap("http://other:8089"); got != 0 {
				t.Fatalf("ExternalWeightCap(unknown origin) = %d, want 0", got)
			}
			s := DefaultSettings()
			s.Enabled = false
			d.UpdateSettings(s)
			if got := d.ExternalWeightCap(testOrigin); got != 0 {
				t.Fatalf("ExternalWeightCap under the emergency stop = %d, want 0", got)
			}
		})
	}
	if ExternalWeightUnbounded <= weightSlots*1000 {
		t.Fatalf("ExternalWeightUnbounded = %d must lie far above any slot count", ExternalWeightUnbounded)
	}
}

// TestWeightHeavyHeadBlocksOnlyItsBucket (A03-W4 rewrite of
// TestWeightHeavyHeadBlocksOnlyItsClass — the pick became weight-aware, d1):
// a waiting external head that does not fit (w = 4 on Σw_ext = 3, E = 5) lets
// a background newcomer through (work conservation, design/03 §4.2: an
// inadmissible higher-ranked head blocks no lower class), a light newcomer
// of the SAME consumer (w = 1, it would fit) queues behind it (FIFO inside a
// bucket), and a light newcomer of ANOTHER consumer (w = 1) is admitted at
// once — before A03-W4 it queued behind the heavy head of the foreign bucket
// (FIFO across the buckets). The release of the first lease hands the units
// to the heavy head (1 + 4 = E); the same-consumer light one (5 + 1 > E)
// waits for the heavy release.
func TestWeightHeavyHeadBlocksOnlyItsBucket(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	x1 := mustAcquire(t, d, xctx(context.Background(), "c1", "tenant-a"), weightedReq(3))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	heavyCh := make(chan admission, 1)
	sameCh := make(chan admission, 1)
	startWaiter(xctx(ctx, "c2", "tenant-a"), d, "heavy", weightedReq(4), heavyCh)
	waitFor(t, "heavy external head queued", func() bool { return waitingExternal(d) == 1 })
	b, _, err := acquireWithin(t, d, context.Background(), backgroundReq(), time.Second)
	if err != nil {
		t.Fatalf("background behind an inadmissible heavy external head: %v — work conservation broken", err)
	}
	startWaiter(xctx(ctx, "c2", "tenant-a"), d, "same", weightedReq(1), sameCh)
	waitFor(t, "same-consumer light queued behind the heavy head", func() bool { return waitingExternal(d) == 2 })
	other, _, err := acquireWithin(t, d, xctx(context.Background(), "c3", "tenant-b"), weightedReq(1), time.Second)
	if err != nil {
		t.Fatalf("light newcomer of another consumer behind a foreign heavy head that does not fit: %v, want admitted at once (d1)", err)
	}
	if ts := target(t, d); ts.External.Waiting != 2 || ts.HeldExternalWeight != 4 || ts.HeldWeight != 5 {
		t.Fatalf("after the newcomers: waiting=%d HeldExternalWeight=%d HeldWeight=%d, want 2/4/5 (the same-consumer light one queues behind its own heavy head)",
			ts.External.Waiting, ts.HeldExternalWeight, ts.HeldWeight)
	}
	x1.Release()
	heavy := recvAdmission(t, heavyCh, "heavy head after the first release")
	if heavy.err != nil {
		t.Fatalf("heavy head: %v", heavy.err)
	}
	if ts := target(t, d); ts.External.Waiting != 1 || ts.HeldExternalWeight != weightE || ts.HeldWeight != weightE+1 {
		t.Fatalf("after the first release: waiting=%d HeldExternalWeight=%d HeldWeight=%d, want 1/5/6 — the same-consumer light one does not fit (5+1 > E)",
			ts.External.Waiting, ts.HeldExternalWeight, ts.HeldWeight)
	}
	heavy.lease.Release()
	same := recvAdmission(t, sameCh, "same-consumer light after the heavy release")
	if same.err != nil {
		t.Fatalf("same-consumer light: %v", same.err)
	}
	if ts := target(t, d); ts.HeldExternalWeight != 2 || ts.HeldExternal != 2 || ts.HeldWeight != 3 {
		t.Fatalf("after the heavy release: HeldExternalWeight=%d HeldExternal=%d HeldWeight=%d, want 2/2/3", ts.HeldExternalWeight, ts.HeldExternal, ts.HeldWeight)
	}
	same.lease.Release()
	other.Release()
	b.Release()
}

// TestWeightWakeReadsHeadWeight: the wake hands freed units to an external
// head only when the head fits with its weight — one release of a light
// lease must not admit a heavy head that would overrun E.
func TestWeightWakeReadsHeadWeight(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	x4 := mustAcquire(t, d, xctx(context.Background(), "c1", "tenant-a"), weightedReq(4))
	x1 := mustAcquire(t, d, xctx(context.Background(), "c2", "tenant-a"), weightedReq(1))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(xctx(ctx, "c3", "tenant-b"), d, "heavy", weightedReq(2), ch)
	waitFor(t, "w=2 external queued at E", func() bool { return waitingExternal(d) == 1 })
	x1.Release() // Σw_ext 4: a w=2 head does not fit (4 + 2 > 5)
	if ts := target(t, d); ts.External.Waiting != 1 || ts.HeldExternalWeight != 4 {
		t.Fatalf("after a w=1 release: waiting=%d HeldExternalWeight=%d, want 1/4 — the w=2 head overruns E", ts.External.Waiting, ts.HeldExternalWeight)
	}
	x4.Release()
	got := recvAdmission(t, ch, "w=2 head after the w=4 release")
	if got.err != nil {
		t.Fatalf("w=2 head: %v", got.err)
	}
	if ts := target(t, d); ts.HeldExternalWeight != 2 {
		t.Fatalf("HeldExternalWeight=%d, want 2", ts.HeldExternalWeight)
	}
	got.lease.Release()
}

// TestWeightPolicyShrinkEndsOverweightWaiter: a waiting external acquire
// whose target shrinks below its weight (R raised: E 5 → 3, the waiter
// weighs 4) can never be admitted — it ends with ErrExternalNotOffered at
// the swap (one WARN), instead of blocking its class head-of-line; a lighter
// waiter behind it stays queued and is admitted once the units free up.
func TestWeightPolicyShrinkEndsOverweightWaiter(t *testing.T) {
	d, h := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	occ := mustAcquire(t, d, xctx(context.Background(), "occ", "tenant-a"), weightedReq(weightE))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 2)
	startWaiter(xctx(ctx, "c1", "tenant-a"), d, "heavy", weightedReq(4), ch)
	waitFor(t, "w=4 external queued", func() bool { return waitingExternal(d) == 1 })
	startWaiter(xctx(ctx, "c2", "tenant-b"), d, "light", weightedReq(1), ch)
	waitFor(t, "w=1 external queued", func() bool { return waitingExternal(d) == 2 })

	d.UpdatePolicy(partitionPolicy(weightSlots, 4)) // E = 3 < 4
	a := recvAdmission(t, ch, "overweight waiter after the shrink")
	if a.label != "heavy" || !errors.Is(a.err, ErrExternalNotOffered) || a.lease != nil {
		t.Fatalf("after the shrink: label=%s lease=%v err=%v, want heavy with ErrExternalNotOffered", a.label, a.lease != nil, a.err)
	}
	if ts := target(t, d); ts.External.Waiting != 1 || ts.ExternalRejects.NotOffered != 1 || ts.HeldExternalWeight != weightE {
		t.Fatalf("waiting=%d not_offered=%d HeldExternalWeight=%d, want 1/1/%d (the running lease lives out)",
			ts.External.Waiting, ts.ExternalRejects.NotOffered, ts.HeldExternalWeight, weightE)
	}
	if n := h.count("heavier than its target's external capacity"); n != 1 {
		t.Fatalf("%d overweight WARNs, want one per drain", n)
	}
	occ.Release()
	light := recvAdmission(t, ch, "light waiter after the release")
	if light.label != "light" || light.err != nil {
		t.Fatalf("light waiter: label=%s err=%v", light.label, light.err)
	}
	light.lease.Release()
}

// TestWeightPreemptCountsUnits: the preempt need counts free UNITS, not
// leases. S = 7, R = 2: one external lease of weight 5 fills E and two
// background leases fill R — three leases, seven units, no unit free. A
// waiting interactive acquire on a preempt-enabled target needs one victim;
// a lease-count view (7 − 3 = 4 "free") would preempt nobody and leave the
// human waiting.
func TestWeightPreemptCountsUnits(t *testing.T) {
	pol := Policy{Targets: map[string]TargetPolicy{testOrigin: {Slots: weightSlots, InternalReserved: weightReserved, PreemptBackground: true}}}
	d, _ := newTestDispatcher(t, DefaultSettings(), pol)
	x, xrun, err := d.Acquire(xctx(context.Background(), "c1", "tenant-a"), weightedReq(weightE))
	if err != nil {
		t.Fatalf("external w=E: %v", err)
	}
	bctx := make([]context.Context, 2)
	bl := make([]*Lease, 2)
	for i := range bl {
		l, runCtx, err := d.Acquire(context.Background(), backgroundReq())
		if err != nil {
			t.Fatalf("background %d: %v", i, err)
		}
		bl[i], bctx[i] = l, runCtx
	}
	if ts := target(t, d); ts.HeldWeight != weightSlots || ts.Held != 3 {
		t.Fatalf("fixture: HeldWeight=%d Held=%d, want 7/3", ts.HeldWeight, ts.Held)
	}
	ctx, cancel := context.WithCancel(ictx(principal("i1")))
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(ctx, d, "interactive", interactiveReq(), ch)
	waitFor(t, "one background victim canceled", func() bool {
		return context.Cause(bctx[0]) != nil || context.Cause(bctx[1]) != nil
	})
	if ts := target(t, d); ts.Preempt.PreemptsTotal != 1 {
		t.Fatalf("PreemptsTotal=%d, want exactly one victim for one missing unit", ts.Preempt.PreemptsTotal)
	}
	for i, c := range bctx {
		if errors.Is(context.Cause(c), ErrPreempted) {
			bl[i].Release()
		}
	}
	got := recvAdmission(t, ch, "interactive after the victim release")
	if got.err != nil {
		t.Fatalf("interactive: %v", got.err)
	}
	if context.Cause(xrun) != nil {
		t.Fatal("the external lease was canceled — never a preempt victim")
	}
	got.lease.Release()
	for _, l := range bl {
		l.Release()
	}
	x.Release()
}

// TestWeightPartitionUnderMixedLoad is the partition gate with weight: a
// flood of external acquires with w ∈ {1,2,3} from eight consumers in two
// tenants plus a background load. The test keeps its OWN weight sums (added
// after the admission, subtracted before the release — never above the true
// sums) and checks at every handoff and in a parallel sampler: Σw_ext ≤ E,
// Σw ≤ S, the snapshot identities, and the background floor — while
// background waits, the in-house side holds at least R units.
func TestWeightPartitionUnderMixedLoad(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	// The test's own sums: raised after an admission, lowered before the
	// release, so at any instant they lie at or below the dispatcher's true
	// sums. One mutex for both — two separate atomic loads would pair an
	// external sum from before a release with a background sum from after the
	// handoff that release made, and report a false ext+bg > S.
	var sums struct {
		sync.Mutex
		ext, bg int64
	}
	add := func(ext, bg int64) {
		sums.Lock()
		sums.ext += ext
		sums.bg += bg
		sums.Unlock()
	}
	var fail atomic.Value
	report := func(format string, args ...any) { fail.CompareAndSwap(nil, fmt.Sprintf(format, args...)) }
	check := func(where string) {
		sums.Lock()
		e, b := sums.ext, sums.bg
		sums.Unlock()
		if e > weightE || e+b > weightSlots {
			report("%s: own sums ext=%d bg=%d, want ext <= E=%d and ext+bg <= S=%d", where, e, b, weightE, weightSlots)
		}
		ts := target(t, d)
		switch {
		case ts.HeldExternalWeight > weightE || ts.HeldWeight > weightSlots:
			report("%s: HeldExternalWeight=%d HeldWeight=%d past E=%d / S=%d", where, ts.HeldExternalWeight, ts.HeldWeight, weightE, weightSlots)
		case ts.HeldWeight-ts.HeldExternalWeight != ts.HeldInternal || ts.HeldInternal+ts.HeldExternal != ts.Held:
			report("%s: snapshot identities broken: %+v", where, ts)
		case ts.Background.Waiting > 0 && ts.HeldInternal < weightReserved:
			report("%s: background waits while the in-house side holds %d < R=%d units", where, ts.HeldInternal, weightReserved)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	const consumers, perConsumer = 8, 30
	for c := 1; c <= consumers; c++ {
		tenant := "tenant-a"
		if c > consumers/2 {
			tenant = "tenant-b"
		}
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			cctx := xctx(ctx, fmt.Sprintf("c%d", c), tenant)
			rng := rand.New(rand.NewSource(int64(c)))
			for i := 0; i < perConsumer; i++ {
				w := 1 + (c+i)%3
				l, _, err := d.Acquire(cctx, weightedReq(w))
				if err != nil {
					report("consumer %d acquire w=%d: %v", c, w, err)
					return
				}
				add(int64(w), 0)
				check(fmt.Sprintf("external handoff c%d w=%d", c, w))
				time.Sleep(time.Duration(rng.Intn(800)) * time.Microsecond)
				add(-int64(w), 0)
				l.Release()
			}
		}(c)
	}
	stopBG := make(chan struct{})
	var bgwg sync.WaitGroup
	var bgServed atomic.Int64
	for i := 0; i < 4; i++ {
		bgwg.Add(1)
		go func(i int) {
			defer bgwg.Done()
			rng := rand.New(rand.NewSource(int64(100 + i)))
			for {
				select {
				case <-stopBG:
					return
				default:
				}
				l, _, err := d.Acquire(ctx, backgroundReq())
				if err != nil {
					return
				}
				add(0, 1)
				bgServed.Add(1)
				check("background handoff")
				time.Sleep(time.Duration(rng.Intn(800)) * time.Microsecond)
				add(0, -1)
				l.Release()
			}
		}(i)
	}
	sampleDone := make(chan struct{})
	go func() {
		defer close(sampleDone)
		for {
			select {
			case <-stopBG:
				return
			default:
				check("sampler")
				time.Sleep(100 * time.Microsecond)
			}
		}
	}()
	extDone := make(chan struct{})
	go func() { wg.Wait(); close(extDone) }()
	select {
	case <-extDone:
	case <-time.After(20 * time.Second):
		t.Fatal("external flood did not complete within 20s")
	}
	close(stopBG)
	bgwg.Wait()
	<-sampleDone
	if msg := fail.Load(); msg != nil {
		t.Fatal(msg)
	}
	if bgServed.Load() == 0 {
		t.Fatal("background was never served under the external flood")
	}
	if ts := target(t, d); ts.HeldWeight != 0 || ts.Held != 0 || ts.External.Waiting != 0 {
		t.Fatalf("after the load: HeldWeight=%d Held=%d waiting=%d, want 0/0/0", ts.HeldWeight, ts.Held, ts.External.Waiting)
	}
}

// TestWeightSelfAdmittingCapCountsWeight: on an offered self-admitting
// target (no slots, external_max_inflight 4) the cap counts weight units —
// w = 3 is admitted, a waiting w = 2 (3 + 2 > 4) is admitted after the
// release, through the no-slots branch of the wake.
func TestWeightSelfAdmittingCapCountsWeight(t *testing.T) {
	pol := Policy{Targets: map[string]TargetPolicy{testOrigin: {ExternalOffer: true, ExternalMaxInflight: 4}}}
	d, _ := newTestDispatcher(t, DefaultSettings(), pol)
	x3 := mustAcquire(t, d, xctx(context.Background(), "c1", "tenant-a"), weightedReq(3))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(xctx(ctx, "c2", "tenant-b"), d, "second", weightedReq(2), ch)
	waitFor(t, "w=2 settled", func() bool {
		ts := target(t, d)
		return ts.HeldExternal+ts.External.Waiting == 2
	})
	if ts := target(t, d); ts.External.Waiting != 1 || ts.HeldExternalWeight != 3 {
		t.Fatalf("w=2 next to w=3 on external_max_inflight 4: waiting=%d HeldExternalWeight=%d, want 1/3", ts.External.Waiting, ts.HeldExternalWeight)
	}
	x3.Release()
	got := recvAdmission(t, ch, "w=2 after the release")
	if got.err != nil || !got.lease.slotHeld {
		t.Fatalf("w=2: err=%v — an external lease always holds units", got.err)
	}
	if ts := target(t, d); ts.HeldExternalWeight != 2 || ts.HeldWeight != 2 {
		t.Fatalf("HeldExternalWeight=%d HeldWeight=%d, want 2/2", ts.HeldExternalWeight, ts.HeldWeight)
	}
	got.lease.Release()
}

// TestWeightSlotTermCountsBackgroundInE: background may use free units of
// the external partition; an external acquire must then fit BOTH terms —
// heldExt + w ≤ E and held + w ≤ S. Four background leases (two of them in
// E) leave three units: w = 4 waits although 0 + 4 ≤ E, w = 3 is admitted.
func TestWeightSlotTermCountsBackgroundInE(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	var bl []*Lease
	for i := 0; i < 4; i++ {
		bl = append(bl, mustAcquire(t, d, context.Background(), backgroundReq()))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(xctx(ctx, "c1", "tenant-a"), d, "w4", weightedReq(4), ch)
	waitFor(t, "w=4 settled", func() bool {
		ts := target(t, d)
		return ts.HeldExternal+ts.External.Waiting == 1
	})
	if ts := target(t, d); ts.External.Waiting != 1 || ts.HeldWeight != 4 {
		t.Fatalf("w=4 on held=4, S=7: waiting=%d HeldWeight=%d, want 1/4 — held + w > S must wait", ts.External.Waiting, ts.HeldWeight)
	}
	bl[0].Release()
	got := recvAdmission(t, ch, "w=4 after one background release")
	if got.err != nil {
		t.Fatalf("w=4: %v", got.err)
	}
	if ts := target(t, d); ts.HeldWeight != weightSlots || ts.HeldExternalWeight != 4 {
		t.Fatalf("HeldWeight=%d HeldExternalWeight=%d, want 7/4", ts.HeldWeight, ts.HeldExternalWeight)
	}
	got.lease.Release()
	for _, l := range bl[1:] {
		l.Release()
	}
}

// TestWeightCanceledHeavyHeadWakesTheNext (review F1): a waiting heavy
// external head that is canceled may leave a lighter waiter behind it that
// fits now (Σw_ext 3, E = 5, the next head weighs 2). The light waiter is the
// heavy head's own consumer (A03-W4: a light waiter of another consumer no
// longer waits behind a foreign head that does not fit — d1). Its admission must not
// wait for the next release on the target — and in o1 a background newcomer
// must not be refused behind an admissible external head that no wake
// serves (the B floor of design/03 §4.3).
func TestWeightCanceledHeavyHeadWakesTheNext(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	x1 := mustAcquire(t, d, xctx(context.Background(), "c1", "tenant-a"), weightedReq(3))
	hctx, hcancel := context.WithCancel(context.Background())
	defer hcancel()
	heavyCh := make(chan admission, 1)
	startWaiter(xctx(hctx, "c2", "tenant-a"), d, "heavy", weightedReq(4), heavyCh)
	waitFor(t, "heavy external head queued", func() bool { return waitingExternal(d) == 1 })
	lctx, lcancel := context.WithCancel(context.Background())
	defer lcancel()
	lightCh := make(chan admission, 1)
	startWaiter(xctx(lctx, "c2", "tenant-a"), d, "light", weightedReq(2), lightCh)
	waitFor(t, "light external queued behind it", func() bool { return waitingExternal(d) == 2 })

	hcancel()
	if a := recvAdmission(t, heavyCh, "canceled heavy head"); !errors.Is(a.err, context.Canceled) {
		t.Fatalf("heavy head: err=%v, want context.Canceled", a.err)
	}
	var light admission
	select {
	case light = <-lightCh:
	case <-time.After(time.Second):
		ts := target(t, d)
		t.Fatalf("light waiter stranded 1s after the heavy head's cancel: waiting=%d HeldExternalWeight=%d (3 + 2 <= E = 5) — the drop must wake the target",
			ts.External.Waiting, ts.HeldExternalWeight)
	}
	if light.err != nil {
		t.Fatalf("light: %v", light.err)
	}
	b, _, err := acquireWithin(t, d, context.Background(), backgroundReq(), time.Second)
	if err != nil {
		t.Fatalf("background newcomer after the cancel: %v", err)
	}
	if ts := target(t, d); ts.HeldExternalWeight != weightE || ts.HeldWeight != weightE+1 || ts.External.Waiting != 0 {
		t.Fatalf("HeldExternalWeight=%d HeldWeight=%d waiting=%d, want 5/6/0", ts.HeldExternalWeight, ts.HeldWeight, ts.External.Waiting)
	}
	b.Release()
	light.lease.Release()
	x1.Release()
}

// TestWeightHotSwapDrainEndsEveryOverweightWaiter (review F2, P2): E 5 → 3
// → 6 → 3 with waiters of weight 4, 3, 2, 5 (in this order) behind a full
// partition. The first shrink ends EVERY waiter above the new E (4 and 5),
// a waiter at exactly E (3) stays; growing E admits a new w = 6 waiter into
// the queue, the second shrink ends it. Afterwards the release serves the
// survivors in fairness order under E = 3 (A03-W4 hardening, review 2 F-1
// (ii)): one tenant, two equal consumers ⇒ consumer share ⌈3/2⌉ = 2, but a
// consumer that holds nothing is within its share whatever its weight — so
// both are, the slot times are equal (0) and the older waiter w3 goes first
// (3 = E); w2 follows once w3's units are back (3 + 2 > E). Before the
// hardening w2 went first, because w3 alone was counted above its share.
func TestWeightHotSwapDrainEndsEveryOverweightWaiter(t *testing.T) {
	const warn = "waiting acquires ended after the capacity shrank"
	d, h := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	occ := mustAcquire(t, d, xctx(context.Background(), "occ", "tenant-a"), weightedReq(weightE))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 8)
	for i, w := range []int{4, 3, 2, 5} {
		startWaiter(xctx(ctx, fmt.Sprintf("c%d", i), "tenant-b"), d, fmt.Sprintf("w%d", w), weightedReq(w), ch)
		n := i + 1
		waitFor(t, fmt.Sprintf("waiter %d queued", n), func() bool { return waitingExternal(d) == n })
	}
	d.UpdatePolicy(partitionPolicy(weightSlots, 4)) // E = 3
	ended := map[string]error{}
	for i := 0; i < 2; i++ {
		a := recvAdmission(t, ch, "drain after E 5 -> 3")
		ended[a.label] = a.err
	}
	if len(ended) != 2 || !errors.Is(ended["w4"], ErrExternalNotOffered) || !errors.Is(ended["w5"], ErrExternalNotOffered) {
		t.Fatalf("E = 3 ended %v, want exactly w4 and w5 with ErrExternalNotOffered (w3 = E stays)", ended)
	}
	if ts := target(t, d); ts.External.Waiting != 2 || ts.ExternalRejects.NotOffered != 2 || h.count(warn) != 1 {
		t.Fatalf("after E = 3: waiting=%d not_offered=%d warns=%d, want 2/2/1", ts.External.Waiting, ts.ExternalRejects.NotOffered, h.count(warn))
	}
	d.UpdatePolicy(partitionPolicy(weightSlots, 1)) // E = 6
	startWaiter(xctx(ctx, "c9", "tenant-b"), d, "w6", weightedReq(6), ch)
	waitFor(t, "w6 queued under E = 6", func() bool { return waitingExternal(d) == 3 })
	d.UpdatePolicy(partitionPolicy(weightSlots, 4)) // E = 3 again
	if a := recvAdmission(t, ch, "w6 after the second shrink"); a.label != "w6" || !errors.Is(a.err, ErrExternalNotOffered) {
		t.Fatalf("second shrink: %s %v, want w6 with ErrExternalNotOffered", a.label, a.err)
	}
	if ts := target(t, d); ts.External.Waiting != 2 || ts.ExternalRejects.NotOffered != 3 || h.count(warn) != 2 {
		t.Fatalf("after the second shrink: waiting=%d not_offered=%d warns=%d, want 2/3/2", ts.External.Waiting, ts.ExternalRejects.NotOffered, h.count(warn))
	}
	occ.Release()
	first := recvAdmission(t, ch, "w3 after the release")
	if first.label != "w3" || first.err != nil {
		t.Fatalf("after the release: %s %v, want w3 (both consumers hold nothing, so both are within their share; w3 is older)", first.label, first.err)
	}
	if ts := target(t, d); ts.External.Waiting != 1 || ts.HeldExternalWeight != 3 {
		t.Fatalf("waiting=%d HeldExternalWeight=%d, want 1/3 (w2 waits: 3 + 2 > E = 3)", ts.External.Waiting, ts.HeldExternalWeight)
	}
	first.lease.Release()
	second := recvAdmission(t, ch, "w2 after the w3 release")
	if second.label != "w2" || second.err != nil {
		t.Fatalf("%s %v, want w2", second.label, second.err)
	}
	second.lease.Release()
}

// TestWeightDrainBeforeScanSameWake (review F2, P6): the shrink drain runs
// BEFORE the rank scan of the same wake — a heavy head ends and the light
// waiter behind it (same consumer: A03-W4 admits a light waiter of another
// consumer past a head that does not fit at once, d1) that fits under the
// new E is admitted by that wake, not by a later release.
func TestWeightDrainBeforeScanSameWake(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	occ := mustAcquire(t, d, xctx(context.Background(), "occ", "tenant-a"), weightedReq(2))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 2)
	startWaiter(xctx(ctx, "c1", "tenant-b"), d, "heavy", weightedReq(4), ch)
	waitFor(t, "heavy queued", func() bool { return waitingExternal(d) == 1 })
	startWaiter(xctx(ctx, "c1", "tenant-b"), d, "light", weightedReq(1), ch)
	waitFor(t, "light queued", func() bool { return waitingExternal(d) == 2 })
	d.UpdatePolicy(partitionPolicy(weightSlots, 4)) // E = 3: heavy ends, light fits (2 + 1 <= 3)
	got := map[string]admission{}
	for i := 0; i < 2; i++ {
		a := recvAdmission(t, ch, "after the shrink")
		got[a.label] = a
	}
	if !errors.Is(got["heavy"].err, ErrExternalNotOffered) || got["light"].err != nil || got["light"].lease == nil {
		t.Fatalf("after the shrink: heavy=%v light=%v, want ErrExternalNotOffered / admitted in the same wake", got["heavy"].err, got["light"].err)
	}
	got["light"].lease.Release()
	occ.Release()
}

// TestWeightDrainWarnsOnlyWhenItEnds (review F6): the weight bound may stay
// above the queue after a heavy waiter left by cancel; a later shrink below
// that stale bound scans, ends nobody — and logs nothing.
func TestWeightDrainWarnsOnlyWhenItEnds(t *testing.T) {
	const warn = "waiting acquires ended after the capacity shrank"
	d, h := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	occ := mustAcquire(t, d, xctx(context.Background(), "occ", "tenant-a"), weightedReq(weightE))
	hctx, hcancel := context.WithCancel(context.Background())
	defer hcancel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 2)
	startWaiter(xctx(hctx, "c1", "tenant-b"), d, "heavy", weightedReq(4), ch)
	waitFor(t, "heavy queued", func() bool { return waitingExternal(d) == 1 })
	startWaiter(xctx(ctx, "c2", "tenant-b"), d, "light", weightedReq(1), ch)
	waitFor(t, "light queued", func() bool { return waitingExternal(d) == 2 })
	hcancel()
	if a := recvAdmission(t, ch, "canceled heavy"); a.label != "heavy" || !errors.Is(a.err, context.Canceled) {
		t.Fatalf("%s %v, want heavy canceled", a.label, a.err)
	}
	d.UpdatePolicy(partitionPolicy(weightSlots, 4)) // E = 3, below the stale bound 4
	if ts := target(t, d); ts.External.Waiting != 1 || ts.ExternalRejects.NotOffered != 0 || h.count(warn) != 0 {
		t.Fatalf("waiting=%d not_offered=%d warns=%d, want 1/0/0 — nobody was ended", ts.External.Waiting, ts.ExternalRejects.NotOffered, h.count(warn))
	}
	occ.Release()
	if a := recvAdmission(t, ch, "light after the release"); a.label != "light" || a.err != nil {
		t.Fatalf("%s %v, want light admitted", a.label, a.err)
	} else {
		a.lease.Release()
	}
}

// TestWeightPeekIsThePickAcrossBuckets (review F2, P7; A03-W4: the pick and
// the weight check are one function, externalPickLocked): the wake hands
// units to the heads that fit, in fairness order across the buckets.
// Consumer c1 queues w = 2 and later w = 1, consumer c2 in between w = 4, one
// tenant (consumer share ⌈5·10/20⌉ = 3): the release serves c1's w = 2
// (within its share; c2's w = 4 is above it), then c1's w = 1 — c2's w = 4
// does not fit (2 + 4 > E) and no longer blocks the other bucket (before
// A03-W4 it was the FIFO head and c1's w = 1 waited behind it).
func TestWeightPeekIsThePickAcrossBuckets(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	occ := mustAcquire(t, d, xctx(context.Background(), "occ", "tenant-a"), weightedReq(4))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 3)
	for i, step := range []struct {
		consumer, label string
		w               int
	}{{"c1", "a2", 2}, {"c2", "b4", 4}, {"c1", "c1", 1}} {
		startWaiter(xctx(ctx, step.consumer, "tenant-b"), d, step.label, weightedReq(step.w), ch)
		n := i + 1
		waitFor(t, step.label+" queued", func() bool { return waitingExternal(d) == n })
	}
	occ.Release()
	got := map[string]admission{}
	for i := 0; i < 2; i++ {
		a := recvAdmission(t, ch, "the wake after the release")
		if a.err != nil {
			t.Fatalf("%s: %v", a.label, a.err)
		}
		got[a.label] = a
	}
	if got["a2"].lease == nil || got["c1"].lease == nil {
		t.Fatalf("the release admitted %v, want a2 and c1 (b4 does not fit after a2: 2 + 4 > E = 5)", got)
	}
	if ts := target(t, d); ts.HeldExternalWeight != 3 || ts.External.Waiting != 1 {
		t.Fatalf("HeldExternalWeight=%d waiting=%d, want 3/1 — b4 waits", ts.HeldExternalWeight, ts.External.Waiting)
	}
	got["a2"].lease.Release()
	b4 := recvAdmission(t, ch, "b4 after the a2 release")
	if b4.label != "b4" || b4.err != nil {
		t.Fatalf("%s %v, want b4 (1 + 4 = E)", b4.label, b4.err)
	}
	b4.lease.Release()
	got["c1"].lease.Release()
}

// levelsOf returns the levels of the records whose message contains sub
// (levelRecorder, consumer_gate_test.go).
func (h *levelRecorder) levelsOf(sub string) []slog.Level {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []slog.Level
	for _, r := range h.recs {
		if strings.Contains(r.Message, sub) {
			out = append(out, r.Level)
		}
	}
	return out
}

// TestWeightRefusalsLogAtWarn (review 2 F-C): the weight refusals are a
// configuration error and log at WARN (briefing (c), design/03 §4.7) — the
// door refusal once per refused acquire through both doors, the swap drain
// once per drain. A record at INFO would vanish from the default log level.
func TestWeightRefusalsLogAtWarn(t *testing.T) {
	const door = "heavier than its target's external capacity (or with a negative weight) refused"
	const drain = "waiting acquires ended after the capacity shrank"
	h := &levelRecorder{}
	d := New(slog.New(h), DefaultSettings())
	d.UpdatePolicy(weightPolicy())
	t.Cleanup(d.Close)

	if _, _, err := d.Acquire(xctx(context.Background(), "c1", "tenant-a"), weightedReq(weightE+1)); !errors.Is(err, ErrExternalNotOffered) {
		t.Fatalf("acquire w=E+1: err=%v, want ErrExternalNotOffered", err)
	}
	if _, _, err := d.TryAcquire(xctx(context.Background(), "c1", "tenant-a"), weightedReq(weightE+1)); !errors.Is(err, ErrExternalNotOffered) {
		t.Fatalf("try-acquire w=E+1: err=%v, want ErrExternalNotOffered", err)
	}
	if got := h.levelsOf(door); len(got) != 2 || got[0] != slog.LevelWarn || got[1] != slog.LevelWarn {
		t.Fatalf("door refusal records at %v, want exactly two at WARN (one per refused acquire)", got)
	}

	occ := mustAcquire(t, d, xctx(context.Background(), "occ", "tenant-a"), weightedReq(weightE))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(xctx(ctx, "c2", "tenant-b"), d, "heavy", weightedReq(4), ch)
	waitFor(t, "w=4 external queued", func() bool { return waitingExternal(d) == 1 })
	d.UpdatePolicy(partitionPolicy(weightSlots, 4)) // E = 3 < 4
	if a := recvAdmission(t, ch, "overweight waiter after the shrink"); !errors.Is(a.err, ErrExternalNotOffered) {
		t.Fatalf("after the shrink: err=%v, want ErrExternalNotOffered", a.err)
	}
	if got := h.levelsOf(drain); len(got) != 1 || got[0] != slog.LevelWarn {
		t.Fatalf("drain records at %v, want exactly one at WARN", got)
	}
	occ.Release()
}

// TestWeightExpiredHeavyHeadWakesTheNext (A03-W5 × A03-W7, W5 review 2 F-6):
// a heavy external head whose wait budget runs out leaves its queue through
// dropWaiterLocked and wakes the target in the same mutex section, exactly
// like a canceled one. S = 7, R = 2 (E = 5): an external lease of weight 1
// runs, a head of weight E waits (1 + 5 > E) with MaxWait 300 ms, a light
// waiter of weight 2 of the SAME consumer queues behind it (A03-W4: a light
// waiter of another consumer passes a head that does not fit at once, d1;
// it would fit; FIFO inside the bucket keeps it behind
// the head) and in o1 a background newcomer still passes the inadmissible
// head (work conservation). The head ends with ErrWaitExpired after its
// budget; when its Acquire returns, the light waiter already holds its
// lease, and an o1 background newcomer is admitted at once. A drop without
// the wake strands the light waiter until the next release and turns the o1
// newcomer away (an admissible external head waits above it).
func TestWeightExpiredHeavyHeadWakesTheNext(t *testing.T) {
	const budget = 300 * time.Millisecond
	d, _ := newTestDispatcher(t, DefaultSettings(), weightPolicy())
	occ := mustAcquire(t, d, xctx(context.Background(), "occ", "tenant-a"), weightedReq(1))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		err  error
		took time.Duration
	}
	heavyCh := make(chan result, 1)
	heavy := weightedReq(weightE)
	heavy.MaxWait = budget
	go func() {
		start := time.Now()
		l, _, err := d.Acquire(xctx(ctx, "c1", "tenant-a"), heavy)
		if l != nil {
			l.Release()
		}
		heavyCh <- result{err: err, took: time.Since(start)}
	}()
	waitFor(t, "heavy head queued", func() bool { return waitingExternal(d) == 1 })
	lightCh := make(chan admission, 1)
	startWaiter(xctx(ctx, "c1", "tenant-a"), d, "light", weightedReq(2), lightCh)
	waitFor(t, "light waiter queued behind the head", func() bool { return waitingExternal(d) == 2 })
	b0, _, err := d.TryAcquire(context.Background(), backgroundReq())
	if err != nil {
		t.Fatalf("o1 background newcomer beside the inadmissible heavy head: %v, want admitted (work conservation)", err)
	}
	b0.Release()

	var h result
	select {
	case h = <-heavyCh:
	case <-time.After(5 * time.Second):
		t.Fatalf("heavy head still waiting 5s into a %v wait budget", budget)
	}
	if !errors.Is(h.err, ErrWaitExpired) || h.took < budget {
		t.Fatalf("heavy head: err=%v after %v, want ErrWaitExpired after its %v budget", h.err, h.took, budget)
	}
	ts := target(t, d)
	if ts.External.Waiting != 0 || ts.HeldExternal != 2 || ts.HeldExternalWeight != 3 || ts.ExternalRejects.Expired != 1 || ts.ExternalRejects.NotOffered != 0 {
		t.Fatalf("when the expired head's acquire returned: waiting=%d HeldExternal=%d HeldExternalWeight=%d expired=%d not_offered=%d, want 0/2/3/1/0 — the expiry must wake the light waiter in the same mutex section",
			ts.External.Waiting, ts.HeldExternal, ts.HeldExternalWeight, ts.ExternalRejects.Expired, ts.ExternalRejects.NotOffered)
	}
	b1, _, err := d.TryAcquire(context.Background(), backgroundReq())
	if err != nil {
		t.Fatalf("o1 background newcomer right after the heavy head expired: %v, want admitted", err)
	}
	b1.Release()
	light := recvAdmission(t, lightCh, "light waiter after the head expired")
	if light.label != "light" || light.err != nil || light.lease == nil {
		t.Fatalf("light waiter: label=%s err=%v", light.label, light.err)
	}
	t.Logf("heavy w=E ended after %v with ErrWaitExpired (budget %v); the light w=2 waiter held its lease when that acquire returned", h.took, budget)
	light.lease.Release()
	occ.Release()
	if ts := target(t, d); ts.HeldWeight != 0 || ts.Held != 0 || ts.External.Waiting != 0 {
		t.Fatalf("after the releases: HeldWeight=%d Held=%d waiting=%d, want 0/0/0", ts.HeldWeight, ts.Held, ts.External.Waiting)
	}
}
