// MW18 preempt-path probes (design/02 §7 wave P1 gates a–g, i–k; the
// wire-level gate h lives in k1_wire_test.go against the real
// backends.Classify). Red probes documented in the wave handover: victim
// choice inverted to OLDEST ⇒ TestPreemptCancelsYoungestBackground red;
// watchdog force-release disabled ⇒ TestPreemptWatchdogForceRelease red.
package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"
)

// preemptPolicy declares the test origin preempt-enabled with n slots.
func preemptPolicy(slots int) Policy {
	return Policy{Targets: map[string]TargetPolicy{
		testOrigin: {Slots: slots, PreemptBackground: true},
	}}
}

func preemptStats(t *testing.T, d *Dispatcher) PreemptStats {
	t.Helper()
	return target(t, d).Preempt
}

// Gate (a)+(f): on a full preempt-enabled target an interactive acquire
// speaks exactly ONE cancel with cause ErrPreempted against the YOUNGEST
// background lease (E-P1/K3 contrast probe: the older lease keeps running),
// the victim's release hands the slot to the interactive waiter over the
// REGULAR release path, and the telemetry counts the sunk work.
func TestPreemptCancelsYoungestBackground(t *testing.T) {
	d, h := newTestDispatcher(t, DefaultSettings(), preemptPolicy(2))
	older, olderCtx, err := d.Acquire(context.Background(), backgroundReq())
	if err != nil {
		t.Fatalf("older background: %v", err)
	}
	time.Sleep(10 * time.Millisecond) // separate admitted timestamps
	younger, youngerCtx, err := d.Acquire(context.Background(), backgroundReq())
	if err != nil {
		t.Fatalf("younger background: %v", err)
	}
	time.Sleep(10 * time.Millisecond) // let the victim accumulate sunk occupancy
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(withPrincipal(ctx, principal("a")), d, "ia", interactiveReq(), ch)
	waitFor(t, "victim canceled", func() bool { return youngerCtx.Err() != nil })

	// Contrast probe: the OLDEST lease must keep running — an inverted
	// victim choice (design/01 §4.4 old wording) would trip here.
	if olderCtx.Err() != nil {
		t.Fatalf("victim choice inverted: the oldest background lease was canceled (E-P1 wants the youngest)")
	}
	// K1 gate: the cause chain carries ErrPreempted AND context.Canceled,
	// distinguishable from ErrReaped.
	cause := context.Cause(youngerCtx)
	if !errors.Is(cause, ErrPreempted) {
		t.Fatalf("cancel cause: got %v want ErrPreempted", cause)
	}
	if !errors.Is(cause, context.Canceled) {
		t.Fatalf("ErrPreempted must wrap context.Canceled through the chain (K1)")
	}
	if errors.Is(cause, ErrReaped) {
		t.Fatalf("preempt cause must not match ErrReaped")
	}
	if !h.contains("background lease preempted") {
		t.Fatalf("expected the preempt WARN")
	}
	ps := preemptStats(t, d)
	if ps.PreemptsTotal != 1 || ps.ForcedReleasesTotal != 0 {
		t.Fatalf("counters after one preempt: %+v", ps)
	}
	if ps.WastedMsTotal < 5 {
		t.Fatalf("preempt_wasted_ms_total must sum the victim's sunk occupancy (≥ the 10ms age): %+v", ps)
	}

	// The slot moves at the victim's RELEASE (wire return), not at cancel.
	select {
	case a := <-ch:
		t.Fatalf("interactive admitted before the victim released: %+v", a)
	case <-time.After(30 * time.Millisecond):
	}
	younger.Release()
	a := <-ch
	if a.err != nil {
		t.Fatalf("interactive not admitted after victim release: %v", a.err)
	}
	if got := preemptStats(t, d); got.ReleaseMsLast < 30 {
		t.Fatalf("preempt_release_ms must sample cancel→release (victim held ≥30ms): %+v", got)
	}
	a.lease.Release()
	older.Release()
}

// Gate (b), negative — degradation rule §4.5: preempt_background=false keeps
// the target a pure admission-control target; the running background lease
// is never canceled, interactive waits until the regular release.
func TestPreemptDisabledStaysAdmissionControl(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))
	bg, bgCtx, err := d.Acquire(context.Background(), backgroundReq())
	if err != nil {
		t.Fatalf("background: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(withPrincipal(ctx, principal("a")), d, "ia", interactiveReq(), ch)
	waitFor(t, "interactive queued", func() bool { return waitingInteractive(d) == 1 })
	time.Sleep(30 * time.Millisecond)
	if bgCtx.Err() != nil {
		t.Fatalf("background canceled without preempt_background=true")
	}
	if got := preemptStats(t, d); got.PreemptsTotal != 0 {
		t.Fatalf("preempt counter without activation: %+v", got)
	}
	bg.Release()
	a := <-ch
	if a.err != nil {
		t.Fatalf("interactive after regular release: %v", a.err)
	}
	a.lease.Release()
}

// Gate (d), PB7 single-preempt guard: two waiting interactive on a 1-slot
// target speak ONE cancel — a lease in eviction covers the second waiter.
// Falsifiability: a guard that did NOT count "in eviction" would find no
// candidate for the second waiter and emit the B5 WARN — its absence pins
// that the deficit math short-circuits BEFORE the candidate scan.
func TestPreemptSingleGuardTwoWaitersOneCancel(t *testing.T) {
	d, h := newTestDispatcher(t, DefaultSettings(), preemptPolicy(1))
	bg, bgCtx, err := d.Acquire(context.Background(), backgroundReq())
	if err != nil {
		t.Fatalf("background: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 2)
	startWaiter(withPrincipal(ctx, principal("a")), d, "ia1", interactiveReq(), ch)
	waitFor(t, "victim canceled", func() bool { return bgCtx.Err() != nil })
	startWaiter(withPrincipal(ctx, principal("b")), d, "ia2", interactiveReq(), ch)
	waitFor(t, "second waiter queued", func() bool { return waitingInteractive(d) == 2 })
	if got := preemptStats(t, d); got.PreemptsTotal != 1 {
		t.Fatalf("single-preempt guard: got %d cancels want 1", got.PreemptsTotal)
	}
	if h.contains("without cancelable background lease") {
		t.Fatalf("second waiter reached the candidate scan — a victim in eviction must cover it")
	}
	bg.Release()
	for i := 0; i < 2; i++ {
		a := <-ch
		if a.err != nil {
			t.Fatalf("waiter %q: %v", a.label, a.err)
		}
		a.lease.Release()
	}
}

// Multi-slot single-preempt guard: ONE waiter on a 2-slot target with two
// background leases frees exactly one slot — no cascade over the demand.
func TestPreemptSingleGuardMultiSlotNoCascade(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), preemptPolicy(2))
	_, olderCtx, err := d.Acquire(context.Background(), backgroundReq())
	if err != nil {
		t.Fatalf("older background: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	younger, youngerCtx, err := d.Acquire(context.Background(), backgroundReq())
	if err != nil {
		t.Fatalf("younger background: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(withPrincipal(ctx, principal("a")), d, "ia", interactiveReq(), ch)
	waitFor(t, "one victim canceled", func() bool { return youngerCtx.Err() != nil })
	time.Sleep(20 * time.Millisecond)
	if olderCtx.Err() != nil {
		t.Fatalf("cascade: both background leases canceled for one waiter (PB7)")
	}
	if got := preemptStats(t, d); got.PreemptsTotal != 1 {
		t.Fatalf("cancels: got %d want 1", got.PreemptsTotal)
	}
	younger.Release()
	a := <-ch
	if a.err != nil {
		t.Fatalf("waiter: %v", a.err)
	}
	a.lease.Release()
}

// Gate (c), I-D1: a full target held by INTERACTIVE leases is a no-op + WARN
// under interactive demand pressure — an interactive lease is never a victim
// (its cancel func does not exist in the registry, structurally).
func TestPreemptNeverCancelsInteractive(t *testing.T) {
	d, h := newTestDispatcher(t, DefaultSettings(), preemptPolicy(1))
	occ, occCtx, err := d.Acquire(withPrincipal(context.Background(), principal("occ")), interactiveReq())
	if err != nil {
		t.Fatalf("occupier: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(withPrincipal(ctx, principal("a")), d, "ia", interactiveReq(), ch)
	waitFor(t, "waiter queued", func() bool { return waitingInteractive(d) == 1 })
	time.Sleep(30 * time.Millisecond) // demand pressure holds — still no cancel
	if occCtx.Err() != nil {
		t.Fatalf("I-D1 violated: interactive lease canceled by the dispatcher")
	}
	if got := preemptStats(t, d); got.PreemptsTotal != 0 {
		t.Fatalf("preempt counter on interactive-only target: %+v", got)
	}
	if !h.contains("without cancelable background lease") {
		t.Fatalf("expected the B5 no-op WARN")
	}
	occ.Release()
	a := <-ch
	if a.err != nil {
		t.Fatalf("waiter after regular release: %v", a.err)
	}
	a.lease.Release()
}

// Gate (g): a background acquire on a full preempt-enabled target never
// opens a preempt — only interactive demand does (§4.2.1).
func TestPreemptBackgroundAcquireNeverTriggers(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), preemptPolicy(1))
	bg, bgCtx, err := d.Acquire(context.Background(), backgroundReq())
	if err != nil {
		t.Fatalf("background occupier: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(ctx, d, "bg2", backgroundReq(), ch)
	waitFor(t, "second background queued", func() bool { return waitingBackground(d) == 1 })
	time.Sleep(30 * time.Millisecond)
	if bgCtx.Err() != nil {
		t.Fatalf("background acquire opened a preempt against its own class")
	}
	if got := preemptStats(t, d); got.PreemptsTotal != 0 {
		t.Fatalf("preempt counter after background-only pressure: %+v", got)
	}
	bg.Release()
	a := <-ch
	if a.err != nil {
		t.Fatalf("second background: %v", a.err)
	}
	a.lease.Release()
}

// Gate (e), PB2/PB6 + E-P2: a victim whose wire call never returns is
// force-released after preempt_release_timeout (third legitimate release
// path): ERROR + divergence counter, the waiter is admitted, and the
// LAGGING real Release is a no-op (idempotence). Timing probe both ways:
// before the fence the slot stays held, after it the handoff runs.
func TestPreemptWatchdogForceRelease(t *testing.T) {
	s := DefaultSettings()
	s.PreemptReleaseTimeout = 60 * time.Millisecond
	d, h := newTestDispatcher(t, s, preemptPolicy(1))
	victim, victimCtx, err := d.Acquire(context.Background(), backgroundReq())
	if err != nil {
		t.Fatalf("victim: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 1)
	startWaiter(withPrincipal(ctx, principal("a")), d, "ia", interactiveReq(), ch)
	waitFor(t, "victim canceled", func() bool { return victimCtx.Err() != nil })

	// The victim HOLDS (no Release). Before the fence: no force-release.
	select {
	case a := <-ch:
		t.Fatalf("interactive admitted before the watchdog fence: %+v", a)
	case <-time.After(20 * time.Millisecond):
	}
	// After the fence: force-release admits the waiter.
	a := <-ch
	if a.err != nil {
		t.Fatalf("waiter after force-release: %v", a.err)
	}
	ps := preemptStats(t, d)
	if ps.ForcedReleasesTotal != 1 {
		t.Fatalf("forced_releases_total: got %d want 1", ps.ForcedReleasesTotal)
	}
	if ps.ReleaseMsLast != 0 {
		t.Fatalf("a forced release must leave NO release_ms sample (it would record the timeout): %+v", ps)
	}
	if !h.contains("force-released after preempt_release_timeout") {
		t.Fatalf("expected the watchdog ERROR log")
	}
	// The lagging real release is a no-op — the waiter's slot stays intact.
	victim.Release()
	if ts := target(t, d); ts.Held != 1 {
		t.Fatalf("lagging victim release must not free the waiter's slot: %+v", ts)
	}
	if got := preemptStats(t, d); got.ForcedReleasesTotal != 1 || got.PreemptsTotal != 1 {
		t.Fatalf("counters moved on the lagging no-op release: %+v", got)
	}
	a.lease.Release()
}

// Gate (k), PB10: the preempt TRIGGER cancels its own ctx after the victim
// cancel is spoken — the cancel is not revoked, the freed slot returns to
// regular admission (empty waitQ ⇒ background admits again), no slot or
// wait-queue leak; wastedMs keeps counting the honest loss.
func TestPreemptTriggerCanceledAfterVictimCancel(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), preemptPolicy(1))
	victim, victimCtx, err := d.Acquire(context.Background(), backgroundReq())
	if err != nil {
		t.Fatalf("victim: %v", err)
	}
	iaCtx, iaCancel := context.WithCancel(context.Background())
	ch := make(chan admission, 1)
	startWaiter(withPrincipal(iaCtx, principal("a")), d, "ia", interactiveReq(), ch)
	waitFor(t, "victim canceled", func() bool { return victimCtx.Err() != nil })
	iaCancel() // the beneficiary walks away AFTER the cancel is spoken
	a := <-ch
	if !errors.Is(a.err, context.Canceled) {
		t.Fatalf("trigger cancel: got %v want context.Canceled", a.err)
	}
	victim.Release()
	ts := target(t, d)
	if ts.Held != 0 || ts.Interactive.Waiting != 0 || ts.Inflight != 0 {
		t.Fatalf("slot or wait-queue leak after canceled trigger: %+v", ts)
	}
	if got := preemptStats(t, d); got.PreemptsTotal != 1 || got.WastedMsTotal < 0 {
		t.Fatalf("wasted work must stay honestly counted: %+v", got)
	}
	// Empty waitQ ⇒ the origin is back in regular admission (background too).
	l, _, err := d.Acquire(context.Background(), backgroundReq())
	if err != nil {
		t.Fatalf("regular background admission after the canceled trigger: %v", err)
	}
	l.Release()
}

// --- derivation gates (i)/(j): authority + external guard.

// Gate (j), §3.1 no. 2 / E-P5: preempt_background=true on an external
// provider class is ignored + WARN — the slots cap (admission control)
// SURVIVES; only the cancel authority is refused. generic (openai
// pass-through) and empty classes fail closed the same way.
func TestDeriveExternalProviderPreemptIgnored(t *testing.T) {
	pol, h := derive(t, []BackendRow{
		{Name: "openrouter", Scope: GlobalScope, BaseURL: "https://openrouter.ai/api/v1",
			ProviderClass: "openrouter",
			Limits:        map[string]any{"slots": float64(1), "preempt_background": true}},
	})
	tp := pol.Targets["https://openrouter.ai:443"]
	if tp.Slots != 1 {
		t.Fatalf("admission control must survive the external guard: %+v", tp)
	}
	if tp.PreemptBackground {
		t.Fatalf("preempt_background armed on an external provider class (PB billing damage)")
	}
	if !h.contains("non-local provider class ignored") {
		t.Fatalf("expected the external-guard WARN")
	}
	for _, class := range []string{"generic", ""} {
		pol, h := derive(t, []BackendRow{
			{Name: "amb", Scope: GlobalScope, BaseURL: "http://amb:9000",
				ProviderClass: class,
				Limits:        map[string]any{"slots": float64(1), "preempt_background": true}},
		})
		if pol.Targets["http://amb:9000"].PreemptBackground {
			t.Fatalf("class %q must fail closed to admission control", class)
		}
		if !h.contains("non-local provider class ignored") {
			t.Fatalf("expected the external-guard WARN for class %q", class)
		}
	}
	// preempt_background=false never trips the guard (no WARN noise).
	_, h2 := derive(t, []BackendRow{
		{Name: "quiet", Scope: GlobalScope, BaseURL: "https://openrouter.ai/api/v1",
			ProviderClass: "openrouter",
			Limits:        map[string]any{"slots": float64(1), "preempt_background": false}},
	})
	if h2.contains("non-local provider class") {
		t.Fatalf("false must not WARN: %v", h2.msgs)
	}
}

// Gate (i), K2 authority: on a shared origin only the _global row arms
// preempt_background (the tenant-row case is pinned in policy_test.go); a
// tenant-EXCLUSIVE origin stays free — the tenant carries benefit and damage
// itself. Both only on a local provider class.
func TestDerivePreemptAuthority(t *testing.T) {
	pol, _ := derive(t, []BackendRow{
		{Name: "gpu", Scope: GlobalScope, BaseURL: "http://gpu:8089/v1",
			ProviderClass: providerClassLlamaCpp,
			Limits:        map[string]any{"slots": float64(1), "preempt_background": true}},
	})
	if !pol.Targets["http://gpu:8089"].PreemptBackground {
		t.Fatalf("_global llamacpp row must arm preempt_background")
	}
	pol, h := derive(t, []BackendRow{
		{Name: "own", Scope: "tenant-x", BaseURL: "http://own:9000",
			ProviderClass: providerClassLlamaCpp,
			Limits:        map[string]any{"slots": float64(1), "preempt_background": true}},
	})
	if !pol.Targets["http://own:9000"].PreemptBackground {
		t.Fatalf("tenant-exclusive origin must keep its preempt authority")
	}
	if h.contains("non-authoritative") {
		t.Fatalf("unexpected K2 WARN on a tenant-exclusive origin")
	}
}

// The OR merge over several authoritative rows of one origin (MW1 doctrine):
// one local true arms the origin even next to a false sibling.
func TestDerivePreemptOrMerge(t *testing.T) {
	pol, _ := derive(t, []BackendRow{
		{Name: "a", Scope: GlobalScope, BaseURL: "http://gpu:8089",
			ProviderClass: providerClassLlamaCpp,
			Limits:        map[string]any{"slots": float64(1), "preempt_background": false}},
		{Name: "b", Scope: GlobalScope, BaseURL: "http://GPU:8089/v1",
			ProviderClass: providerClassLlamaCpp,
			Limits:        map[string]any{"preempt_background": true}},
	})
	if !pol.Targets["http://gpu:8089"].PreemptBackground {
		t.Fatalf("OR merge over authoritative rows drifted")
	}
}

// Integrator pin (defense-in-depth edge of pickVictimsLocked): when the
// youngest background lease is ALREADY in eviction and a further interactive
// waiter raises need again, the picker must select the next-youngest lease —
// re-picking the evicting one would be a no-op cancel and leave the new
// waiter hanging until the watchdog. Caught live: removing the `evicting`
// filter keeps every other preempt gate green; only this probe turns red.
func TestPreemptSkipsVictimAlreadyInEviction(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), preemptPolicy(2))
	_, olderCtx, err := d.Acquire(context.Background(), backgroundReq())
	if err != nil {
		t.Fatalf("older background: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	_, youngerCtx, err := d.Acquire(context.Background(), backgroundReq())
	if err != nil {
		t.Fatalf("younger background: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 2)
	startWaiter(withPrincipal(ctx, principal("a")), d, "ia", interactiveReq(), ch)
	waitFor(t, "youngest victim canceled", func() bool { return youngerCtx.Err() != nil })
	// The youngest victim does NOT release (slow teardown) — the second
	// waiter's demand must evict the OLDER lease, not re-pick the first.
	startWaiter(withPrincipal(ctx, principal("b")), d, "ib", interactiveReq(), ch)
	waitFor(t, "older victim canceled for the second waiter", func() bool { return olderCtx.Err() != nil })
	if got := preemptStats(t, d); got.PreemptsTotal != 2 {
		t.Fatalf("cancels: got %d want 2 (one per victim, no re-pick)", got.PreemptsTotal)
	}
}
