package dispatch

// A03-W1 table gate (design/03 §4.1 C7 precondition, §7 row A03-W1): every
// class below numClasses has exactly one table row with a non-empty, pairwise
// distinct name; the table is the only source of String/valid/External; a
// value outside it is refused at both admission doors with ErrInvalidClass
// (not a rejection — a code bug, K29) and counted. The compile-time half of
// the gate (len(classTable) == numClasses) lives next to the table.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// outOfTable are values a future bug could carry: the first one past the
// table, the next one and the far end of the uint8 range.
var outOfTable = []Class{numClasses, numClasses + 1, Class(255)}

func TestClassTableCoversEveryClass(t *testing.T) {
	if len(classTable) != int(numClasses) {
		t.Fatalf("len(classTable) = %d, numClasses = %d — one row per class", len(classTable), numClasses)
	}
	seen := make(map[string]Class, numClasses)
	for c := Class(0); c < numClasses; c++ {
		if !c.valid() {
			t.Errorf("Class(%d) < numClasses but not valid — its table row is missing or empty", c)
		}
		name := c.String()
		if name == "" || name == invalidClassName {
			t.Errorf("Class(%d).String() = %q — every class below numClasses needs a table row with a name", c, name)
		}
		if prev, dup := seen[name]; dup {
			t.Errorf("Class(%d) and Class(%d) share the name %q — telemetry strings must be pairwise distinct", prev, c, name)
		}
		seen[name] = c
	}
	// The persisted dispatch_class vocabulary of the two in-house classes
	// (context_llm_log, design/03 §3.4) is a contract, not a free choice.
	if got := ClassInteractive.String(); got != "interactive" {
		t.Errorf("ClassInteractive.String() = %q, want interactive", got)
	}
	if got := ClassBackground.String(); got != "background" {
		t.Errorf("ClassBackground.String() = %q, want background", got)
	}
	// Both in-house classes are ctx' own work — never external (design/01 §4.5).
	for _, c := range []Class{ClassInteractive, ClassBackground} {
		if c.External() {
			t.Errorf("%s.External() = true, want false", c)
		}
	}
}

func TestClassOutsideTable(t *testing.T) {
	for _, c := range outOfTable {
		if c.valid() {
			t.Errorf("Class(%d).valid() = true, want false", c)
		}
		if c.External() {
			t.Errorf("Class(%d).External() = true, want false (fail-closed)", c)
		}
		if got := c.String(); got != invalidClassName {
			t.Errorf("Class(%d).String() = %q, want %q", c, got, invalidClassName)
		}
	}
}

// TestInvalidClassRefusedAtBothDoors: ErrInvalidClass at once from Acquire
// and TryAcquire on a full slot-capped target, a free one and a pass-through
// one, with and without a principal; nothing queued, held or inflight; the
// counter equals the number of attempts; one ERROR line per attempt; never a
// rejection (no 429 mapping — K29 maps it to 503 server_error in A03-W8).
func TestInvalidClassRefusedAtBothDoors(t *testing.T) {
	if IsRejection(ErrInvalidClass) || IsRejection(fmt.Errorf("wrapped: %w", ErrInvalidClass)) {
		t.Fatal("IsRejection(ErrInvalidClass) = true — a code bug must not map to 429")
	}
	type door func(*Dispatcher, context.Context, Request) (*Lease, context.Context, error)
	doors := map[string]door{
		"Acquire":    (*Dispatcher).Acquire,
		"TryAcquire": (*Dispatcher).TryAcquire,
	}
	for _, tc := range []struct {
		name     string
		pol      Policy
		occupied bool
	}{
		{"full 1-slot target", onSlotPolicy(1), true},
		{"free 1-slot target", onSlotPolicy(1), false},
		{"pass-through target", Policy{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, h := newTestDispatcher(t, DefaultSettings(), tc.pol)
			var occ *Lease
			if tc.occupied {
				var err error
				if occ, _, err = d.Acquire(withPrincipal(context.Background(), principal("occ")), interactiveReq()); err != nil {
					t.Fatalf("occupier: %v", err)
				}
			}
			attempts := 0
			for name, enter := range doors {
				for _, ctx := range []context.Context{context.Background(), withPrincipal(context.Background(), principal("x"))} {
					for _, c := range outOfTable {
						attempts++
						req := Request{Target: Target{Origin: testOrigin}, Class: c, Role: "test"}
						l, runCtx, err := enter(d, ctx, req)
						if !errors.Is(err, ErrInvalidClass) || l != nil || runCtx != nil {
							t.Errorf("%s(Class(%d)) = (lease %v, runCtx %v, err %v), want (nil, nil, ErrInvalidClass)",
								name, c, l != nil, runCtx != nil, err)
						}
					}
				}
			}
			snap := d.Snapshot()
			if snap.InvalidClass != int64(attempts) {
				t.Errorf("Snapshot.InvalidClass = %d, want %d (one per attempt)", snap.InvalidClass, attempts)
			}
			if snap.ClassDowngrades != 0 {
				t.Errorf("ClassDowngrades = %d — an invalid class must be refused, never downgraded", snap.ClassDowngrades)
			}
			if got := h.count("dispatch: acquire with an invalid class refused"); got != attempts {
				t.Errorf("ERROR lines = %d, want %d", got, attempts)
			}
			wantHeld := 0
			if tc.occupied {
				wantHeld = 1
			}
			ts := target(t, d)
			if ts.Held != wantHeld || ts.Inflight != wantHeld || ts.Interactive.Waiting != 0 || ts.Background.Waiting != 0 {
				t.Errorf("after refusals: Held=%d Inflight=%d waiting=%d/%d, want %d/%d/0/0",
					ts.Held, ts.Inflight, ts.Interactive.Waiting, ts.Background.Waiting, wantHeld, wantHeld)
			}
			if occ != nil {
				occ.Release()
				if ts := target(t, d); ts.Held != 0 || ts.Inflight != 0 {
					t.Errorf("after occupier release: Held=%d Inflight=%d, want 0/0", ts.Held, ts.Inflight)
				}
			}
		})
	}
}

// TestInvalidClassSecondLine pins the fail-closed defaults BEHIND the doors
// (design/03 §4.1 "jede Klassen-Verzweigung als Switch"): should a refactor
// ever let an out-of-table waiter past the door check, it is never eligible
// (K6), never enqueued but refused and counted (K7), its drop is a logged
// no-op (K12), its admission leaves no wait sample in any class's window
// (K9), and its Retry-After estimate is 0 (K14).
func TestInvalidClassSecondLine(t *testing.T) {
	d, h := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))
	d.mu.Lock()
	st := d.targetLocked(testOrigin)
	for _, c := range outOfTable {
		w := &waiter{class: c, role: "test", cancel: func(error) {}}
		if d.eligibleNowLocked(st, c, 1, 1) {
			t.Errorf("eligibleNowLocked(Class(%d)) = true, want false", c)
		}
		if err := d.enqueueLocked(st, w, 0); !errors.Is(err, ErrInvalidClass) {
			t.Errorf("enqueueLocked(Class(%d)) = %v, want ErrInvalidClass", c, err)
		}
		d.dropWaiterLocked(st, w)
		if st.interactive.total != 0 || st.background.len() != 0 {
			t.Errorf("Class(%d): queues interactive=%d background=%d after enqueue+drop, want 0/0",
				c, st.interactive.total, st.background.len())
		}
		if r := st.ring(c); r != nil {
			t.Errorf("ring(Class(%d)) = %p, want nil — an out-of-table value must not map onto a window", c, r)
		}
		l := d.admitLocked(st, w, time.Now(), false)
		d.finishLocked(l)
	}
	for i := range st.waits {
		if st.waits[i].count != 0 {
			t.Errorf("waits[%d].count = %d, want 0 — no window absorbs an out-of-table sample", i, st.waits[i].count)
		}
	}
	if len(st.inflight) != 0 || st.held != 0 {
		t.Errorf("after admit+finish: inflight=%d held=%d, want 0/0", len(st.inflight), st.held)
	}
	d.mu.Unlock()
	if got := d.Snapshot().InvalidClass; got != int64(len(outOfTable)) {
		t.Errorf("Snapshot.InvalidClass = %d after %d enqueue refusals, want %d", got, len(outOfTable), len(outOfTable))
	}
	for msg, want := range map[string]int{
		"dispatch: class without an eligibility case":                   len(outOfTable),
		"dispatch: acquire with an invalid class refused":               len(outOfTable),
		"dispatch: class without a drop case":                           len(outOfTable),
		"dispatch: class outside the class table — wait sample dropped": len(outOfTable),
	} {
		if got := h.count(msg); got != want {
			t.Errorf("ERROR lines %q = %d, want %d", msg, got, want)
		}
	}
	for _, c := range outOfTable {
		if got := d.RetryAfterHint(testOrigin, c); got != 0 {
			t.Errorf("RetryAfterHint(Class(%d)) = %v, want 0", c, got)
		}
	}
	if got := h.count("dispatch: class without a retry-after case"); got != len(outOfTable) {
		t.Errorf("retry-after ERROR lines = %d, want %d", got, len(outOfTable))
	}
}

// TestEveryClassHasEveryBranch is the other half of the C7 gate: for every
// class BELOW numClasses no fail-closed default fires. A new class that got
// its table row but not its case in one of the class switches (eligibility,
// enqueue, drop, wait ring, Retry-After) is red here instead of being
// refused or dropped in production (design/03 §4.1: a forgotten branch is
// test-red, not silent behavior).
func TestEveryClassHasEveryBranch(t *testing.T) {
	d, h := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))
	d.mu.Lock()
	st := d.targetLocked(testOrigin)
	for c := Class(0); c < numClasses; c++ {
		w := &waiter{class: c, principal: principal("x"), role: "test", cancel: func(error) {}}
		d.eligibleNowLocked(st, c, 1, 1)
		if err := d.enqueueLocked(st, w, 0); err != nil {
			t.Errorf("enqueueLocked(%s) = %v, want nil", c, err)
		}
		d.dropWaiterLocked(st, w)
		if r := st.ring(c); r != &st.waits[c] {
			t.Errorf("ring(%s) = %p, want its own window %p", c, r, &st.waits[c])
		}
		l := d.admitLocked(st, w, time.Now(), false)
		if got := st.waits[c].count; got != 1 {
			t.Errorf("waits[%s].count = %d after one admission, want 1", c, got)
		}
		d.finishLocked(l)
	}
	if st.interactive.total != 0 || st.background.len() != 0 || st.external.total != 0 || len(st.inflight) != 0 {
		t.Errorf("after enqueue+drop+admit+finish: interactive=%d background=%d external=%d inflight=%d, want 0/0/0/0",
			st.interactive.total, st.background.len(), st.external.total, len(st.inflight))
	}
	d.mu.Unlock()
	for c := Class(0); c < numClasses; c++ {
		d.RetryAfterHint(testOrigin, c)
	}
	if got := h.count("(fail-closed default)"); got != 0 {
		t.Errorf("%d fail-closed default(s) fired for classes below numClasses — a class switch lacks a case", got)
	}
	if got := d.Snapshot().InvalidClass; got != 0 {
		t.Errorf("Snapshot.InvalidClass = %d for classes below numClasses, want 0", got)
	}
}

// TestDoorOrderCtxErrBeforeInvalidClass pins the door order A03-W1 and W01-7
// build on (ctx.Err() → ErrInvalidClass → …; review 2 F3): a canceled ctx
// answers ctx.Err() even for an out-of-table class — the class check neither
// counts nor logs it, at both doors.
func TestDoorOrderCtxErrBeforeInvalidClass(t *testing.T) {
	d, h := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))
	ctx, cancel := context.WithCancel(withPrincipal(context.Background(), principal("x")))
	cancel()
	doors := map[string]func(*Dispatcher, context.Context, Request) (*Lease, context.Context, error){
		"Acquire":    (*Dispatcher).Acquire,
		"TryAcquire": (*Dispatcher).TryAcquire,
	}
	for name, enter := range doors {
		for _, c := range outOfTable {
			l, runCtx, err := enter(d, ctx, Request{Target: Target{Origin: testOrigin}, Class: c, Role: "test"})
			if !errors.Is(err, context.Canceled) || errors.Is(err, ErrInvalidClass) || l != nil || runCtx != nil {
				t.Errorf("%s(canceled ctx, Class(%d)) = (lease %v, runCtx %v, err %v), want (nil, nil, context.Canceled)",
					name, c, l != nil, runCtx != nil, err)
			}
		}
	}
	if got := d.Snapshot().InvalidClass; got != 0 {
		t.Errorf("Snapshot.InvalidClass = %d after canceled-ctx attempts, want 0 (ctx.Err() comes first)", got)
	}
	if got := h.count("dispatch: acquire with an invalid class refused"); got != 0 {
		t.Errorf("invalid-class ERROR lines = %d after canceled-ctx attempts, want 0", got)
	}
}

// TestClassTableGapIsRefusedNotFatal pins deviation A1 (valid() requires a
// named row) and the panic-freedom of every wait-ring read (review 2 F2/F4):
// a row emptied at run time — the shape a constant inserted mid-enum without
// its row compiles to — makes that class invalid: refused at both doors on a
// slot-capped and a pass-through target (never admitted), named "invalid",
// while a valid class keeps working and Snapshot / RetryAfterHint still
// answer instead of panicking on a negative ring index under the dispatcher.
func TestClassTableGapIsRefusedNotFatal(t *testing.T) {
	saved := classTable[ClassInteractive]
	classTable[ClassInteractive].name = ""
	t.Cleanup(func() { classTable[ClassInteractive] = saved })

	if ClassInteractive.valid() {
		t.Fatal("ClassInteractive.valid() = true with an empty table row, want false")
	}
	if got := ClassInteractive.String(); got != invalidClassName {
		t.Errorf("ClassInteractive.String() = %q with an empty table row, want %q", got, invalidClassName)
	}
	for _, tc := range []struct {
		name string
		pol  Policy
	}{
		{"1-slot target", onSlotPolicy(1)},
		{"pass-through target", Policy{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newTestDispatcher(t, DefaultSettings(), tc.pol)
			ctx := withPrincipal(context.Background(), principal("x"))
			if l, _, err := d.Acquire(ctx, interactiveReq()); !errors.Is(err, ErrInvalidClass) || l != nil {
				t.Errorf("Acquire(gap class) = (lease %v, err %v), want (nil, ErrInvalidClass)", l != nil, err)
				if l != nil {
					l.Release()
				}
			}
			if l, _, err := d.TryAcquire(ctx, interactiveReq()); !errors.Is(err, ErrInvalidClass) || l != nil {
				t.Errorf("TryAcquire(gap class) = (lease %v, err %v), want (nil, ErrInvalidClass)", l != nil, err)
				if l != nil {
					l.Release()
				}
			}
			l, _, err := d.Acquire(context.Background(), backgroundReq())
			if err != nil {
				t.Fatalf("Acquire(background) with a gap elsewhere: %v", err)
			}
			l.Release()
			snap := d.Snapshot()
			if snap.InvalidClass != 2 {
				t.Errorf("Snapshot.InvalidClass = %d, want 2", snap.InvalidClass)
			}
			// The class switches compare constants, so the emptied row of an
			// existing constant still takes its case — the door is what refuses
			// it. The pin here is that the ring read answers (nil window ⇒ no
			// samples) instead of panicking on a negative index.
			if got := d.RetryAfterHint(testOrigin, ClassInteractive); got < 0 || got > retryAfterCap {
				t.Errorf("RetryAfterHint(gap class) = %v, want within [0, %v]", got, retryAfterCap)
			}
			if ts := target(t, d); ts.Held != 0 || ts.Inflight != 0 || ts.Background.Samples != 1 {
				t.Errorf("after release: Held=%d Inflight=%d Background.Samples=%d, want 0/0/1",
					ts.Held, ts.Inflight, ts.Background.Samples)
			}
		})
	}
}

// TestWaitRingAccessorNilSafe (review 2 F2): ring is the only index into
// waits — its own window for a class below numClasses, nil outside the class
// table — and both window methods are nil-safe, so a caller that forgets the
// nil check neither panics nor leaves d.mu locked.
func TestWaitRingAccessorNilSafe(t *testing.T) {
	var st targetState
	for c := Class(0); c < numClasses; c++ {
		if r := st.ring(c); r != &st.waits[c] {
			t.Errorf("ring(%s) = %p, want %p", c, r, &st.waits[c])
		}
	}
	for _, c := range outOfTable {
		r := st.ring(c)
		if r != nil {
			t.Errorf("ring(Class(%d)) = %p, want nil", c, r)
		}
		r.add(time.Second)
		ws := WaitStats{}
		r.statsInto(&ws)
		if ws != (WaitStats{}) {
			t.Errorf("nil ring statsInto changed the view: %+v", ws)
		}
	}
	for i := range st.waits {
		if st.waits[i].count != 0 {
			t.Errorf("waits[%d].count = %d after out-of-table writes, want 0", i, st.waits[i].count)
		}
	}
}
