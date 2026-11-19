//go:build integration

// Several ctxd instances on one database and the re-embed migration
// (issues #366 and #370): one verify scan per watermark, the gate's index
// stage when the server ends the build session, the migration arm after a
// confirm it did not see, the confirm beside a runtime index build, and the
// drop of the pending index by its schema.
//
//	go test -tags=integration ./internal/events/ -run 'TestEmbedMultiInstance' -count=1 -v
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/embedmigration"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// miRouter is the router of runVerifyCycle: a fake embed backend armed with
// the embed_next key, serving to_model.
func miRouter(t *testing.T) *dream.Router {
	t.Helper()
	srv := newMigrateEmbedServer(t)
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{embedNextPoolRow("embed-a", srv.srv.URL, 100)})
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	return backfillRouter(bpool, d)
}

// miCycle runs one migration cycle like runVerifyCycle and returns its error,
// so a goroutine can run it.
func miCycle(t *testing.T, s *Scheduler, cfg *config.Config) error {
	t.Helper()
	return s.runEmbedMigrationCycle(context.Background(), miRouter(t), cfg)
}

// createPlainIndexT builds a managed runtime index with its production DDL
// without CONCURRENTLY: a finished index another session left, in one
// statement.
func createPlainIndexT(t *testing.T, pool *pgxpool.Pool, name string) {
	t.Helper()
	ddl := strings.Replace(ddlOfManaged(name), "CONCURRENTLY ", "", 1)
	if ddl == "" {
		t.Fatalf("no managed DDL for %s", name)
	}
	if _, err := pool.Exec(context.Background(), ddl); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
}

// holdVerifyRunLock takes the verify run lock of migration id at watermark
// wm on a connection of its own, as another instance's gate holds it while it
// runs. The key is spelled out here: it is the cross-instance contract.
func holdVerifyRunLock(t *testing.T, pool *pgxpool.Pool, id string, wm time.Time) func() {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("holder acquire: %v", err)
	}
	key := fmt.Sprintf("%s@%d", id, wm.UnixMicro())
	var locked bool
	if err := conn.QueryRow(ctx,
		`SELECT pg_try_advisory_lock(hashtext('ctx:embed_verify_run'), hashtext($1))`, key).Scan(&locked); err != nil || !locked {
		conn.Release()
		t.Fatalf("holder lock: locked=%t err=%v", locked, err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtext('ctx:embed_verify_run'), hashtext($1))`, key)
		conn.Release()
	}
	t.Cleanup(release)
	return release
}

// stableToastBlocks reads toastBlocks until two reads 1.5 s apart agree: the
// sessions that just exited have reported.
func stableToastBlocks(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	last := toastBlocks(t, pool)
	for i := 0; i < 20; i++ {
		time.Sleep(1500 * time.Millisecond)
		n := toastBlocks(t, pool)
		if n == last {
			return n
		}
		last = n
	}
	t.Fatalf("TOAST counters of context_blocks did not settle")
	return 0
}

func sqlstateOf(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func TestEmbedMultiInstance_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()

	// Issue #370, point 2: the server ends the session of the gate's index
	// build (pg_terminate_backend, as a database restart does). The gate
	// stores no verdict and pauses nothing, like the repair; the next cycle
	// drops the INVALID leftover and runs the gate again.
	t.Run("gate_build_session_ended_by_the_server_stores_nothing", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		seedCutoverBackend(t, pool, cutoverBackendName, flippableModelMap())
		migID := seedMigrationRow(t, pool, "verifying")
		seedCleanMigrated(t, pool, 3)
		createPlainIndexT(t, pool, migrationPendingIndexName)
		wm := *readVerifyRow(t, pool, migID).VerifyStartedAt
		s, _ := probeScheduler(t, pool)

		release := holdBuilds(t, pool)
		done := make(chan error, 1)
		go func() { done <- miCycle(t, s, verifyTestCfg()) }()
		pid := buildingPID(t, pool, verifyNextHNSWIndexName)
		if _, err := pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
			t.Fatalf("terminate: %v", err)
		}
		select {
		case <-done:
		case <-time.After(60 * time.Second):
			t.Fatalf("cycle did not end within 60s")
		}
		release()

		st := readVerifyRow(t, pool, migID)
		if st.Status != "verifying" || st.LastErr != nil || len(st.Report) != 0 || !st.VerifyStartedAt.Equal(wm) {
			t.Fatalf("after the ended session: status=%q last_error=%v report=%s, want verifying at %v without a verdict",
				st.Status, st.LastErr, st.Report, wm)
		}
		if ix := indexStateT(t, pool, verifyNextHNSWIndexName); !ix.exists || ix.valid {
			t.Fatalf("after the ended session: exists=%t valid=%t, want an INVALID leftover", ix.exists, ix.valid)
		}
		measuredCycles(t, pool, 1)
		st = readVerifyRow(t, pool, migID)
		rep := parseReport(t, st.Report)
		if rep.Result != verifyGreen || len(rep.Index.Indexes) == 0 || rep.Index.Indexes[0].Action != indexActionRebuiltInvalid {
			t.Fatalf("next cycle: verdict %s, want green with %s rebuilt_invalid", st.Report, verifyNextHNSWIndexName)
		}
	})

	// Issue #366, point 1: while another session holds the verify run lock of
	// the watermark, a cycle starts no gate — no verdict, and not one TOAST
	// block of context_blocks read; once the lock is free, the gate runs.
	t.Run("held_verify_run_lock_no_second_scan", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		seedCutoverBackend(t, pool, cutoverBackendName, flippableModelMap())
		migID := seedMigrationRow(t, pool, "verifying")
		seedCleanMigrated(t, pool, 3)
		createPlainIndexT(t, pool, migrationPendingIndexName)
		wm := *readVerifyRow(t, pool, migID).VerifyStartedAt

		release := holdVerifyRunLock(t, pool, migID, wm)
		reads := measuredCycles(t, pool, 1)
		st := readVerifyRow(t, pool, migID)
		if len(st.Report) != 0 || st.Status != "verifying" || reads != 0 {
			t.Fatalf("lock held by another session: report=%s status=%q toast blocks=%d, want no verdict and no scan",
				st.Report, st.Status, reads)
		}
		release()
		reads = measuredCycles(t, pool, 1)
		st = readVerifyRow(t, pool, migID)
		if rep := parseReport(t, st.Report); rep.Result != verifyGreen || reads == 0 {
			t.Fatalf("lock free: verdict %s, toast blocks=%d, want a green run that scanned", st.Report, reads)
		}
	})

	// Issue #366, point 1, two instances: A's gate is in its index stage
	// (a held build). B's cycle starts no gate of its own — it reads no TOAST
	// block and returns at once —, and A's verdict is the one verdict.
	t.Run("second_instance_awaits_the_verdict_of_the_first", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		seedCutoverBackend(t, pool, cutoverBackendName, flippableModelMap())
		migID := seedMigrationRow(t, pool, "verifying")
		seedCleanMigrated(t, pool, 3)
		createPlainIndexT(t, pool, migrationPendingIndexName)
		// The HNSW twin stands, so A's held build is a btree that reads no
		// TOAST: every TOAST read in the window below is a scan.
		createPlainIndexT(t, pool, verifyNextHNSWIndexName)
		sA, closeA := probeScheduler(t, pool)
		sB, closeB := probeScheduler(t, pool)

		release := holdBuilds(t, pool)
		chA := make(chan error, 1)
		go func() { chA <- miCycle(t, sA, verifyTestCfg()) }()
		buildingPID(t, pool, "idx_embedding_pending_next")
		// A backend reports its block counters when it ends a later
		// transaction or exits, not while it idles: A, held or idle, reports
		// nothing in the window below; the test pool's own sessions (the
		// seeding) exit here and report now. B's sessions exit (closeB) before
		// the window closes, so the window counts B's reads alone.
		pool.Reset()
		before := stableToastBlocks(t, pool)
		chB := make(chan error, 1)
		go func() { chB <- miCycle(t, sB, verifyTestCfg()) }()
		bFast := false
		select {
		case err := <-chB:
			bFast = true
			if err != nil {
				t.Errorf("B: %v", err)
			}
		case <-time.After(10 * time.Second):
		}
		bReads := int64(-1)
		if bFast {
			closeB()
			bReads = toastBlocks(t, pool) - before
		}
		release()
		select {
		case err := <-chA:
			if err != nil {
				t.Errorf("A: %v", err)
			}
		case <-time.After(60 * time.Second):
			t.Fatalf("A did not end within 60s")
		}
		if !bFast {
			select {
			case <-chB:
			case <-time.After(90 * time.Second):
				t.Fatalf("B did not end within 90s")
			}
		}
		closeB()
		closeA()
		st := readVerifyRow(t, pool, migID)
		rep := parseReport(t, st.Report)
		if !bFast || bReads != 0 {
			t.Errorf("B: returned within 10s=%t, TOAST blocks read while A ran=%d — want B to start no gate", bFast, bReads)
		}
		if rep.Result != verifyGreen || rep.Integrity.RowsChecked != 3 || st.Status != "verifying" {
			t.Errorf("verdict %s status %q, want A's green run", st.Report, st.Status)
		}
	})

	// Issue #366, point 1: B's cycle read the row before A's gate stored the
	// verdict of the watermark and reached the lock after A gave it back. B
	// scans nothing: under the lock it finds the verdict stored.
	t.Run("cycle_that_read_the_row_before_the_verdict_does_not_scan", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		seedCutoverBackend(t, pool, cutoverBackendName, flippableModelMap())
		migID := seedMigrationRow(t, pool, "verifying")
		seedCleanMigrated(t, pool, 3)
		createPlainIndexT(t, pool, migrationPendingIndexName)
		stale, err := embedmigration.Active(ctx, pool)
		if err != nil || stale == nil || stale.HasVerifyReport {
			t.Fatalf("active: %+v %v", stale, err)
		}
		measuredCycles(t, pool, 1) // A: the whole gate, the verdict lands
		if rep := parseReport(t, readVerifyRow(t, pool, migID).Report); rep.Result != verifyGreen {
			t.Fatalf("A's verdict %+v, want green", rep)
		}
		sB, closeB := probeScheduler(t, pool)
		pool.Reset()
		before := stableToastBlocks(t, pool)
		if err := sB.runEmbedVerify(ctx, stale, verifyTestCfg()); err != nil {
			t.Fatalf("B: %v", err)
		}
		closeB()
		if n := toastBlocks(t, pool) - before; n != 0 {
			t.Fatalf("B read %d TOAST blocks of context_blocks after the verdict of its watermark was stored, want 0", n)
		}
	})

	// Issue #370, the smaller points: verify_report.index_repairs keeps its
	// newest entries and counts every one, and the verdict and each repair
	// entry name the release that wrote them.
	t.Run("index_repairs_capped_counted_and_release_named", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID, wm, _ := verifiedGreen(t, pool)
		mig, err := embedmigration.Active(ctx, pool)
		if err != nil || mig == nil {
			t.Fatalf("active: %+v %v", mig, err)
		}
		s := newIndexTestScheduler(pool)
		const synthetic = 52
		for i := 0; i < synthetic; i++ {
			at := time.Unix(int64(1_800_000_000+i), 0).UTC()
			rep := verifyIndexRepair{Result: verifyGreen, StartedAt: at, FinishedAt: at, Watermark: wm, ScanStages: indexRepairScanStages}
			if err := s.finishIndexRepairGreen(ctx, mig, &rep); err != nil {
				t.Fatalf("entry %d: %v", i, err)
			}
		}
		dropIndexT(t, pool, "idx_guard_pending_next")
		measuredCycles(t, pool, 1) // a real repair: the 53rd entry
		raw := readVerifyRow(t, pool, migID).Report
		m := reportSections(t, raw)
		var total int64
		_ = json.Unmarshal(m["index_repairs_total"], &total)
		var entries []map[string]any
		if err := json.Unmarshal(m["index_repairs"], &entries); err != nil {
			t.Fatalf("index_repairs: %v", err)
		}
		var release string
		_ = json.Unmarshal(m["release"], &release)
		first, _ := entries[0]["started_at"].(string)
		lastRelease, _ := entries[len(entries)-1]["release"].(string)
		wantFirst := time.Unix(1_800_000_000+synthetic+1-verifyOffenderListCap, 0).UTC().Format(time.RFC3339)
		if len(entries) != verifyOffenderListCap || total != synthetic+1 || first != wantFirst {
			t.Errorf("index_repairs: %d entries, total %d, oldest kept %q — want the newest %d of %d, oldest kept %q",
				len(entries), total, first, verifyOffenderListCap, synthetic+1, wantFirst)
		}
		if release == "" || lastRelease == "" {
			t.Errorf("release of the verdict %q, of the real repair entry %q — want both named", release, lastRelease)
		}
	})

	// Issue #370, point 3, the migration arm: a cycle that read the row in
	// verifying before the operator's confirm committed picks from the
	// swapped table afterwards. It writes nothing into the fresh
	// embedding_next pair, which the rollback requires empty.
	t.Run("arm_after_a_confirm_it_did_not_see_writes_no_fresh_pair", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID, _, _ := verifiedGreen(t, pool)
		mig, err := embedmigration.Active(ctx, pool)
		if err != nil || mig == nil {
			t.Fatalf("active: %+v %v", mig, err)
		}
		if _, err := ConfirmEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID); err != nil {
			t.Fatalf("confirm: %v", err)
		}
		s := newIndexTestScheduler(pool)
		out, err := s.migrateOneEmbedding(ctx, miRouter(t), verifyTestCfg(), mig, nil)
		if err != nil {
			t.Fatalf("migrate one: %v", err)
		}
		var written int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_blocks WHERE embedding_next IS NOT NULL OR embed_model_next IS NOT NULL`).Scan(&written); err != nil {
			t.Fatalf("fresh pair: %v", err)
		}
		if written != 0 || out.kind != migrateOutcomeDeferred {
			t.Fatalf("after the confirm: outcome %d, %d row(s) in the fresh embedding_next pair, want deferred and none", out.kind, written)
		}
	})

	// Issue #366 (handed over from #365): a CREATE INDEX CONCURRENTLY of the
	// pending index runs (held) while the operator confirms. The confirm
	// answers at once without a deadlock, the build stands.
	t.Run("confirm_beside_a_pending_index_build", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID, wm, _ := verifiedGreen(t, pool)
		dropIndexT(t, pool, migrationPendingIndexName)
		sA, _ := probeScheduler(t, pool)
		release := holdBuilds(t, pool)
		chA := make(chan error, 1)
		go func() { chA <- miCycle(t, sA, verifyTestCfg()) }()
		buildingPID(t, pool, migrationPendingIndexName)

		type confirmResult struct {
			err  error
			took time.Duration
		}
		chC := make(chan confirmResult, 1)
		go func() {
			start := time.Now()
			_, err := ConfirmEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID)
			chC <- confirmResult{err, time.Since(start)}
		}()
		var c confirmResult
		answered := false
		select {
		case c = <-chC:
			answered = true
		case <-time.After(time.Second):
		}
		// The holder goes while a confirm that still waits holds its snapshot:
		// the build then waits for that snapshot in turn.
		release()
		if !answered {
			select {
			case c = <-chC:
			case <-time.After(30 * time.Second):
				t.Fatalf("confirm did not end within 30s")
			}
		}
		var errA error
		select {
		case errA = <-chA:
		case <-time.After(60 * time.Second):
			t.Fatalf("cycle did not end within 60s")
		}
		t.Logf("confirm: answered within 1s=%t took=%s err=%v sqlstate=%q; cycle err=%v sqlstate=%q",
			answered, c.took, c.err, sqlstateOf(c.err), errA, sqlstateOf(errA))
		if !answered || !errors.Is(c.err, ErrConfirmRuntimeIndexBusy) || sqlstateOf(errA) == "40P01" {
			t.Errorf("confirm beside the build: answered within 1s=%t err=%v; cycle err=%v — want an immediate refusal and no deadlock", answered, c.err, errA)
		}
		if st := readVerifyRow(t, pool, migID); st.Status != "verifying" || !st.VerifyStartedAt.Equal(wm) {
			t.Errorf("after the refused confirm: status=%q watermark=%v", st.Status, st.VerifyStartedAt)
		}
		requireIndexOfDDL(t, pool, migrationPendingIndexName)
		if _, err := ConfirmEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID); err != nil {
			t.Fatalf("confirm after the build: %v", err)
		}
	})

	// Issue #366 (open since #331): with no active migration the cycle drops
	// the pending index on context_blocks — not a same-named index of a
	// schema that comes first in the session's search_path.
	t.Run("pending_index_drop_names_its_schema", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		for _, sql := range []string{
			`CREATE SCHEMA ctx_shadow`,
			`CREATE TABLE ctx_shadow.shadow_t (x int)`,
			`CREATE INDEX idx_embedding_next_pending ON ctx_shadow.shadow_t (x)`,
		} {
			if _, err := pool.Exec(ctx, sql); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		createPlainIndexT(t, pool, migrationPendingIndexName)
		cfg := pool.Config()
		cfg.MaxConns = 4
		cfg.ConnConfig.RuntimeParams["search_path"] = "ctx_shadow, public"
		p, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("pool: %v", err)
		}
		t.Cleanup(p.Close)
		if err := newIndexTestScheduler(p).dropMigrationPendingIndex(ctx); err != nil {
			t.Fatalf("drop: %v", err)
		}
		var shadow bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = 'ctx_shadow' AND c.relname = 'idx_embedding_next_pending')`).Scan(&shadow); err != nil {
			t.Fatalf("shadow probe: %v", err)
		}
		if ix := indexStateT(t, pool, migrationPendingIndexName); ix.exists || !shadow {
			t.Fatalf("after the drop: index on context_blocks exists=%t, shadow index exists=%t — want only the one on context_blocks dropped",
				ix.exists, shadow)
		}
	})
}
