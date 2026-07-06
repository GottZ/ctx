// MW25 aging-escape probes (design/04 §7 wave FA gates): default-off
// neutrality, release-trigger escape, idle-target reaper-tick trigger,
// F-B7 cap (never past a waiting interactive), coupling invariant in both
// directions (no escape on interactive-role targets without
// preempt_background; an aged-admitted lease stays preemptable and counts
// into the waste metric), no-ping-pong (a preempted arm re-enqueues with a
// fresh wait clock), and the InteractiveRole derivation. Red probe
// documented in the wave handover: coupling predicate removed from
// agingEscapeLocked ⇒ TestAgingCouplingInvariantBothDirections red.
package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"
)

// agingSettings arms the escape on top of the registry defaults.
func agingSettings(after time.Duration) Settings {
	s := DefaultSettings()
	s.BackgroundAgingAfter = after
	return s
}

// agingPolicy hand-builds one declared target with explicit coupling flags.
func agingPolicy(slots int, interactiveRole, preempt bool) Policy {
	return Policy{Targets: map[string]TargetPolicy{
		testOrigin: {Slots: slots, PreemptBackground: preempt, InteractiveRole: interactiveRole},
	}}
}

// assertNotAdmitted proves a waiter stays queued across a short window.
func assertNotAdmitted(t *testing.T, what string, ch chan admission) {
	t.Helper()
	select {
	case a := <-ch:
		t.Fatalf("%s must NOT be admitted (err=%v)", what, a.err)
	case <-time.After(60 * time.Millisecond):
	}
}

// Gate: default 0 ⇒ the herald term is unweakened — an arbitrarily old
// background waiter is never admitted under demand > 0, neither on a
// release nor on a reaper tick (behavior byte-identical, E-F5/K6).
func TestAgingDefaultOffNeverEscapes(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), agingPolicy(1, false, false))
	occ, _, err := d.Acquire(context.Background(), backgroundReq())
	if err != nil {
		t.Fatalf("occupier: %v", err)
	}
	done := d.InteractiveArrived() // demand > 0 for the whole probe
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(ctx, d, "bg", backgroundReq(), ch)
	waitFor(t, "bg queued", func() bool { return waitingBackground(d) == 1 })
	time.Sleep(30 * time.Millisecond) // far past any plausible aging — but aging is OFF

	occ.Release() // trigger (a): release re-evaluates admission
	assertNotAdmitted(t, "background under demand with aging off (release trigger)", ch)
	d.ReapForTest(time.Now()) // trigger (b): reaper tick
	assertNotAdmitted(t, "background under demand with aging off (reaper trigger)", ch)
	if got := target(t, d).Preempt.AgedAdmitsTotal; got != 0 {
		t.Fatalf("aged_admits must stay 0 with the escape off, got %d", got)
	}
}

// Gate: an aged background waiter is admitted despite demand > 0 on the
// release trigger; a younger waiter behind it stays gated (the escape is
// re-checked per head). The admit is counted and marked.
func TestAgingEscapeAdmitsAgedWaiterOnRelease(t *testing.T) {
	const aging = 25 * time.Millisecond
	d, h := newTestDispatcher(t, agingSettings(aging), agingPolicy(1, false, false))
	occ, _, err := d.Acquire(context.Background(), backgroundReq())
	if err != nil {
		t.Fatalf("occupier: %v", err)
	}
	done := d.InteractiveArrived()
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	aged := make(chan admission, 1)
	startWaiter(ctx, d, "aged", backgroundReq(), aged)
	waitFor(t, "aged bg queued", func() bool { return waitingBackground(d) == 1 })
	time.Sleep(2 * aging) // head crosses the aging threshold
	young := make(chan admission, 1)
	startWaiter(ctx, d, "young", backgroundReq(), young)
	waitFor(t, "young bg queued", func() bool { return waitingBackground(d) == 2 })

	occ.Release()
	a := <-aged
	if a.err != nil {
		t.Fatalf("aged waiter must escape the herald term: %v", a.err)
	}
	// The younger head has NOT aged yet: it must stay gated even though the
	// slot frees again — one escape per aged waiter, no herald bypass.
	a.lease.Release()
	assertNotAdmitted(t, "young background behind a fresh aging window", young)
	if got := target(t, d).Preempt.AgedAdmitsTotal; got != 1 {
		t.Fatalf("aged_admits = %d, want 1 (only the aged head escaped)", got)
	}
	if !h.contains("aging escape") {
		t.Fatalf("expected the aging-escape admit log line")
	}
}

// Gate (idle-target probe, design/04 §4.6 trigger (b)): a target with a free
// slot, NO running lease and process-wide demand > 0 has no release event —
// the aged waiter must be admitted within reaper-tick cadence, not never.
func TestAgingEscapeIdleTargetAdmitsOnReaperTick(t *testing.T) {
	const aging = 40 * time.Millisecond
	d, _ := newTestDispatcher(t, agingSettings(aging), agingPolicy(1, false, false))
	done := d.InteractiveArrived() // herald gates the empty target
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(ctx, d, "bg", backgroundReq(), ch)
	waitFor(t, "bg queued on idle target", func() bool { return waitingBackground(d) == 1 })

	d.ReapForTest(time.Now()) // tick BEFORE the threshold: still gated
	assertNotAdmitted(t, "background below the aging threshold", ch)
	time.Sleep(aging + 20*time.Millisecond)
	d.ReapForTest(time.Now()) // tick past the threshold: escape admits
	a := <-ch
	if a.err != nil {
		t.Fatalf("aged waiter on idle target must be admitted on the reaper tick: %v", a.err)
	}
	a.lease.Release()
}

// Gate (F-B7 cap): an aged background waiter NEVER overtakes a waiting
// interactive acquire — expired aging + queued interactive ⇒ interactive
// first, background only once the interactive queue is empty again.
func TestAgingNeverOvertakesWaitingInteractive(t *testing.T) {
	const aging = 20 * time.Millisecond
	d, _ := newTestDispatcher(t, agingSettings(aging), agingPolicy(1, false, false))
	occ, _, err := d.Acquire(context.Background(), interactiveReq(principal("occ")))
	if err != nil {
		t.Fatalf("occupier: %v", err)
	}
	done := d.InteractiveArrived()
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bg := make(chan admission, 1)
	startWaiter(ctx, d, "bg", backgroundReq(), bg)
	waitFor(t, "bg queued", func() bool { return waitingBackground(d) == 1 })
	time.Sleep(2 * aging) // bg is aged well past the threshold
	ia := make(chan admission, 1)
	startWaiter(ctx, d, "ia", interactiveReq(principal("a")), ia)
	waitFor(t, "ia queued", func() bool { return waitingInteractive(d) == 1 })

	occ.Release()
	first := <-ia
	if first.err != nil {
		t.Fatalf("interactive must win against the aged background: %v", first.err)
	}
	// While interactive holds the slot the aged waiter keeps waiting — a
	// reaper tick must not fabricate capacity.
	d.ReapForTest(time.Now())
	assertNotAdmitted(t, "aged background behind an interactive lease", bg)

	first.lease.Release() // interactive queue empty now ⇒ escape may fire
	second := <-bg
	if second.err != nil {
		t.Fatalf("aged background must follow once interactive drained: %v", second.err)
	}
	second.lease.Release()
}

// Gate (coupling invariant, F-B7, both directions of the flag): on a target
// WITH an interactive role the escape requires preempt_background=true —
// expired aging on the non-preempt variant admits nothing (head-of-line
// blocking up to a full generation would be the damage case); flipping the
// policy to preempt-enabled admits the same waiter within NOTIFY latency
// (UpdatePolicy wake). Red probe: coupling predicate removed from
// agingEscapeLocked ⇒ the first half of this test fails.
func TestAgingCouplingInvariantBothDirections(t *testing.T) {
	const aging = 20 * time.Millisecond
	d, _ := newTestDispatcher(t, agingSettings(aging), agingPolicy(1, true, false))
	done := d.InteractiveArrived()
	defer done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(ctx, d, "bg", backgroundReq(), ch)
	waitFor(t, "bg queued", func() bool { return waitingBackground(d) == 1 })
	time.Sleep(2 * aging)

	d.ReapForTest(time.Now())
	assertNotAdmitted(t, "aged background on interactive-role target without preempt", ch)
	if got := target(t, d).Preempt.AgedAdmitsTotal; got != 0 {
		t.Fatalf("aged_admits = %d, want 0 while the invariant blocks", got)
	}

	// Direction two: preempt_background=true makes the escape legal — the
	// dispatcher can take the slot back, worst case is preemption latency.
	d.UpdatePolicy(agingPolicy(1, true, true))
	a := <-ch
	if a.err != nil {
		t.Fatalf("aged waiter must be admitted once the target is preempt-enabled: %v", a.err)
	}
	a.lease.Release()
}

// Gate (coupling, second direction of the preemption seam): an aged-admitted
// lease stays NORMALLY preemptable — a later interactive acquire cancels it
// with ErrPreempted, the aged_preempts waste metric counts it (the NEGATIVE
// condition of the FA activation gate), and a re-enqueued background acquire
// starts a FRESH wait clock: the next escape is a full aging period away
// (structural no-ping-pong bound).
func TestAgingEscapedLeaseStaysPreemptableAndCounts(t *testing.T) {
	const aging = 25 * time.Millisecond
	d, _ := newTestDispatcher(t, agingSettings(aging), agingPolicy(1, true, true))
	done := d.InteractiveArrived()
	defer done()

	type bgResult struct {
		lease *Lease
		ctx   context.Context
		err   error
	}
	bgCh := make(chan bgResult, 1)
	go func() {
		l, c, err := d.Acquire(context.Background(), backgroundReq())
		bgCh <- bgResult{l, c, err}
	}()
	waitFor(t, "bg queued", func() bool { return waitingBackground(d) == 1 })
	time.Sleep(2 * aging)
	d.ReapForTest(time.Now())
	bg := <-bgCh
	if bg.err != nil {
		t.Fatalf("aged admit: %v", bg.err)
	}

	// Interactive demand arrives: the aged lease is the (only) victim.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ia := make(chan admission, 1)
	startWaiter(ctx, d, "ia", interactiveReq(principal("a")), ia)
	waitFor(t, "aged victim canceled", func() bool { return bg.ctx.Err() != nil })
	if cause := context.Cause(bg.ctx); !errors.Is(cause, ErrPreempted) {
		t.Fatalf("aged lease preempt cause: got %v want ErrPreempted", cause)
	}
	ps := target(t, d).Preempt
	if ps.AgedAdmitsTotal != 1 || ps.AgedPreemptsTotal != 1 || ps.PreemptsTotal != 1 {
		t.Fatalf("waste metric after one aged admit + one preempt: %+v", ps)
	}
	bg.lease.Release() // wire return of the victim hands the slot over
	a := <-ia
	if a.err != nil {
		t.Fatalf("interactive after preempt: %v", a.err)
	}

	// No ping-pong: the preempted arm re-enqueues as a NEW waiter — its wait
	// clock is fresh, so a reaper tick right after the interactive release
	// must NOT re-admit it before a full aging period elapsed again.
	retry := make(chan admission, 1)
	startWaiter(ctx, d, "retry", backgroundReq(), retry)
	waitFor(t, "retry queued", func() bool { return waitingBackground(d) == 1 })
	a.lease.Release() // demand still > 0 (done not called): herald gates
	d.ReapForTest(time.Now())
	assertNotAdmitted(t, "re-enqueued background inside the fresh aging window", retry)
	time.Sleep(2 * aging)
	d.ReapForTest(time.Now())
	r := <-retry
	if r.err != nil {
		t.Fatalf("retry must escape after a full aging period: %v", r.err)
	}
	r.lease.Release()
}

// Gate: the InteractiveRole derivation (aging.go) — any interactive-capable
// role marks the target, "embed" counts as interactive (query-path embeds),
// pure background sidecars do not, a role-less group fails closed to
// interactive, and a foreign (non-authoritative) row's roles still count:
// serving interactive traffic is a physical property, not a K2 question.
func TestDeriveInteractiveRole(t *testing.T) {
	pol := DerivePolicy([]BackendRow{
		{Name: "gpu", Scope: GlobalScope, BaseURL: "http://gpu:8089",
			Roles: []string{"dream"}, Limits: map[string]any{"slots": 1}},
		{Name: "gpu-tenant", Scope: "acme", BaseURL: "http://gpu:8089",
			Roles: []string{"chat"}}, // foreign row, but its interactive role counts
		{Name: "sidecar", Scope: GlobalScope, BaseURL: "http://side:9000",
			Roles: []string{"dream", "digest"}, Limits: map[string]any{"slots": 1}},
		{Name: "embed", Scope: GlobalScope, BaseURL: "http://embed:8081",
			Roles: []string{"embed"}, Limits: map[string]any{"slots": 4}},
		{Name: "bare", Scope: GlobalScope, BaseURL: "http://bare:1234",
			Limits: map[string]any{"slots": 1}}, // no roles declared at all
	}, nil)
	cases := []struct {
		origin string
		want   bool
	}{
		{"http://gpu:8089", true},   // foreign chat role marks the physical target
		{"http://side:9000", false}, // background-only sidecar
		{"http://embed:8081", true}, // embed serves interactive query embeds
		{"http://bare:1234", true},  // fail-closed: unknown topology
	}
	for _, c := range cases {
		tp, ok := pol.Targets[c.origin]
		if !ok {
			t.Fatalf("%s: expected a declared target", c.origin)
		}
		if tp.InteractiveRole != c.want {
			t.Errorf("%s: InteractiveRole = %v, want %v", c.origin, tp.InteractiveRole, c.want)
		}
	}
}
