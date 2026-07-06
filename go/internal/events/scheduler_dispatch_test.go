package events

import (
	"context"
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

// TestAuditTenantScope_DefersOnDemand is the P-GPU negative gate for audit
// (yield SEMANTICS unchanged — 2 s wait-loop, entfernung is A5-W2/MW16): under
// demand the scope loop stays in the yield branch and aborts on ctx cancel
// without reaching the nil-cfg SnapshotForTenant body. A dead demand read would
// fall through to that body and panic instead of returning abort.
func TestAuditTenantScope_DefersOnDemand(t *testing.T) {
	s, _ := schedulerWithDemand(t, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if abort := s.auditTenantScope(ctx, backgroundTenant{scope: store.GlobalScope}, false, 10); !abort {
		t.Fatal("audit did not abort under sustained demand + ctx cancel — yield branch not taken")
	}
}
