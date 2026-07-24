//go:build integration

package schemacontract

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/testdb"
)

// TestRunCheckSingleFlight_ModeSourceDBOff is design/03 §7 W03-3 Gate 2's
// integration half: a context_settings row contract.mode='off' (env unset)
// must NOT disable the check — RunCheckSingleFlight's Report carries
// mode=warn/source=db AND a visible mode_source_db_off finding (design/03
// §4.4), proving the wiring end-to-end (ResolveMode's unit proof in
// mode_test.go only covers the pure function).
func TestRunCheckSingleFlight_ModeSourceDBOff(t *testing.T) {
	t.Setenv(EnvContractMode, "") // ensure no env override leaks in from the test environment
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`INSERT INTO context_settings (scope, key, value) VALUES ('_global', 'contract.mode', '"off"')`,
	); err != nil {
		t.Fatalf("seeding contract.mode=off: %v", err)
	}

	report, err := RunCheckSingleFlight(ctx, pool)
	if err != nil {
		t.Fatalf("RunCheckSingleFlight: %v", err)
	}
	if report.Mode != ModeWarn {
		t.Errorf("Mode = %s, want %s (off from DB must not be honored)", report.Mode, ModeWarn)
	}
	if report.ModeSource != SourceDB {
		t.Errorf("ModeSource = %s, want %s", report.ModeSource, SourceDB)
	}
	found := findDrift(report.Drifts, "contract.mode")
	if found == nil {
		t.Fatalf("no mode_source_db_off finding — drifts: %+v", report.Drifts)
	}
	if found.Class != ClassModeSourceDBOff || found.Severity != SeverityParam {
		t.Errorf("got class=%s severity=%s, want %s/%s", found.Class, found.Severity, ClassModeSourceDBOff, SeverityParam)
	}
	if report.Status != StatusDrift {
		t.Errorf("Status = %s, want drift (the db-off finding alone must surface as drift)", report.Status)
	}

	// Also visible through the process-wide holder — RunCheckSingleFlight
	// must have published it, not just returned it.
	stored, ok := LatestReport()
	if !ok {
		t.Fatal("LatestReport: no report stored")
	}
	if stored.ModeSource != SourceDB || findDrift(stored.Drifts, "contract.mode") == nil {
		t.Errorf("stored report does not carry the same mode/finding: %+v", stored)
	}
}

// TestRunCheckSingleFlight_EnvOverridesDBEnforce is the integration cousin
// of TestResolveMode_EnvAlwaysWins's most adversarial cell: a DB row says
// enforce, but CTX_CONTRACT_MODE=off is set — the report must resolve to
// off/env, end-to-end through the real context_settings read.
func TestRunCheckSingleFlight_EnvOverridesDBEnforce(t *testing.T) {
	t.Setenv(EnvContractMode, ModeOff)
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`INSERT INTO context_settings (scope, key, value) VALUES ('_global', 'contract.mode', '"enforce"')`,
	); err != nil {
		t.Fatalf("seeding contract.mode=enforce: %v", err)
	}

	report, err := RunCheckSingleFlight(ctx, pool)
	if err != nil {
		t.Fatalf("RunCheckSingleFlight: %v", err)
	}
	if report.Mode != ModeOff {
		t.Errorf("Mode = %s, want %s — rot unter der alten DB>env-Annahme (db=enforce würde hier gewinnen)", report.Mode, ModeOff)
	}
	if report.ModeSource != SourceEnv {
		t.Errorf("ModeSource = %s, want %s", report.ModeSource, SourceEnv)
	}
	if findDrift(report.Drifts, "contract.mode") != nil {
		t.Error("mode_source_db_off finding present, want none — env decided, the db=enforce value was never consulted as an off-attempt")
	}
}

// TestRunCheckSingleFlight_ConcurrentBurst_ExactlyOneIntrospection is
// design/03 §7 W03-3 Gate 3's single-flight probe: N concurrent
// RunCheckSingleFlight calls must perform exactly ONE Check execution — the
// CAS guard (recheck.go) makes this a structural guarantee (a losing
// caller runs no code path that could touch checkRunCount at all), not a
// timing-dependent probability, so the assertion is exact rather than
// "at least 1, at most N".
func TestRunCheckSingleFlight_ConcurrentBurst_ExactlyOneIntrospection(t *testing.T) {
	t.Setenv(EnvContractMode, "")
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	before := checkRunCount.Load()

	const n = 12
	var wg sync.WaitGroup
	var errs int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := RunCheckSingleFlight(ctx, pool); err != nil {
				atomic.AddInt32(&errs, 1)
			}
		}()
	}
	wg.Wait()

	// A losing caller with no prior report returns errCheckInFlight — only
	// possible on the very first burst in a fresh process before any
	// report exists. Tolerate it (it is not the thing under test), but
	// only up to n-1 (the winner must always succeed).
	if int(atomic.LoadInt32(&errs)) >= n {
		t.Fatalf("all %d calls errored — the winner itself must succeed", n)
	}

	got := checkRunCount.Load() - before
	if got != 1 {
		t.Errorf("checkRunCount delta = %d for %d concurrent callers, want exactly 1", got, n)
	}

	if _, ok := LatestReport(); !ok {
		t.Error("LatestReport: no report stored after the burst")
	}
}

// TestRunCheckSingleFlight_ErrorSemantics_NeverServesStaleOK is design/03
// §7 W03-3 Gate 4: an introspection failure (here, a closed pool) must
// replace whatever "ok" report was stored with an unchecked one — never
// leave the old ok status readable (design/03 §4.5: "nie den alten
// ok-Stand stehen lassen").
func TestRunCheckSingleFlight_ErrorSemantics_NeverServesStaleOK(t *testing.T) {
	t.Setenv(EnvContractMode, "")
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	report, err := RunCheckSingleFlight(ctx, pool)
	if err != nil {
		t.Fatalf("first (healthy) RunCheckSingleFlight: %v", err)
	}
	if report.Status != StatusOK {
		t.Fatalf("first report Status = %s, want ok — drifts: %+v", report.Status, report.Drifts)
	}
	if stored, ok := LatestReport(); !ok || stored.Status != StatusOK {
		t.Fatalf("stored report after healthy run: ok=%v status=%v, want ok=true status=ok", ok, stored.Status)
	}

	pool.Close() // breaks every subsequent query on this pool

	report2, err2 := RunCheckSingleFlight(ctx, pool)
	if err2 == nil {
		t.Fatal("RunCheckSingleFlight against a closed pool: want an error")
	}
	if report2.Status != StatusUnchecked {
		t.Errorf("Status = %s, want unchecked", report2.Status)
	}

	stored, ok := LatestReport()
	if !ok {
		t.Fatal("LatestReport: no report stored")
	}
	if stored.Status != StatusUnchecked {
		t.Errorf("stored report Status = %s, want unchecked — the old ok-Stand is still readable, Gate 4 violated", stored.Status)
	}
}

// findDrift is defined in check_integration_test.go (same package,
// integration-tagged); reused here without redeclaration.
