//go:build integration

// Gate A02-5, BA10 half (design/02 §7.2, last probe): the distiller must not be
// able to stall the ticker arms. It is a SEPARATE file because it is the only
// probe that runs the whole scheduler and therefore pays the real 60-second
// guard interval and digest debounce — the journal gates next to it run in
// milliseconds and should not inherit that cost.
//
// Run with:
//
//	go test -tags=integration ./internal/events/ -run TestDistillBlockingArm -count=1 -v -timeout 10m
package events

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/distillsource"
	"github.com/GottZ/ctx/internal/testdb"
)

// blockingDistillSource parks in Sessions until it is released. It is the
// stand-in for what this arm becomes in A02-8: minutes of sequential inference
// inside one tick.
type blockingDistillSource struct {
	fakeDistillSource
	release <-chan struct{}
	entered chan struct{}
}

func (b *blockingDistillSource) Sessions(ctx context.Context) ([]distillsource.Ref, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return nil, nil
}

// distillDerivedFor is the watermark derivation, spelled out here so this file
// stands on its own: the boot-path sweep assertion must not read the production
// helper it is trying to catch.
func distillDerivedFor(t *testing.T, pool *pgxpool.Pool, key string) int64 {
	t.Helper()
	var wm int64
	if err := pool.QueryRow(context.Background(), `
		SELECT COALESCE(max(watermark_to), 0)
		  FROM distill_run
		 WHERE source_key = $1 AND outcome <> 'running'`, key).Scan(&wm); err != nil {
		t.Fatalf("derive watermark: %v", err)
	}
	return wm
}

// TestDistillBlockingArm is the BA10 gate plus the startup-sweep WIRING probe
// (review #2): a distiller tick that parks for the whole guard interval must
// leave the guard and digest stamps moving, and the arm's boot must have swept
// the orphaned run row before any of that.
//
// RED, recorded in the wave report and reproducible by patching Run: with the
// tick driven from a case in Run's central select instead of from its own
// goroutine, both stamps stay at the zero time for the entire run — a ticker arm
// holds the loop that also drives guard and digest.
func TestDistillBlockingArm(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool, dsn := testdb.SetupTestDBWithDSN(t)

	release := make(chan struct{})
	defer close(release)
	src := &blockingDistillSource{release: release, entered: make(chan struct{}, 1)}

	cfg := &config.Config{}
	cfg.Distill.Enabled = true
	cfg.Distill.CtxEnabled = true
	cfg.Distill.CtxSourceLabel = "ctx-checkpoint"
	cfg.Distill.CheckpointCategory = "compaction-checkpoints"
	cfg.Distill.MaxSessionsPerRun = 4
	cfg.Distill.RowsPerRead = 400
	cfg.Distill.Interval = time.Second // park almost immediately
	cfg.Scheduler.HomeScope = "private"

	// A REAL backend pool over the test database: Run starts the NOTIFY
	// listener, and its settings-write handler reloads the pool on the boot
	// backlog — a pool without a database dereferences nil there.
	s := NewScheduler(pool, config.NewStore(cfg), backends.NewPool(pool, nil), StartupConfig{DSN: dsn})
	s.SetBlocktypeRegistry(blocktype.NewRegistry())
	s.distillSource = func(*config.Config, string) (distillsource.Source, error) { return src, nil }

	// The startup sweep, probed through the BOOT PATH rather than through a
	// direct call (review #2). §4.5.5 makes the sweep a correctness condition of
	// the ARM START — "erst danach darf der Arm laufen" — and a subtest that
	// calls distillStartupSweep itself cannot see the call disappear from
	// runDistiller. This harness already runs the whole Run(), so the orphan
	// costs nothing here: a running row with watermark_to = 500 goes in BEFORE
	// the scheduler starts, and the derivation afterwards is the assertion.
	const sweepKey = "ctx-checkpoint:private:20260801_090000_orphan"
	if _, err := pool.Exec(ctx, `
		INSERT INTO distill_run (source_key, root_session_id, outcome, watermark_from, watermark_to)
		VALUES ($1, '20260801_090000_orphan', 'running', 0, 500)`, sweepKey); err != nil {
		t.Fatalf("insert orphan run row: %v", err)
	}
	if got := distillDerivedFor(t, pool, sweepKey); got != 0 {
		t.Fatalf("RED precondition broken: derivation = %d before the boot, want 0", got)
	}

	runCtx, stop := context.WithCancel(ctx)
	defer func() { stop(); s.Wait() }()
	go s.Run(runCtx)

	// The sweep is the first thing runDistiller does, so it lands long before the
	// first tick interval.
	sweepDeadline := time.Now().Add(30 * time.Second)
	var swept int64
	for time.Now().Before(sweepDeadline) {
		if swept = distillDerivedFor(t, pool, sweepKey); swept == 500 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("context expired while waiting for the startup sweep")
		case <-time.After(200 * time.Millisecond):
		}
	}
	if swept != 500 {
		t.Fatalf("derivation = %d after the arm booted, want 500 — the startup sweep is not wired into runDistiller", swept)
	}
	var outcome, errClass string
	if err := pool.QueryRow(ctx, `
		SELECT outcome, COALESCE(error, '') FROM distill_run WHERE source_key = $1`,
		sweepKey).Scan(&outcome, &errClass); err != nil {
		t.Fatalf("read swept row: %v", err)
	}
	if outcome != "killed" || errClass != "daemon_restart" {
		t.Fatalf("swept row = %s/%s, want killed/daemon_restart", outcome, errClass)
	}

	// Arm both ticker arms: guard and digest need a pending write, and the
	// digest debounce measures from it.
	s.NotifyWrite()

	select {
	case <-src.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the distiller never reached its blocking call — the probe would prove nothing")
	}

	// The guard interval and the digest debounce are both 60 s, so one wait
	// covers both stamps. The margin absorbs a slow container.
	deadline := time.Now().Add(110 * time.Second)
	var guard, digest time.Time
	for time.Now().Before(deadline) {
		guard, digest, _ = s.LastArmRuns()
		if !guard.IsZero() && !digest.IsZero() {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("context expired while waiting for the ticker arms")
		case <-time.After(time.Second):
		}
	}

	if guard.IsZero() || digest.IsZero() {
		t.Fatalf("ticker arms stalled behind the parked distiller: guard=%v digest=%v", guard, digest)
	}
	t.Logf("guard stamped %s, digest stamped %s while the distiller was parked", guard, digest)
}
