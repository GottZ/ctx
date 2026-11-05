package dispatch

// A03-W9 gate (f), the escape against the classes ranked below background
// (A03-W2 review 2 F5, A03-W3 review 2 F-F with the reviewer's probe P16 as
// template): an aged, escape-capable background head counts as admissible in
// the rank scan of a newcomer of a lower-ranked class — XU under o2 here (XB
// in both orders: external_bulk_test.go) — while XU stays ahead of it in o1
// (the negative probe). This file builds without the XB symbols, so it runs
// against the base as well.

import (
	"context"
	"testing"
	"time"
)

// TestEscapeBeforeExternalUnderO2 is gate (f) for XU: under o2 an XU
// newcomer does not take the unit an aged background head is due; under o1
// XU ranks above background and takes it (negative probe).
func TestEscapeBeforeExternalUnderO2(t *testing.T) {
	t.Run("o2 background first", func(t *testing.T) {
		escapeVsNewcomer(t, ExternalOrderO2, externalReq(), "XU", true)
	})
	t.Run("o1 external first", func(t *testing.T) {
		escapeVsNewcomer(t, ExternalOrderO1, externalReq(), "XU", false)
	})
}

// escapeVsNewcomer stages gate (f): S = 2, R = 1, no interactive role (the
// coupling invariant allows the escape), one interactive lease, demand > 0
// for the whole probe, a background waiter that ages past
// background_aging_after with the second unit free and no wake in between.
// Then the newcomer req arrives: bgFirst = the aged background head is
// admitted and the newcomer waits until the interactive lease ends; else the
// newcomer is admitted at once and background keeps waiting (o1 with XU:
// XU ranks above background — the negative probe).
func escapeVsNewcomer(t *testing.T, order string, req Request, label string, bgFirst bool) {
	t.Helper()
	const aging = 20 * time.Millisecond
	s := agingSettings(aging)
	s.RankOrder = mustRankOrder(t, order)
	pol := Policy{Targets: map[string]TargetPolicy{testOrigin: {Slots: 2, InternalReserved: 1}}}
	d, _ := newTestDispatcher(t, s, pol)
	il := mustAcquire(t, d, ictx(principal("i")), interactiveReq())
	done := d.InteractiveArrived()
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 2)
	startWaiter(ctx, d, "B", backgroundReq(), ch)
	waitFor(t, "B queued behind the herald", func() bool { return waitingBackground(d) == 1 })
	time.Sleep(3 * aging) // the head ages; nothing wakes the target meanwhile
	startWaiter(xctx(ctx, "n", "tenant-a"), d, label, req, ch)
	a := recvAdmission(t, ch, "first admission after the newcomer")
	if a.err != nil {
		t.Fatalf("%s: %v", a.label, a.err)
	}
	defer a.lease.Release()
	switch {
	case bgFirst && a.label != "B":
		t.Fatalf("%s %s newcomer took the unit the aged background head is due (escape), want B first", order, label)
	case !bgFirst && a.label != label:
		t.Fatalf("%s: %s admitted first, want the %s newcomer (it ranks above background)", order, a.label, label)
	}
	if bgFirst && !a.lease.agedAdmit {
		t.Fatalf("background admitted without the aging escape")
	}
	select {
	case b := <-ch:
		t.Fatalf("%s admitted (%v) with the target full", b.label, b.err)
	case <-time.After(40 * time.Millisecond):
	}
	il.Release()
	b := recvAdmission(t, ch, "second admission after the interactive release")
	if b.err != nil {
		t.Fatalf("%s: %v", b.label, b.err)
	}
	b.lease.Release()
}

// TestEscapeYieldsOnRefusalWithoutWake is A03-W9 review 1 F-1: a newcomer
// below background refused on a path that does not enqueue — TryAcquire, or
// an Acquire whose wait budget is already spent — leaves no unit idle behind
// the escape-capable aged background head: the head is admitted at once
// (≤ 50 ms), not at the next release or reaper tick.
func TestEscapeYieldsOnRefusalWithoutWake(t *testing.T) {
	spent := externalReq()
	spent.ServeBy = time.Now().Add(-time.Second)
	for name, tc := range map[string]struct {
		order string
		req   Request
		try   bool
	}{
		"o2 XU TryAcquire":   {ExternalOrderO2, externalReq(), true},
		"o2 XU spent budget": {ExternalOrderO2, spent, false},
	} {
		t.Run(name, func(t *testing.T) { escapeYieldOnRefusal(t, tc.order, tc.req, tc.try) })
	}
}

// escapeYieldOnRefusal stages escapeVsNewcomer's target (S = 2, R = 1, one
// interactive lease, demand > 0, an aged background head, a free unit) and
// sends a newcomer that is refused without enqueueing; the aged head must be
// admitted within 50 ms.
func escapeYieldOnRefusal(t *testing.T, order string, req Request, try bool) {
	t.Helper()
	const aging = 20 * time.Millisecond
	s := agingSettings(aging)
	s.RankOrder = mustRankOrder(t, order)
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
	var err error
	var l *Lease
	if try {
		l, _, err = d.TryAcquire(xctx(context.Background(), "n", "tenant-a"), req)
	} else {
		l, _, err = d.Acquire(xctx(context.Background(), "n", "tenant-a"), req)
	}
	if l != nil {
		l.Release()
		t.Fatalf("%s newcomer admitted past the aged background head", order)
	}
	if err == nil {
		t.Fatalf("%s newcomer: no refusal", order)
	}
	select {
	case a := <-ch:
		if a.err != nil || a.label != "B" || time.Since(start) > 50*time.Millisecond {
			t.Fatalf("%s: %s (%v) after %v, want the aged background head within 50ms", order, a.label, a.err, time.Since(start))
		}
		a.lease.Release()
	case <-time.After(150 * time.Millisecond):
		t.Fatalf("%s: the newcomer was refused (%v) and the aged background head still waits 150ms later with a unit free", order, err)
	}
}
