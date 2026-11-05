package dispatch

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestContinuationBeforeFairnessPick (A03-W4 × A03-W17 merge coordination,
// both briefings): a waiting continuation is the head of the external class
// and the fairness pick never reorders it. E = 1 (S = 2, R = 1): consumer
// k1 has served 60 s of slot time and asks for a continuation, consumer k2
// (no slot time — the fairness pick's first choice) waited before it. The
// next release goes to k1's continuation; k2 follows. Under every
// Settings.Fairness mode (A03-W17 review 2 F-6): the mode orders only the
// interactive queue, the continuation stays the external class head.
func TestContinuationBeforeFairnessPick(t *testing.T) {
	for _, mode := range []FairnessMode{FairnessFIFO, FairnessPrincipalRR} {
		t.Run(string(mode), func(t *testing.T) { continuationBeforeFairnessPick(t, mode) })
	}
}

func continuationBeforeFairnessPick(t *testing.T, mode FairnessMode) {
	s := DefaultSettings()
	s.Fairness = mode
	d, _ := newTestDispatcher(t, s, partitionPolicy(2, 1))
	var clk atomic.Int64
	clk.Store(time.Now().UnixNano())
	d.setFairClockForTest(func() time.Time { return time.Unix(0, clk.Load()) })
	bg := context.Background()
	first := mustAcquire(t, d, xctx(bg, "k1", "tenant-a"), weightedReq(1))
	clk.Add(int64(60 * time.Second))
	first.Release()
	occ := mustAcquire(t, d, xctx(bg, "occ", "tenant-b"), weightedReq(1))
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	ch := make(chan admission, 2)
	startWaiter(xctx(ctx, "k2", "tenant-a"), d, "k2", weightedReq(1), ch)
	waitQueued(t, d, ch, 1, "k2 queued")
	startWaiter(xctx(ctx, "k1", "tenant-a"), d, "k1-cont", contReq(1), ch)
	waitQueued(t, d, ch, 2, "k1's continuation queued")
	d.mu.Lock()
	st := d.targetLocked(testOrigin)
	grown := partitionPolicy(3, 1) // one unit free without the wake: the head view must name the continuation
	d.policy.Store(&grown)
	seen, _ := d.externalPickLocked(st, ClassExternal, 3)
	d.policy.Store(&Policy{Targets: map[string]TargetPolicy{testOrigin: {Slots: 2, InternalReserved: 1}}})
	d.mu.Unlock()
	if seen == nil || !seen.cont {
		t.Fatalf("head view (externalPickLocked) = %v, want k1's continuation before k2's bucket head", waiterLabel(seen))
	}
	occ.Release()
	got := recvAdmission(t, ch, "handoff after the release")
	if got.err != nil || got.label != "k1-cont" {
		t.Fatalf("handoff = %s (err %v), want k1-cont — the continuation is the class head, before the fairness pick", got.label, got.err)
	}
	got.lease.Release()
	next := recvAdmission(t, ch, "k2 after the continuation")
	if next.err != nil || next.label != "k2" {
		t.Fatalf("second handoff = %s (err %v), want k2", next.label, next.err)
	}
	next.lease.Release()
}
