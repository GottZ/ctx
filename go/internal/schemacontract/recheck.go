package schemacontract

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgxpool"
)

// errCheckInFlight is returned only in the narrow window where a caller
// loses the CAS on the very first check this process has ever run (no
// prior LatestReport to fall back to) — practically unreachable in
// production (boot's first call never contends), reachable only by a
// pathological test/refresh race.
var errCheckInFlight = errors.New("schemacontract: check already in flight, no prior report to serve")

// latestReport is the process-wide report holder (design/03 §4.1/§4.5):
// written by RunCheckSingleFlight (boot's initial check, the periodic
// re-check ticker, and — from W03-4 — the ?refresh=1 admin endpoint), read
// by every consumer (health, /api/contract, the CLI, test.sh's T07
// successor) without re-introspecting. A nil pointer means "no check has
// run yet in this process" — LatestReport's ok=false, never a zero Report
// masquerading as a real (and misleadingly "ok") result.
var latestReport atomic.Pointer[Report]

// StoreReport publishes r as the current process-wide report. Exported so a
// caller with its own Report (a test fixture, a future call site outside
// this package's own RunCheckSingleFlight) can seed/override the holder;
// RunCheckSingleFlight is the only production caller.
func StoreReport(r Report) {
	latestReport.Store(&r)
}

// LatestReport returns the most recently stored Report. ok=false before the
// first check has run in this process.
func LatestReport() (Report, bool) {
	p := latestReport.Load()
	if p == nil {
		return Report{}, false
	}
	return *p, true
}

// checkRunning is the CAS single-flight guard (design/03 §4.5/§7 W03-3 Gate
// 3, the status.go:337-340 pattern named in the wave brief): a losing
// caller performs NO work at all — it is not queued, not blocked, just a
// no-op that returns whatever is currently stored. This mirrors
// refreshCheapAsync/scanQueueAsync exactly (fire-and-forget on the winner,
// immediate return for everyone else) rather than a blocking join, because
// none of this wave's actual callers ever contend with themselves: boot's
// one-time initial check has no concurrent caller, and the periodic
// re-check ticker is a single goroutine that never calls itself
// concurrently. The guard exists for the FUTURE ?refresh=1 admin endpoint
// (W03-4, "geteilt nutzbar" per design/03 §4.5) and is proven here by Gate
// 3's N-concurrent-callers test.
var checkRunning atomic.Bool

// checkRunCount counts actual Check() executions RunCheckSingleFlight has
// performed — incremented only inside the CAS-won branch, never by a
// losing caller. Package-internal; the W03-3 Gate 3 integration test (same
// package) reads it directly to prove a concurrent burst performs exactly
// one introspection, deterministically (the CAS makes concurrent execution
// of the winning branch structurally impossible, not just improbable).
var checkRunCount atomic.Int64

// RunCheckSingleFlight runs one full check cycle (Check + the §4.4 mode
// resolution + the mode_source_db_off drift injection) and publishes the
// result via StoreReport — UNLESS a check is already in flight, in which
// case it is a no-op that returns the current stored report (or, before
// any check has ever run, a zero unchecked report with a descriptive
// error). It is shared by cmd/ctxd's schemaContractBoot (the boot-time
// first check), the periodic re-check ticker, and — from W03-4 — the
// ?refresh=1 admin endpoint. It NEVER decides enforce/warn boot behavior
// (design/03 §4.5's last bullet: "Re-Check enforced nie") — that decision
// stays exclusively in schemaContractBoot, which is the only caller
// entitled to call os.Exit.
func RunCheckSingleFlight(ctx context.Context, pool *pgxpool.Pool) (Report, error) {
	if !checkRunning.CompareAndSwap(false, true) {
		if r, ok := LatestReport(); ok {
			return r, nil
		}
		return Report{Status: StatusUnchecked}, errCheckInFlight
	}
	defer checkRunning.Store(false)

	checkRunCount.Add(1)
	report, err := Check(ctx, pool)

	// Mode resolution + the dbOffFinding drift apply on EVERY run (boot,
	// re-check, future refresh) regardless of whether Check itself
	// succeeded — Mode/ModeSource is orthogonal, "what is configured right
	// now" information that stays meaningful even on an unchecked report
	// (design/03 §4.4: an enforce-configured instance whose checker itself
	// cannot run is exactly the case an operator most needs to see).
	mode, source, dbOff := gatherModeResolution(ctx, pool)
	report.Mode = mode
	report.ModeSource = source
	if dbOff {
		report.Drifts = append(report.Drifts, Drift{
			Class:    ClassModeSourceDBOff,
			Severity: SeverityParam,
			Object:   "contract.mode",
			Detail:   "contract.mode=off written to the DB is not honored — resolved as warn (design/03 §4.4)",
		})
		sortDrifts(report.Drifts)
		if report.Status == StatusOK {
			report.Status = StatusDrift
		}
	}

	// StoreReport ALWAYS runs, success or failure — the Fehler-Semantik
	// invariant (design/03 §4.5): "nie den alten ok-Stand stehen lassen".
	// report.Status is already StatusUnchecked on any Check failure (set
	// by Check itself before returning), so overwriting unconditionally is
	// exactly the fail-closed behavior Gate 4 tests.
	StoreReport(report)
	return report, err
}
