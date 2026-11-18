//go:build integration

// Issue #332, finding 1: two sessions — two ctxd instances against one
// database, or the migration cycle and the verify goroutine of one — must
// not run runtime index DDL on context_blocks beside each other. A second
// session that meets a build in progress neither drops the index under
// construction nor starts a build of its own beside it (two CREATE INDEX
// CONCURRENTLY on one table wait for each other's snapshot and end in
// SQLSTATE 40P01); the build stands, and the second session reuses it once
// it is valid.
//
// A build is held at a defined point: a transaction that holds ROW
// EXCLUSIVE on context_blocks (a writer that has not committed) makes
// CREATE INDEX CONCURRENTLY wait in its first phase, after it has entered
// the index in the catalog as INVALID.
//
//	go test -tags=integration ./internal/events/ -run 'TestEmbedRuntimeIndexLock|TestEmbedVerifyRunBinding' -count=1 -v
package events

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embedmigration"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// instancePool opens another pool on the test database: one per simulated
// ctxd instance, so the sessions of two instances never share a backend.
func instancePool(t *testing.T, pool *pgxpool.Pool, maxConns int32) *pgxpool.Pool {
	t.Helper()
	cfg := pool.Config()
	cfg.MaxConns = maxConns
	cfg.MinConns = 0
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("instance pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// holdBuilds opens a writer transaction on context_blocks (ROW EXCLUSIVE,
// not committed). A CREATE or DROP INDEX CONCURRENTLY on the table waits for
// it. The returned function ends the transaction; it is idempotent.
func holdBuilds(t *testing.T, pool *pgxpool.Pool) func() {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE context_blocks IN ROW EXCLUSIVE MODE`); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("holder lock: %v", err)
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = tx.Rollback(ctx) }) }
	t.Cleanup(release)
	return release
}

// buildingPID waits until a backend runs CREATE INDEX CONCURRENTLY of name
// and waits (the held build) and returns its pid.
func buildingPID(t *testing.T, pool *pgxpool.Pool, name string) int32 {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var pid int32
		err := pool.QueryRow(context.Background(),
			`SELECT pid FROM pg_stat_activity
			 WHERE datname = current_database() AND pid <> pg_backend_pid()
			   AND query LIKE 'CREATE INDEX CONCURRENTLY%' AND query LIKE '%' || $1 || '%'
			   AND wait_event_type IS NOT NULL AND state = 'active'
			 LIMIT 1`, name).Scan(&pid)
		if err == nil {
			return pid
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("building pid: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no held CREATE INDEX CONCURRENTLY of %s within 20s", name)
	return 0
}

// indexDDLBesides lists the index DDL statements on context_blocks that
// backends other than pid run right now.
func indexDDLBesides(t *testing.T, pool *pgxpool.Pool, pid int32) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT pid::text || ': ' || left(regexp_replace(query, '\s+', ' ', 'g'), 120)
		 FROM pg_stat_activity
		 WHERE datname = current_database() AND pid <> pg_backend_pid() AND pid <> $1
		   AND state = 'active'
		   AND (query LIKE 'CREATE INDEX%' OR query LIKE 'DROP INDEX%')`, pid)
	if err != nil {
		t.Fatalf("index DDL scan: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("index DDL row: %v", err)
		}
		out = append(out, s)
	}
	return out
}

// watchIndexDDL observes for the window whether a backend other than pid
// starts index DDL, or done delivers first. It returns the statements seen.
func watchIndexDDL(t *testing.T, pool *pgxpool.Pool, pid int32, window time.Duration, done <-chan struct{}) []string {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if seen := indexDDLBesides(t, pool, pid); len(seen) > 0 {
			return seen
		}
		select {
		case <-done:
			return indexDDLBesides(t, pool, pid)
		case <-time.After(25 * time.Millisecond):
		}
	}
	return nil
}

// advisoryLocks counts the advisory locks held in the test database.
func advisoryLocks(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted
		   AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&n); err != nil {
		t.Fatalf("advisory locks: %v", err)
	}
	return n
}

// waitNoAdvisoryLocks waits until no advisory lock is held.
func waitNoAdvisoryLocks(t *testing.T, pool *pgxpool.Pool, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for advisoryLocks(t, pool) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("advisory locks still held after %s", within)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

type ensureResult struct {
	action string
	err    error
}

func ensureAsync(ctx context.Context, s *Scheduler, name, ddl string) (<-chan ensureResult, <-chan struct{}) {
	out := make(chan ensureResult, 1)
	done := make(chan struct{})
	go func() {
		a, err := s.ensureConcurrentIndexValid(ctx, name, ddl)
		out <- ensureResult{a, err}
		close(done)
	}()
	return out, done
}

func waitEnsure(t *testing.T, ch <-chan ensureResult, label string) ensureResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(60 * time.Second):
		t.Fatalf("%s: no result within 60s", label)
		return ensureResult{}
	}
}

func indexStateT(t *testing.T, pool *pgxpool.Pool, name string) runtimeIndexState {
	t.Helper()
	st, err := readRuntimeIndex(context.Background(), pool, name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return st
}

func indexOIDT(t *testing.T, pool *pgxpool.Pool, name string) int64 {
	t.Helper()
	var oid int64
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE((SELECT oid::bigint FROM pg_class WHERE relname = $1 AND relkind = 'i'), 0)`, name).Scan(&oid); err != nil {
		t.Fatalf("oid of %s: %v", name, err)
	}
	return oid
}

func sortDurations(d []time.Duration) {
	for i := 1; i < len(d); i++ {
		for j := i; j > 0 && d[j] < d[j-1]; j-- {
			d[j], d[j-1] = d[j-1], d[j]
		}
	}
}

// isBusy reports whether err says another session is building (the
// refusal of a second session that meets a build in progress).
func isBusy(err error) bool {
	return err != nil && strings.Contains(err.Error(), "another session is building")
}

func TestEmbedRuntimeIndexLock_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()

	// second_instance_leaves_a_build_in_progress: instance A builds the
	// pending index (held in its first phase, INVALID in the catalog);
	// instance B ensures the same index. B must neither drop A's index nor
	// build beside it; it says that another session is building. A's build
	// completes as created, its oid stands, and B reuses it afterwards.
	t.Run("second_instance_leaves_a_build_in_progress", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		seedCleanMigrated(t, pool, 3)
		sA := newIndexTestScheduler(instancePool(t, pool, 4))
		sB := newIndexTestScheduler(instancePool(t, pool, 4))

		release := holdBuilds(t, pool)
		chA, _ := ensureAsync(ctx, sA, migrationPendingIndexName, migrationPendingIndexDDL)
		pidA := buildingPID(t, pool, migrationPendingIndexName)
		if st := indexStateT(t, pool, migrationPendingIndexName); !st.exists || st.valid {
			t.Fatalf("held build: exists=%t valid=%t, want an INVALID index in the catalog", st.exists, st.valid)
		}
		oidA := indexOIDT(t, pool, migrationPendingIndexName)

		chB, doneB := ensureAsync(ctx, sB, migrationPendingIndexName, migrationPendingIndexDDL)
		seen := watchIndexDDL(t, pool, pidA, 3*time.Second, doneB)
		release()
		rA := waitEnsure(t, chA, "A")
		rB := waitEnsure(t, chB, "B")
		t.Logf("A: action=%q err=%v; B: action=%q err=%v; DDL of B during A's build: %q", rA.action, rA.err, rB.action, rB.err, seen)

		if len(seen) > 0 {
			t.Errorf("B ran index DDL while A's build was in progress: %q", seen)
		}
		if !isBusy(rB.err) {
			t.Errorf("B: action=%q err=%v, want the refusal that another session is building", rB.action, rB.err)
		}
		if rA.err != nil || rA.action != indexActionCreated {
			t.Errorf("A: action=%q err=%v, want %s", rA.action, rA.err, indexActionCreated)
		}
		if st := indexStateT(t, pool, migrationPendingIndexName); !st.exists || !st.valid {
			t.Errorf("after both: exists=%t valid=%t, want A's valid index", st.exists, st.valid)
		}
		if got := indexOIDT(t, pool, migrationPendingIndexName); got != oidA {
			t.Errorf("index oid %d → %d: A's build was replaced", oidA, got)
		}
		if a, err := sB.ensureConcurrentIndexValid(ctx, migrationPendingIndexName, migrationPendingIndexDDL); err != nil || a != indexActionReused {
			t.Errorf("B after A's build: action=%q err=%v, want %s", a, err, indexActionReused)
		}
	})

	// second_instance_does_not_build_beside_a_build: A builds one runtime
	// index, B ensures ANOTHER one on the same table. Two CREATE INDEX
	// CONCURRENTLY on one table wait for each other (40P01 at the end), so B
	// must not start its build beside A's either.
	t.Run("second_instance_does_not_build_beside_a_build", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		seedCleanMigrated(t, pool, 3)
		sA := newIndexTestScheduler(instancePool(t, pool, 4))
		sB := newIndexTestScheduler(instancePool(t, pool, 4))
		other := "idx_dream_pending_next"

		release := holdBuilds(t, pool)
		chA, _ := ensureAsync(ctx, sA, migrationPendingIndexName, migrationPendingIndexDDL)
		pidA := buildingPID(t, pool, migrationPendingIndexName)
		chB, doneB := ensureAsync(ctx, sB, other, ddlOfManaged(other))
		seen := watchIndexDDL(t, pool, pidA, 3*time.Second, doneB)
		release()
		rA := waitEnsure(t, chA, "A")
		rB := waitEnsure(t, chB, "B")
		t.Logf("A: action=%q err=%v; B: action=%q err=%v; DDL of B during A's build: %q", rA.action, rA.err, rB.action, rB.err, seen)

		if len(seen) > 0 {
			t.Errorf("B ran index DDL on context_blocks while A's build was in progress: %q", seen)
		}
		if !isBusy(rB.err) {
			t.Errorf("B: action=%q err=%v, want the refusal that another session is building", rB.action, rB.err)
		}
		if rA.err != nil || rA.action != indexActionCreated {
			t.Errorf("A: action=%q err=%v, want %s", rA.action, rA.err, indexActionCreated)
		}
		if a, err := sB.ensureConcurrentIndexValid(ctx, other, ddlOfManaged(other)); err != nil || a != indexActionCreated {
			t.Errorf("B after A's build: action=%q err=%v, want %s", a, err, indexActionCreated)
		}
	})

	// builder_dies_mid_build: A's session is terminated while its build is
	// held. The INVALID leftover stays, no advisory lock outlives the
	// session, and B drops the leftover and builds the index.
	t.Run("builder_dies_mid_build", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		seedCleanMigrated(t, pool, 3)
		sA := newIndexTestScheduler(instancePool(t, pool, 4))
		sB := newIndexTestScheduler(instancePool(t, pool, 4))

		release := holdBuilds(t, pool)
		chA, _ := ensureAsync(ctx, sA, migrationPendingIndexName, migrationPendingIndexDDL)
		pidA := buildingPID(t, pool, migrationPendingIndexName)
		if _, err := pool.Exec(ctx, `SELECT pg_terminate_backend($1)`, pidA); err != nil {
			t.Fatalf("terminate A: %v", err)
		}
		rA := waitEnsure(t, chA, "A")
		release()
		t.Logf("A after termination: action=%q err=%v", rA.action, rA.err)
		if rA.err == nil {
			t.Fatalf("A: no error after its session was terminated")
		}
		if st := indexStateT(t, pool, migrationPendingIndexName); !st.exists || st.valid {
			t.Fatalf("after A's death: exists=%t valid=%t, want an INVALID leftover", st.exists, st.valid)
		}
		waitNoAdvisoryLocks(t, pool, 10*time.Second)

		a, err := sB.ensureConcurrentIndexValid(ctx, migrationPendingIndexName, migrationPendingIndexDDL)
		if err != nil || a != indexActionRebuiltInvalid {
			t.Errorf("B on A's leftover: action=%q err=%v, want %s", a, err, indexActionRebuiltInvalid)
		}
		if st := indexStateT(t, pool, migrationPendingIndexName); !st.exists || !st.valid {
			t.Errorf("after B: exists=%t valid=%t, want valid", st.exists, st.valid)
		}
	})

	// no_lock_goes_back_into_the_pool: a pool of ONE connection. After every
	// way out of the helper — a build, a reuse, a failed read-back, a context
	// canceled during the build — no advisory lock is held by a pooled
	// session, and the pool still serves.
	t.Run("no_lock_goes_back_into_the_pool", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		seedCleanMigrated(t, pool, 3)
		one := instancePool(t, pool, 1)
		s := newIndexTestScheduler(one)
		pooledPIDHoldsNoLock := func(label string) {
			t.Helper()
			var pid int32
			var held int
			if err := one.QueryRow(ctx, `SELECT pg_backend_pid(),
			        (SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid())`).Scan(&pid, &held); err != nil {
				t.Fatalf("%s: pooled session: %v", label, err)
			}
			if held != 0 {
				t.Errorf("%s: the pooled session %d holds %d advisory lock(s)", label, pid, held)
			}
		}

		if a, err := s.ensureConcurrentIndexValid(ctx, migrationPendingIndexName, migrationPendingIndexDDL); err != nil || a != indexActionCreated {
			t.Fatalf("build: action=%q err=%v", a, err)
		}
		pooledPIDHoldsNoLock("after a build")
		if a, err := s.ensureConcurrentIndexValid(ctx, migrationPendingIndexName, migrationPendingIndexDDL); err != nil || a != indexActionReused {
			t.Fatalf("reuse: action=%q err=%v", a, err)
		}
		pooledPIDHoldsNoLock("after a reuse")

		// A read-back that fails: the name is taken by an index on another
		// table, CREATE ... IF NOT EXISTS skips.
		for _, sql := range []string{
			`CREATE TABLE ctx_other_t (x int)`,
			`CREATE INDEX idx_guard_pending_next ON ctx_other_t (x)`,
		} {
			if _, err := pool.Exec(ctx, sql); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		if _, err := s.ensureConcurrentIndexValid(ctx, "idx_guard_pending_next", ddlOfManaged("idx_guard_pending_next")); err == nil {
			t.Fatalf("taken name: no error")
		}
		pooledPIDHoldsNoLock("after a failed read-back")
		if advisoryLocks(t, pool) != 0 {
			t.Errorf("advisory locks held after the failed read-back")
		}

		// A context canceled during a held build: the session that carries
		// the build is not pooled again; it keeps building server-side and
		// gives up its lock when the build is done.
		release := holdBuilds(t, pool)
		cctx, cancel := context.WithCancel(ctx)
		ch, _ := ensureAsync(cctx, s, "idx_dream_pending_next", ddlOfManaged("idx_dream_pending_next"))
		pid := buildingPID(t, pool, "idx_dream_pending_next")
		cancel()
		r := waitEnsure(t, ch, "canceled build")
		t.Logf("canceled build: action=%q err=%v (build session %d)", r.action, r.err, pid)
		if r.err == nil {
			t.Errorf("canceled build: no error")
		}
		pooledPIDHoldsNoLock("after a canceled build")
		release()
		waitNoAdvisoryLocks(t, pool, 20*time.Second)
		pooledPIDHoldsNoLock("after the canceled build ended server-side")
	})

	// confirm_refuses_while_another_instance_builds: a green verdict stands,
	// and another instance rebuilds a _next sister index (held). The confirm
	// refuses (the index is INVALID) and the row stays verifying.
	t.Run("confirm_refuses_while_another_instance_builds", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		seedCleanMigrated(t, pool, 3)
		migID := seedConfirmReady(t, pool, 0)
		sister := "idx_guard_pending_next"
		if _, err := pool.Exec(ctx, `DROP INDEX `+sister); err != nil {
			t.Fatalf("drop %s: %v", sister, err)
		}
		sB := newIndexTestScheduler(instancePool(t, pool, 4))
		release := holdBuilds(t, pool)
		chB, _ := ensureAsync(ctx, sB, sister, ddlOfManaged(sister))
		buildingPID(t, pool, sister)

		bpool := backends.NewPool(pool, nil)
		_, err := ConfirmEmbedMigration(ctx, pool, bpool, migID)
		release()
		rB := waitEnsure(t, chB, "B")
		t.Logf("confirm during the build: %v; B: action=%q err=%v", err, rB.action, rB.err)
		if !errors.Is(err, ErrConfirmNextIndexNotReady) {
			t.Errorf("confirm during another instance's build: %v, want ErrConfirmNextIndexNotReady", err)
		}
		if st := readVerifyRow(t, pool, migID); st.Status != "verifying" {
			t.Errorf("status after the refused confirm: %q, want verifying", st.Status)
		}
	})

	// verify_waits_for_the_build_of_another_instance: instance A builds the
	// HNSW twin (held); instance B runs its verify gate. B must not drop or
	// build while A's build is in progress; once A is done, B's gate reuses
	// A's index and goes green.
	t.Run("verify_waits_for_the_build_of_another_instance", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID := seedMigrationRow(t, pool, "verifying")
		seedCleanMigrated(t, pool, 3)
		sA := newIndexTestScheduler(instancePool(t, pool, 4))
		poolB := instancePool(t, pool, 4)
		sB := newIndexTestScheduler(poolB)
		sB.embedVerifySync = true
		// The migration cycle ensures the pending index before it starts the
		// gate; it stands here, so B's cycle reaches the gate.
		if _, err := sA.ensureConcurrentIndexValid(ctx, migrationPendingIndexName, migrationPendingIndexDDL); err != nil {
			t.Fatalf("pending index: %v", err)
		}

		release := holdBuilds(t, pool)
		chA, _ := ensureAsync(ctx, sA, verifyNextHNSWIndexName, verifyNextHNSWIndexDDL)
		pidA := buildingPID(t, pool, verifyNextHNSWIndexName)
		oidA := indexOIDT(t, pool, verifyNextHNSWIndexName)

		srv := newMigrateEmbedServer(t)
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{embedNextPoolRow("embed-a", srv.srv.URL, 100)})
		d := dispatch.New(nil, dispatch.DefaultSettings())
		t.Cleanup(d.Close)
		router := backfillRouter(bpool, d)
		cycleDone := make(chan error, 1)
		doneB := make(chan struct{})
		go func() {
			cycleDone <- sB.runEmbedMigrationCycle(ctx, router, verifyTestCfg())
			close(doneB)
		}()
		seen := watchIndexDDL(t, pool, pidA, 3*time.Second, doneB)
		release()
		rA := waitEnsure(t, chA, "A")
		var cycleErr error
		select {
		case cycleErr = <-cycleDone:
		case <-time.After(90 * time.Second):
			t.Fatalf("B's verify cycle did not end within 90s")
		}
		st := readVerifyRow(t, pool, migID)
		t.Logf("A: action=%q err=%v; B cycle: %v; DDL of B during A's build: %q; status=%s report=%s",
			rA.action, rA.err, cycleErr, seen, st.Status, st.Report)

		if len(seen) > 0 {
			t.Errorf("B's verify ran index DDL while A's build was in progress: %q", seen)
		}
		if rA.err != nil || rA.action != indexActionCreated {
			t.Errorf("A: action=%q err=%v, want %s", rA.action, rA.err, indexActionCreated)
		}
		if got := indexOIDT(t, pool, verifyNextHNSWIndexName); got != oidA {
			t.Errorf("HNSW oid %d → %d: A's build was replaced", oidA, got)
		}
		if st.Status != "verifying" || len(st.Report) == 0 {
			t.Fatalf("after B's verify: status=%q report=%t, want verifying with a green report", st.Status, len(st.Report) > 0)
		}
		rep := parseReport(t, st.Report)
		if rep.Result != verifyGreen {
			t.Errorf("B's verdict %q (index section %+v), want green", rep.Result, rep.Index)
		}
		for _, ix := range rep.Index.Indexes {
			if ix.Name == verifyNextHNSWIndexName && ix.Action != indexActionReused {
				t.Errorf("B's report: HNSW action %q, want %s (A built it)", ix.Action, indexActionReused)
			}
		}
	})

	// cycle_defers_while_another_instance_builds: instance A builds the
	// pending index (held); instance B runs a migration cycle. B must not
	// touch the index under construction, and it migrates nothing in that
	// cycle (its pick reads that index); the next cycle after A's build
	// migrates.
	t.Run("cycle_defers_while_another_instance_builds", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID := seedMigrationRow(t, pool, "running")
		seedMigratableBlock(t, pool, "mig-1", "content one", "public", 2*time.Hour)
		sA := newIndexTestScheduler(instancePool(t, pool, 4))
		sB := newIndexTestScheduler(instancePool(t, pool, 4))

		release := holdBuilds(t, pool)
		chA, _ := ensureAsync(ctx, sA, migrationPendingIndexName, migrationPendingIndexDDL)
		pidA := buildingPID(t, pool, migrationPendingIndexName)
		oidA := indexOIDT(t, pool, migrationPendingIndexName)

		srv := newMigrateEmbedServer(t)
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{embedNextPoolRow("embed-a", srv.srv.URL, 100)})
		d := dispatch.New(nil, dispatch.DefaultSettings())
		t.Cleanup(d.Close)
		router := backfillRouter(bpool, d)
		cycleDone := make(chan error, 1)
		doneB := make(chan struct{})
		go func() {
			cycleDone <- sB.runEmbedMigrationCycle(ctx, router, migCfg(8, 0))
			close(doneB)
		}()
		seen := watchIndexDDL(t, pool, pidA, 3*time.Second, doneB)
		migratedDuringBuild := countNextEmbedded(t, pool)
		release()
		rA := waitEnsure(t, chA, "A")
		var cycleErr error
		select {
		case cycleErr = <-cycleDone:
		case <-time.After(60 * time.Second):
			t.Fatalf("B's cycle did not end within 60s")
		}
		t.Logf("A: action=%q err=%v; B cycle: %v; DDL of B during A's build: %q; migrated during the build: %d",
			rA.action, rA.err, cycleErr, seen, migratedDuringBuild)

		if len(seen) > 0 {
			t.Errorf("B's cycle ran index DDL while A's build was in progress: %q", seen)
		}
		if cycleErr != nil {
			t.Errorf("B's cycle beside A's build: %v, want a deferred cycle without an error", cycleErr)
		}
		if migratedDuringBuild != 0 {
			t.Errorf("B migrated %d block(s) while the pending index was being built", migratedDuringBuild)
		}
		if got := indexOIDT(t, pool, migrationPendingIndexName); got != oidA {
			t.Errorf("pending index oid %d → %d: A's build was replaced", oidA, got)
		}
		if err := sB.runEmbedMigrationCycle(ctx, router, migCfg(8, 0)); err != nil {
			t.Fatalf("B's next cycle: %v", err)
		}
		if n := countNextEmbedded(t, pool); n != 1 {
			t.Errorf("B's next cycle migrated %d block(s), want 1", n)
		}
		if st := readMigrationRow(t, pool, migID); st.Migrated != 1 {
			t.Errorf("migrated_count %d, want 1", st.Migrated)
		}
	})

	// terminal_drop_waits_for_a_build: no migration is active any more (the
	// row was aborted), and another instance's verify gate still builds a
	// _next index (held). B's cycle, which drops the pending index once no
	// migration is active, must not run its DROP INDEX CONCURRENTLY beside
	// that build; it drops on a later tick.
	t.Run("terminal_drop_waits_for_a_build", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		seedCleanMigrated(t, pool, 3)
		sA := newIndexTestScheduler(instancePool(t, pool, 4))
		sB := newIndexTestScheduler(instancePool(t, pool, 4))
		if _, err := sA.ensureConcurrentIndexValid(ctx, migrationPendingIndexName, migrationPendingIndexDDL); err != nil {
			t.Fatalf("pending index: %v", err)
		}
		migID := seedMigrationRow(t, pool, "verifying")
		if err := embedmigration.Transition(ctx, pool, migID, embedmigration.StatusVerifying, embedmigration.StatusAborted,
			embedmigration.WithAbortReason("test")); err != nil {
			t.Fatalf("abort: %v", err)
		}

		release := holdBuilds(t, pool)
		chA, _ := ensureAsync(ctx, sA, "idx_dream_pending_next", ddlOfManaged("idx_dream_pending_next"))
		pidA := buildingPID(t, pool, "idx_dream_pending_next")
		cycleDone := make(chan error, 1)
		doneB := make(chan struct{})
		go func() {
			cycleDone <- sB.runEmbedMigrationCycle(ctx, nil, migCfg(8, 0))
			close(doneB)
		}()
		seen := watchIndexDDL(t, pool, pidA, 3*time.Second, doneB)
		release()
		rA := waitEnsure(t, chA, "A")
		var cycleErr error
		select {
		case cycleErr = <-cycleDone:
		case <-time.After(60 * time.Second):
			t.Fatalf("B's cycle did not end within 60s")
		}
		t.Logf("A: action=%q err=%v; B cycle: %v; DDL of B during A's build: %q", rA.action, rA.err, cycleErr, seen)
		if len(seen) > 0 {
			t.Errorf("B's terminal drop ran beside A's build: %q", seen)
		}
		if cycleErr != nil {
			t.Errorf("B's cycle: %v, want a deferred drop without an error", cycleErr)
		}
		if rA.err != nil || rA.action != indexActionCreated {
			t.Errorf("A: action=%q err=%v, want %s", rA.action, rA.err, indexActionCreated)
		}
		if err := sB.runEmbedMigrationCycle(ctx, nil, migCfg(8, 0)); err != nil {
			t.Fatalf("B's next cycle: %v", err)
		}
		if indexOIDT(t, pool, migrationPendingIndexName) != 0 {
			t.Errorf("pending index still there after the next cycle without an active migration")
		}
	})

	// reuse_takes_no_lock: the common case — every runtime index valid and of
	// its DDL's definition — reads the catalog and reuses without the lock,
	// so a lock held by another session does not hold it up. It also times
	// the reuse of all seven managed indexes (the per-cycle and per-re-run
	// cost of the stage).
	t.Run("reuse_takes_no_lock", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		seedCleanMigrated(t, pool, 3)
		s := newIndexTestScheduler(instancePool(t, pool, 4))
		for _, ix := range managedRuntimeIndexes() {
			if _, err := s.ensureConcurrentIndexValid(ctx, ix.name, ix.ddl); err != nil {
				t.Fatalf("build %s: %v", ix.name, err)
			}
		}
		holder, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("holder: %v", err)
		}
		defer holder.Release()
		var locked bool
		if err := holder.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('ctx:runtime_index'), hashtext('context_blocks'))`).Scan(&locked); err != nil || !locked {
			t.Fatalf("holder lock: %v %v", locked, err)
		}
		defer func() { _, _ = holder.Exec(ctx, `SELECT pg_advisory_unlock_all()`) }()

		const n = 50
		durs := make([]time.Duration, 0, n)
		for i := 0; i < n; i++ {
			start := time.Now()
			for _, ix := range managedRuntimeIndexes() {
				a, err := s.ensureConcurrentIndexValid(ctx, ix.name, ix.ddl)
				if err != nil || a != indexActionReused {
					t.Fatalf("%s with the lock held elsewhere: action=%q err=%v, want %s", ix.name, a, err, indexActionReused)
				}
			}
			durs = append(durs, time.Since(start))
		}
		sortDurations(durs)
		t.Logf("reuse of %d managed indexes, n=%d: median %s, p90 %s, max %s",
			len(managedRuntimeIndexes()), n, durs[n/2], durs[n*9/10], durs[n-1])
	})

	// two_free_running_instances: the reviewer's case of #331 as a
	// repetition — two instances run the verify gate's index list and the
	// pending index at the same time, freely, over a set of rounds. Every
	// round starts without any runtime index. Per round: no error other
	// than the refusal that another session is building (the caller of the
	// helper retries it, as the verify gate does), and every index is built
	// exactly once.
	t.Run("two_free_running_instances", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		seedCleanMigrated(t, pool, 20)
		sA := newIndexTestScheduler(instancePool(t, pool, 4))
		sB := newIndexTestScheduler(instancePool(t, pool, 4))
		names := managedRuntimeIndexes()
		const rounds = 10
		failedRounds, doubledRounds := 0, 0
		for round := 0; round < rounds; round++ {
			for _, ix := range names {
				if _, err := pool.Exec(ctx, `DROP INDEX IF EXISTS `+ix.name); err != nil {
					t.Fatalf("round %d: drop %s: %v", round, ix.name, err)
				}
			}
			type tally struct {
				builds map[string]int
				errs   []string
			}
			run := func(s *Scheduler, offset int) tally {
				out := tally{builds: map[string]int{}}
				for i := range names {
					ix := names[(i+offset)%len(names)]
					for attempt := 0; ; attempt++ {
						a, err := s.ensureConcurrentIndexValid(ctx, ix.name, ix.ddl)
						if isBusy(err) && attempt < 2000 {
							time.Sleep(5 * time.Millisecond)
							continue
						}
						if err != nil {
							out.errs = append(out.errs, fmt.Sprintf("%s: %v", ix.name, err))
						} else if a != indexActionReused {
							out.builds[ix.name]++
						}
						break
					}
				}
				return out
			}
			var wg sync.WaitGroup
			var ta, tb tally
			wg.Add(2)
			go func() { defer wg.Done(); ta = run(sA, 0) }()
			go func() { defer wg.Done(); tb = run(sB, round%len(names)) }()
			wg.Wait()
			doubled := []string{}
			for _, ix := range names {
				if n := ta.builds[ix.name] + tb.builds[ix.name]; n > 1 {
					doubled = append(doubled, fmt.Sprintf("%s×%d", ix.name, n))
				}
			}
			if len(ta.errs)+len(tb.errs) > 0 {
				failedRounds++
			}
			if len(doubled) > 0 {
				doubledRounds++
			}
			t.Logf("round %d: errors A=%q B=%q doubled=%q", round, ta.errs, tb.errs, doubled)
		}
		t.Logf("rounds=%d with an error=%d with a doubled build=%d", rounds, failedRounds, doubledRounds)
		if failedRounds > 0 || doubledRounds > 0 {
			t.Errorf("%d of %d rounds ended with an error, %d with a doubled build; want 0 and 0", failedRounds, rounds, doubledRounds)
		}
		for _, ix := range names {
			if st := indexStateT(t, pool, ix.name); !st.exists || !st.valid {
				t.Errorf("%s after the rounds: exists=%t valid=%t", ix.name, st.exists, st.valid)
			}
		}
	})
}

// Issue #332, finding 2: a verdict and a confirm belong to the verify run of
// one watermark. A run still in flight when the row re-enters verifying
// (resume, drain, verifying again with a newer watermark) must not land its
// verdict on the newer run, and a confirm prepared on the earlier run's
// green report must not swap the newer one.
func TestEmbedVerifyRunBinding_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()

	// reenter moves the verifying row through resume into running and back
	// into verifying with a fresh watermark and a cleared report — the path
	// maybeEnterVerifying takes after a resume — and returns the new
	// watermark.
	reenter := func(t *testing.T, pool *pgxpool.Pool, migID string, before time.Time) time.Time {
		t.Helper()
		if err := embedmigration.Transition(ctx, pool, migID, embedmigration.StatusVerifying, embedmigration.StatusRunning); err != nil {
			t.Fatalf("resume: %v", err)
		}
		time.Sleep(2 * time.Millisecond) // a later now() for the new watermark
		if err := embedmigration.Transition(ctx, pool, migID, embedmigration.StatusRunning, embedmigration.StatusVerifying,
			embedmigration.WithVerifyStartedAt(), embedmigration.WithVerifyReportCleared()); err != nil {
			t.Fatalf("re-enter verifying: %v", err)
		}
		st := readVerifyRow(t, pool, migID)
		if st.VerifyStartedAt == nil || !st.VerifyStartedAt.After(before) {
			t.Fatalf("watermark after re-entry %v, want later than %v", st.VerifyStartedAt, before)
		}
		return *st.VerifyStartedAt
	}
	activeT := func(t *testing.T, pool *pgxpool.Pool) *embedmigration.Migration {
		t.Helper()
		mig, err := embedmigration.Active(ctx, pool)
		if err != nil || mig == nil || mig.VerifyStartedAt == nil {
			t.Fatalf("active migration: %+v err=%v", mig, err)
		}
		return mig
	}

	// late_green_of_an_earlier_run_is_discarded: run 1 (watermark W1)
	// finishes green after the row re-entered verifying with W2. Its report
	// must not land; the next cycle runs the gate for W2.
	t.Run("late_green_of_an_earlier_run_is_discarded", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		s := newIndexTestScheduler(pool)
		s.embedVerifySync = true
		migID := seedMigrationRow(t, pool, "verifying")
		seedCleanMigrated(t, pool, 3)
		run1 := activeT(t, pool)
		w2 := reenter(t, pool, migID, *run1.VerifyStartedAt)

		if err := s.runEmbedVerify(ctx, run1, verifyTestCfg()); err != nil {
			t.Fatalf("run 1: %v", err)
		}
		st := readVerifyRow(t, pool, migID)
		t.Logf("after run 1: status=%s watermark=%v report=%s", st.Status, st.VerifyStartedAt, st.Report)
		if len(st.Report) > 0 {
			rep := parseReport(t, st.Report)
			t.Errorf("the %s report of run 1 (watermark %v) stands on the row of watermark %v", rep.Result, rep.Watermark, w2)
		}
		if st.Status != "verifying" {
			t.Errorf("status %q, want verifying", st.Status)
		}

		runVerifyCycle(t, pool, s, verifyTestCfg())
		st = readVerifyRow(t, pool, migID)
		rep := parseReport(t, st.Report)
		if rep.Result != verifyGreen || !rep.Watermark.Equal(w2) {
			t.Errorf("run 2: result %q watermark %v, want green for %v", rep.Result, rep.Watermark, w2)
		}
	})

	// late_red_of_an_earlier_run_is_discarded: the same with a red verdict
	// of run 1 (a block before the watermark without embedding_next): it
	// must neither store its report nor pause the row of run 2.
	t.Run("late_red_of_an_earlier_run_is_discarded", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		s := newIndexTestScheduler(pool)
		migID := seedMigrationRow(t, pool, "verifying")
		seedCleanMigrated(t, pool, 2)
		seedMigratableBlock(t, pool, "missing", "not migrated", "public", 3*time.Hour)
		run1 := activeT(t, pool)
		w2 := reenter(t, pool, migID, *run1.VerifyStartedAt)

		if err := s.runEmbedVerify(ctx, run1, verifyTestCfg()); err != nil {
			t.Fatalf("run 1: %v", err)
		}
		st := readVerifyRow(t, pool, migID)
		t.Logf("after run 1: status=%s watermark=%v last_error=%v report=%s", st.Status, st.VerifyStartedAt, st.LastErr, st.Report)
		if st.Status != "verifying" {
			t.Errorf("run 1's red verdict moved the row of watermark %v to %q, want verifying", w2, st.Status)
		}
		if len(st.Report) > 0 {
			rep := parseReport(t, st.Report)
			t.Errorf("the %s report of run 1 (watermark %v) stands on the row of watermark %v", rep.Result, rep.Watermark, w2)
		}
		if st.LastErr != nil {
			t.Errorf("last_error %q set by run 1", *st.LastErr)
		}
	})

	// confirm_of_an_earlier_run_does_not_swap: the confirm read the green
	// report of run 1; before its swap the row re-entered verifying with W2
	// (no verdict yet). The swap must not commit.
	t.Run("confirm_of_an_earlier_run_does_not_swap", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		seedCleanMigrated(t, pool, 3)
		migID := seedConfirmReady(t, pool, 0)
		row, err := loadCutoverRow(ctx, pool, migID)
		if err != nil || row.watermark == nil {
			t.Fatalf("load row: %+v %v", row, err)
		}
		targets, err := embedFlipTargets(ctx, pool, row.toModel)
		if err != nil || len(targets) == 0 {
			t.Fatalf("flip targets: %v %v", targets, err)
		}
		reenter(t, pool, migID, *row.watermark)

		err = runConfirmSwapTx(ctx, pool, migID, row, targets)
		st := readVerifyRow(t, pool, migID)
		t.Logf("swap on run 1's preflight: %v; status=%s", err, st.Status)
		if st.Status != "verifying" {
			t.Errorf("the confirm of run 1 moved the row of the newer watermark to %q", st.Status)
		}
		if err == nil {
			t.Errorf("swap committed for a row whose watermark moved")
		}
		if !indexExistsT(t, pool, verifyNextHNSWIndexName) {
			t.Errorf("%s renamed by a swap that should not have run", verifyNextHNSWIndexName)
		}
	})
}
