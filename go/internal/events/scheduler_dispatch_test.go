package events

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/store"
)

// A5-W0 (MW15, design/05 §4.1): the five signal-driven arms (guard, digest,
// overview, dream, audit) read the dispatcher's InteractiveDemand() — via the
// nil-safe interactiveDemand() — instead of the removed scheduler demand mirror.
// QueryStart/QueryEnd + the demandDones pairing shim are gone; the herald lives
// entirely in the dispatcher (proven idempotent by dispatch TestHeraldDone*),
// and the WithScheduler mounts inject the dispatcher directly. These probes
// pin: (a) the read is nil-safe (unwired scheduler = neutral), (b) it tracks
// the herald, and (c) each arm still defers exactly as before against the new
// signal (P-Signal/P-GPU parity — yield SEMANTICS unchanged in this wave).
//
// Red probe (2026-07-06, documented per wave contract): with the guard's
// demand branch hard-wired to a constant 0 (`if 0 > 0`), the demand read is
// dead and TestRunGuard_DefersOnDemand failed with "guard ran under sustained
// demand" — the arm stamped lastGuardNs past a defer that never fired. A second
// mutation totlegged the signal at the source (interactiveDemand returning a
// constant 0): that probe plus the audit/dream/overview probes went red
// together, proving they observe the live signal, not a fixture.

// schedulerWithDemand builds a zero-value scheduler wired to a live dispatcher
// (default settings, empty policy — pass-through, the MW2 herald state) and
// raises interactive demand to n, returning the scheduler and a release that
// lowers demand back to zero (each done is the dispatcher's idempotent herald
// closure).
func schedulerWithDemand(t *testing.T, n int) (*Scheduler, func()) {
	t.Helper()
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	s := &Scheduler{}
	s.SetDispatcher(d)
	dones := make([]func(), 0, n)
	for i := 0; i < n; i++ {
		dones = append(dones, d.InteractiveArrived())
	}
	return s, func() {
		for _, done := range dones {
			done()
		}
	}
}

// schedulerEnforcingWithDemand builds a scheduler wired to a dispatcher whose
// derived policy fully covers the local background-reachable targets (a capped
// GPU chat origin carrying the background roles + a capped embed origin — the
// live-shaped fixture of dispatch.enforcingRows), so Enforcing() is true, and
// raises interactive demand to n. Release lowers demand back to zero. Used by
// the P-GPU probes: unlike schedulerWithDemand (empty policy ⇒ !Enforcing ⇒
// P-Fallback), this one exercises the poll-less path.
func schedulerEnforcingWithDemand(t *testing.T, n int) (*Scheduler, func()) {
	t.Helper()
	logger := slog.Default()
	d := dispatch.New(logger, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	d.UpdatePolicy(dispatch.DerivePolicy([]dispatch.BackendRow{
		{Name: "gpu-chat", Scope: dispatch.GlobalScope, BaseURL: "http://gpu:8089/v1",
			Roles:  []string{"chat", "dream", "classify", "digest", "synthesis", "translate"},
			Limits: map[string]any{"slots": float64(1)}},
		{Name: "embed", Scope: dispatch.GlobalScope, BaseURL: "http://embed:8081",
			Roles:  []string{"embed", "dream-embed"},
			Limits: map[string]any{"slots": float64(4)}},
	}, logger))
	s := &Scheduler{}
	s.SetDispatcher(d)
	if !s.enforcing() {
		t.Fatal("fixture policy did not make the dispatcher Enforcing")
	}
	dones := make([]func(), 0, n)
	for i := 0; i < n; i++ {
		dones = append(dones, d.InteractiveArrived())
	}
	return s, func() {
		for _, done := range dones {
			done()
		}
	}
}

// TestEnforcing_NilSafe pins the unwired-scheduler neutrality of the yield
// gate: no dispatcher ⇒ enforcing() false ⇒ the legacy yield fallback stays
// active (P-Fallback), never the removed poll-less path (SetDispatcher
// happens-before Run, so production always has a live dispatcher).
func TestEnforcing_NilSafe(t *testing.T) {
	s := &Scheduler{}
	if s.enforcing() {
		t.Fatal("nil-dispatcher enforcing() = true, want false (fallback must stay active)")
	}
}

// TestEnforcing_ReflectsDispatcher pins that the arm's yield gate tracks the
// dispatcher: full local coverage ⇒ true; an emergency stop (enabled=false)
// drops it back to false even under that coverage — the fallback returns within
// a reload, no arm involvement (design/05 §4.2).
func TestEnforcing_ReflectsDispatcher(t *testing.T) {
	s, release := schedulerEnforcingWithDemand(t, 0)
	defer release()
	if !s.enforcing() {
		t.Fatal("enforcing() = false under full local coverage")
	}
	off := dispatch.DefaultSettings()
	off.Enabled = false
	s.dispatcher.UpdateSettings(off)
	if s.enforcing() {
		t.Fatal("enforcing() = true after emergency stop (enabled=false)")
	}
}

// TestAuditTenantScope_ProceedsUnderEnforcing is the P-GPU gate for audit
// (A5-W2/MW16): with the dispatcher Enforcing and demand > 0 the arm does NOT
// self-poll — it proceeds past the removed vor-start yield so each classify
// call waits AT THE TARGET in its background lease. Observable via this file's
// idiom: the zero-value scheduler has a nil cfg, so proceeding past the yield
// reaches s.cfg.SnapshotForTenant and panics, whereas still sitting in the
// yield would instead abort cleanly on the ctx timeout — panic ⟺ proceeded.
// The "waits at target / waitQ depth 1" property belongs to the background
// lease (dispatch Acquire/Herald tests, MW3); the MW16 delta is precisely that
// the arm stops polling under Enforcing.
func TestAuditTenantScope_ProceedsUnderEnforcing(t *testing.T) {
	s, release := schedulerEnforcingWithDemand(t, 1)
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	proceeded := make(chan bool, 1)
	go func() {
		defer func() {
			// nil cfg deref past the yield ⇒ the arm proceeded (did not poll).
			proceeded <- recover() != nil
		}()
		s.auditTenantScope(ctx, backgroundTenant{scope: store.GlobalScope}, false, 10)
		proceeded <- false // returned via the yield/abort path — did NOT proceed
	}()

	select {
	case p := <-proceeded:
		if !p {
			t.Fatal("audit yielded under Enforcing instead of proceeding to the target (vor-start poll not removed)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("audit stuck under Enforcing")
	}
}

// TestInteractiveDemand_NilSafe pins the unwired-scheduler neutrality: no
// dispatcher ⇒ demand 0 ⇒ no arm defers, identical to the removed zero-value
// query-count mirror (SetDispatcher happens-before Run, so production always
// has a live dispatcher; tests and pre-wire boot must stay neutral).
func TestInteractiveDemand_NilSafe(t *testing.T) {
	s := &Scheduler{}
	if got := s.interactiveDemand(); got != 0 {
		t.Fatalf("nil-dispatcher interactiveDemand() = %d, want 0", got)
	}
}

// TestInteractiveDemand_ReflectsHerald pins that the arms' read tracks the
// dispatcher herald 0 → n → 0 (the successor of the old demand mirror).
func TestInteractiveDemand_ReflectsHerald(t *testing.T) {
	s, release := schedulerWithDemand(t, 2)
	if got := s.interactiveDemand(); got != 2 {
		t.Fatalf("interactiveDemand() = %d, want 2", got)
	}
	release()
	if got := s.interactiveDemand(); got != 0 {
		t.Fatalf("after release interactiveDemand() = %d, want 0", got)
	}
}

// TestRunGuard_DefersOnDemand is the P-Signal negative gate for guard: under
// demand the arm returns BEFORE the lastGuardNs run-stamp (return-defer, Ist);
// with zero demand it runs past the defer (stamp advances). lastGuardNs is the
// observable because it is stamped ONLY past the demand check (scheduler.go,
// MW12/§4.5). The zero-demand leg is the built-in red-probe: if the guard
// stopped reading the signal, the demand leg would stamp too and this fails.
func TestRunGuard_DefersOnDemand(t *testing.T) {
	s, release := schedulerWithDemand(t, 1)

	s.runGuard(context.Background())
	if g, _, _ := s.LastArmRuns(); !g.IsZero() {
		t.Fatalf("guard ran under sustained demand (lastGuardNs stamped) — return-defer broken")
	}

	release()
	s.runGuard(context.Background()) // nil block-type registry ⇒ loud skip past the stamp
	if g, _, _ := s.LastArmRuns(); g.IsZero() {
		t.Fatalf("guard did not run at zero demand (lastGuardNs unstamped) — probe blind")
	}
}

// TestRunDigest_DefersOnDemand is the P-Signal negative gate for digest,
// symmetric to guard (lastDigestNs is the past-defer run-stamp). The empty
// tenant set makes the zero-demand leg complete cleanly past the stamp.
func TestRunDigest_DefersOnDemand(t *testing.T) {
	s, release := schedulerWithDemand(t, 1)
	s.backgroundTenantsFn = func(context.Context) []backgroundTenant { return nil }

	s.runDigest(context.Background())
	if _, dg, _ := s.LastArmRuns(); !dg.IsZero() {
		t.Fatalf("digest ran under sustained demand (lastDigestNs stamped) — return-defer broken")
	}

	release()
	s.runDigest(context.Background())
	if _, dg, _ := s.LastArmRuns(); dg.IsZero() {
		t.Fatalf("digest did not run at zero demand (lastDigestNs unstamped) — probe blind")
	}
}

// TestRunDreamLoop_YieldsOnDemand is the P-GPU negative gate for dream in this
// wave (yield SEMANTICS unchanged — 2 s wait-loop, entfernung is A5-W3/MW17):
// under sustained demand the loop stays in the yield branch and exits on ctx,
// never reaching the nil-cfg pick/backfill body (which would panic if the yield
// were skipped). DreamModeOn is the zero value, so no SetDreamMode is needed.
func TestRunDreamLoop_YieldsOnDemand(t *testing.T) {
	s, _ := schedulerWithDemand(t, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runDreamLoop(ctx)
	}()
	select {
	case <-done: // yielded through the demand branch, exited cleanly on ctx
	case <-time.After(5 * time.Second):
		t.Fatal("dream loop did not yield/exit under sustained demand")
	}
}

// TestAuditTenantScope_DefersOnDemand is the P-Fallback gate for audit after
// the yield removal (A5-W2/MW16): schedulerWithDemand wires an EMPTY-policy
// dispatcher ⇒ !Enforcing ⇒ the fail-open fallback 2 s wait-loop runs, so under
// demand the scope loop stays in the yield branch and aborts on ctx cancel
// without reaching the nil-cfg SnapshotForTenant body — byte-identical to the
// pre-removal Ist. A dead demand read (or a wrongly-Enforcing fallback) would
// fall through to that body and panic instead of returning abort. The
// Enforcing (poll-less) leg is TestAuditTenantScope_ProceedsUnderEnforcing.
func TestAuditTenantScope_DefersOnDemand(t *testing.T) {
	s, _ := schedulerWithDemand(t, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if abort := s.auditTenantScope(ctx, backgroundTenant{scope: store.GlobalScope}, false, 10); !abort {
		t.Fatal("audit did not abort under sustained demand + ctx cancel — yield branch not taken")
	}
}
