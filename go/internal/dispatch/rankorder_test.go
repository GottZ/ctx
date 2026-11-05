package dispatch

import (
	"context"
	"testing"
)

// mutatedRankOrder is the A03-W2 mutation probe: background ranked before
// interactive (external last, A03-W3). The settings path refuses it
// (ClassInteractive must hold rank 0); only SetRankOrderForTest stores it,
// unvalidated.
var mutatedRankOrder = [numClasses]Class{ClassBackground, ClassInteractive, ClassExternal}

// TestRankOrderIsTheSource is the A03-W2 gate: the admission scan reads its
// class order from the rank table, not from class literals. Same input in
// every case — one occupied slot, one interactive and one background waiter,
// demand 0, one release — and only the rank table differs: the production
// order hands the slot to interactive, the mutated order (installed through
// the unvalidated test hook) to background, regardless of arrival order.
func TestRankOrderIsTheSource(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   bool
		bgFirst  bool
		want     string
		wantNext string
	}{
		{name: "production order, background arrives first", bgFirst: true, want: "ia", wantNext: "bg"},
		{name: "production order, interactive arrives first", want: "ia", wantNext: "bg"},
		{name: "mutated order, background arrives first", mutate: true, bgFirst: true, want: "bg", wantNext: "ia"},
		{name: "mutated order, interactive arrives first", mutate: true, want: "bg", wantNext: "ia"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))
			if tc.mutate {
				d.SetRankOrderForTest(mutatedRankOrder)
			}
			occ, _, err := d.Acquire(withPrincipal(context.Background(), principal("occ")), interactiveReq())
			if err != nil {
				t.Fatalf("occupier: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch := make(chan admission, 2)
			queueBg := func() {
				startWaiter(ctx, d, "bg", backgroundReq(), ch)
				waitFor(t, "bg waiter queued", func() bool { return waitingBackground(d) == 1 })
			}
			queueIa := func() {
				startWaiter(withPrincipal(ctx, principal("a")), d, "ia", interactiveReq(), ch)
				waitFor(t, "ia waiter queued", func() bool { return waitingInteractive(d) == 1 })
			}
			if tc.bgFirst {
				queueBg()
				queueIa()
			} else {
				queueIa()
				queueBg()
			}
			if got := d.InteractiveDemand(); got != 0 {
				t.Fatalf("demand = %d, want 0 (the herald must not decide this case)", got)
			}

			occ.Release()
			first := <-ch
			if first.err != nil || first.label != tc.want {
				t.Fatalf("first handoff = %q (err %v), want %q", first.label, first.err, tc.want)
			}
			first.lease.Release()
			second := <-ch
			if second.err != nil || second.label != tc.wantNext {
				t.Fatalf("second handoff = %q (err %v), want %q", second.label, second.err, tc.wantNext)
			}
			second.lease.Release()
		})
	}
}

// TestRankOrderHotSwapSteersNextHandoff pins that the rank table is read at
// every wake (hot, like UpdateSettings): with four waiters queued behind one
// slot, each swap of the table decides the very next handoff.
func TestRankOrderHotSwapSteersNextHandoff(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))
	occ, _, err := d.Acquire(withPrincipal(context.Background(), principal("occ")), interactiveReq())
	if err != nil {
		t.Fatalf("occupier: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 4)
	startWaiter(ctx, d, "bg1", backgroundReq(), ch)
	waitFor(t, "bg1 queued", func() bool { return waitingBackground(d) == 1 })
	startWaiter(withPrincipal(ctx, principal("a")), d, "ia1", interactiveReq(), ch)
	waitFor(t, "ia1 queued", func() bool { return waitingInteractive(d) == 1 })
	startWaiter(ctx, d, "bg2", backgroundReq(), ch)
	waitFor(t, "bg2 queued", func() bool { return waitingBackground(d) == 2 })
	startWaiter(withPrincipal(ctx, principal("b")), d, "ia2", interactiveReq(), ch)
	waitFor(t, "ia2 queued", func() bool { return waitingInteractive(d) == 2 })

	holder := occ
	for _, step := range []struct {
		order [numClasses]Class
		want  string
	}{
		{mutatedRankOrder, "bg1"},
		{DefaultRankOrder(), "ia1"},
		{mutatedRankOrder, "bg2"},
		{DefaultRankOrder(), "ia2"},
	} {
		// Every slot is held: the hook's wake admits nothing, the swap acts on
		// the release below.
		d.SetRankOrderForTest(step.order)
		holder.Release()
		got := <-ch
		if got.err != nil || got.label != step.want {
			t.Fatalf("order %v: handoff = %q (err %v), want %q", step.order, got.label, got.err, step.want)
		}
		holder = got.lease
	}
	holder.Release()
}

// TestRankOrderSteersEligibleNow pins the immediate-admission half of the
// rank scan (design/03 §4.2 eligibleNow) white-box, with one real waiter
// queued on a free 1-slot target: a class is eligible only when its own
// queue is empty and no higher-ranked class has a waiting AND admissible
// head. A waiting but inadmissible higher-ranked class (background behind
// the herald) does not block a lower one — the work-conservation rule. The
// free slot next to a waiter is the window between a policy store and its
// wake (UpdatePolicy), the only place a newcomer meets it in production.
func TestRankOrderSteersEligibleNow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		order  [numClasses]Class
		queued Class
		demand bool
		wantIa bool
		wantBg bool
	}{
		// Production order, background waiting: lower-ranked, so it never
		// blocks interactive; background waits behind its own queue.
		{name: "production order, background waiting", order: DefaultRankOrder(), queued: ClassBackground, wantIa: true},
		// Production order, interactive waiting: a background newcomer stays
		// behind the waiting, admissible higher-ranked interactive head (no
		// barging across classes).
		{name: "production order, interactive waiting", order: DefaultRankOrder(), queued: ClassInteractive},
		// Mutated order, background waiting: the admissible background head
		// is ranked higher and blocks the interactive newcomer.
		{name: "mutated order, background waiting", order: mutatedRankOrder, queued: ClassBackground},
		// Mutated order, interactive waiting: background is ranked higher,
		// so the waiting interactive does not block it.
		{name: "mutated order, interactive waiting", order: mutatedRankOrder, queued: ClassInteractive, wantBg: true},
		// Mutated order under demand: the background head is inadmissible
		// (herald), so it does not block interactive.
		{name: "mutated order, background waiting, herald closed", order: mutatedRankOrder, queued: ClassBackground, demand: true, wantIa: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, h := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))
			if tc.demand {
				done := d.InteractiveArrived()
				defer done()
			}
			d.SetRankOrderForTest(tc.order)
			d.mu.Lock()
			st := d.targetLocked(testOrigin)
			w := &waiter{class: tc.queued, principal: principal("w"), role: "test", cancel: func(error) {}, admitCh: make(chan struct{})}
			if err := d.enqueueLocked(st, w); err != nil {
				d.mu.Unlock()
				t.Fatalf("enqueue %s: %v", tc.queued, err)
			}
			gotIa := d.eligibleNowLocked(st, ClassInteractive, 1, 1)
			gotBg := d.eligibleNowLocked(st, ClassBackground, 1, 1)
			d.dropWaiterLocked(st, w)
			d.mu.Unlock()
			if gotIa != tc.wantIa {
				t.Errorf("eligibleNow(interactive) with a waiting %s = %v, want %v", tc.queued, gotIa, tc.wantIa)
			}
			if gotBg != tc.wantBg {
				t.Errorf("eligibleNow(background) with a waiting %s = %v, want %v", tc.queued, gotBg, tc.wantBg)
			}
			if got := h.count("(fail-closed default)"); got != 0 {
				t.Errorf("%d fail-closed default(s) fired for table classes", got)
			}
		})
	}
}

// TestRankOrderValidation pins the settings path of the rank table: the zero
// value reads as the default order without a WARN; any other order must be a
// permutation of all classes with ClassInteractive on rank 0 (I-03-2), else
// the default is in effect and every New/UpdateSettings logs exactly one
// WARN — never a silent recalibration (A-E2 no. 3).
func TestRankOrderValidation(t *testing.T) {
	const warn = "dispatch: invalid rank order"
	def := DefaultRankOrder()
	if def[0] != ClassInteractive || !validRankOrder(def) {
		t.Fatalf("DefaultRankOrder() = %v: must be valid with interactive on rank 0", def)
	}
	if got := DefaultSettings().RankOrder; got != def {
		t.Fatalf("DefaultSettings().RankOrder = %v, want %v", got, def)
	}
	// ClassInteractive is 0: an all-interactive order IS the zero value, so it
	// reads as the default like an unset field (no WARN possible or needed).
	if ([numClasses]Class{ClassInteractive, ClassInteractive}) != ([numClasses]Class{}) {
		t.Fatalf("all-interactive order differs from the zero value — revisit the zero-value rule")
	}
	// The predicate itself rejects an all-interactive order; through
	// resolveRankOrder that order is the zero value (short-circuited above
	// validRankOrder). Since A03-W3 a duplicate that is NOT the zero value is
	// representable and runs through the table below.
	if validRankOrder([numClasses]Class{ClassInteractive, ClassInteractive}) {
		t.Errorf("validRankOrder(interactive, interactive) = true, want false (not a permutation)")
	}

	for _, tc := range []struct {
		name  string
		order [numClasses]Class
		warns int
	}{
		{name: "zero value", order: [numClasses]Class{}, warns: 0},
		{name: "default order", order: def, warns: 0},
		{name: "background first", order: [numClasses]Class{ClassBackground, ClassInteractive, ClassExternal}, warns: 1},
		// Refused on rank 0 already (interactive missing).
		{name: "background twice", order: [numClasses]Class{ClassBackground, ClassBackground, ClassExternal}, warns: 1},
		// A03-W2 handover 4: the duplicate with interactive on rank 0.
		{name: "interactive twice", order: [numClasses]Class{ClassInteractive, ClassInteractive, ClassBackground}, warns: 1},
		{name: "external missing", order: [numClasses]Class{ClassInteractive, ClassBackground, ClassBackground}, warns: 1},
		{name: "interactive first, enum end", order: [numClasses]Class{ClassInteractive, numClasses, ClassBackground}, warns: 1},
		{name: "interactive first, out-of-table value", order: [numClasses]Class{ClassInteractive, Class(255), ClassBackground}, warns: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := DefaultSettings()
			s.RankOrder = tc.order
			d, h := newTestDispatcher(t, s, onSlotPolicy(1))
			if got := h.count(warn); got != tc.warns {
				t.Errorf("New: %d WARN %q, want %d", got, warn, tc.warns)
			}
			if got := d.currentSettings().RankOrder; got != def {
				t.Errorf("New: effective order = %v, want the default %v", got, def)
			}
			for i := 1; i <= 2; i++ {
				d.UpdateSettings(s)
				if got := h.count(warn); got != tc.warns*(i+1) {
					t.Errorf("after %d UpdateSettings: %d WARN, want %d (exactly one per swap)", i, got, tc.warns*(i+1))
				}
				if got := d.currentSettings().RankOrder; got != def {
					t.Errorf("UpdateSettings: effective order = %v, want the default %v", got, def)
				}
			}
		})
	}
}

// TestRankScanSwitchesCoverEveryClass extends the C7 gate (A03-W1,
// class_table_test.go) to the class switches of the rank scan: for every
// class below numClasses each switch has a case (no fail-closed default
// fires), and a value outside the table is never admissible, never queued
// and never picked — each refusal logged at ERROR.
func TestRankScanSwitchesCoverEveryClass(t *testing.T) {
	// A03-W3: the target offers the external class (R = 1 of 2 slots), so
	// every table class is admissible on it while idle.
	d, h := newTestDispatcher(t, DefaultSettings(), partitionPolicy(2, 1))
	d.mu.Lock()
	st := d.targetLocked(testOrigin)
	for c := Class(0); c < numClasses; c++ {
		if !d.admissibleLocked(st, c, 1, 2) {
			t.Errorf("admissible(%s) on an idle 2-slot target with R = 1 = false, want true", c)
		}
		if d.queuedLocked(st, c) {
			t.Errorf("queued(%s) on an idle target = true, want false", c)
		}
		if w := d.pickAdmissibleLocked(st, c, FairnessFIFO, 2); w != nil {
			t.Errorf("pick(%s) on an idle target = %p, want nil", c, w)
		}
	}
	if got := h.count("(fail-closed default)"); got != 0 {
		t.Errorf("%d fail-closed default(s) fired for classes below numClasses — a rank-scan switch lacks a case", got)
	}
	for _, c := range outOfTable {
		if d.admissibleLocked(st, c, 1, 1) {
			t.Errorf("admissible(Class(%d)) = true, want false", c)
		}
		if d.queuedLocked(st, c) {
			t.Errorf("queued(Class(%d)) = true, want false", c)
		}
		if w := d.pickAdmissibleLocked(st, c, FairnessFIFO, 1); w != nil {
			t.Errorf("pick(Class(%d)) = %p, want nil", c, w)
		}
	}
	d.mu.Unlock()
	for msg, want := range map[string]int{
		"dispatch: class without an eligibility case": len(outOfTable),
		"dispatch: class without a queue case":        len(outOfTable),
		"dispatch: class without a wake case":         len(outOfTable),
	} {
		if got := h.count(msg); got != want {
			t.Errorf("ERROR lines %q = %d, want %d", msg, got, want)
		}
	}
}
