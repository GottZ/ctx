//go:build integration

// Migration 170 gates (issue #266): the backfill counts what is there, and the
// reconcile stays correct against a writer that is in flight while it runs.
//
//	go test -tags=integration -p 1 ./internal/store/ -run 'TestMigration170|TestBlockCounts_ReconcileAgainstInFlightWriter' -count=1 -v
package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestMigration170_BackfillCountsExistingBlocks: blocks written on the 169
// schema (every key shape: archived, guard_status NULL, archived_dup live and
// archived, several scopes and categories) are counted by the migration, and
// the reading functions agree with the per-call counts right after it, and
// again after a write through the new triggers.
func TestMigration170_BackfillCountsExistingBlocks(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, 169)
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, scope, guard_status, is_archived)
		 SELECT 'c' || (g % 4), 'm170 ' || g, 'm170 body ' || g, CASE WHEN g % 3 = 0 THEN 'private' ELSE 'shared' END,
		        CASE WHEN g % 5 = 0 THEN NULL WHEN g % 7 = 0 THEN 'archived_dup' WHEN g % 2 = 0 THEN 'clean' ELSE 'active' END,
		        g % 6 = 0
		   FROM generate_series(1, 300) g`); err != nil {
		t.Fatalf("seed on 169: %v", err)
	}
	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	windows := [][]string{{"private"}, {"shared"}, {"private", "shared"}}
	assertCountParity(t, pool, "after backfill", windows)
	if _, err := store.UpsertBlock(ctx, pool, "c1", "m170 after", "after", nil, nil, "private", true, store.SensitivityWrite{}, ""); err != nil {
		t.Fatal(err)
	}
	assertCountParity(t, pool, "after a trigger write", windows)
}

// TestBlockCounts_ReconcileAgainstInFlightWriter: a key with drift (counter
// 1, no block) and a writer that adds a block to exactly that key while the
// reconcile runs. The reconcile reads the counter and the count in one
// snapshot that does not contain the writer, decides to remove the row, and
// has to wait on the writer's row lock. When the writer commits, a reconcile
// that went ahead with the delete would drop the writer's block from the
// count; REPEATABLE READ refuses the delete (40001), the scope is retried with
// a fresh snapshot, and the counter ends at the real count.
func TestBlockCounts_ReconcileAgainstInFlightWriter(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := spSetup(t)
	ctx, pool := f.ctx, f.pool
	s := f.scope(t, "cnt-inflight")
	id := f.block(t, s, "drifted", nil)
	for _, sql := range []string{
		`ALTER TABLE context_blocks DISABLE TRIGGER trg_block_counts_del`,
		fmt.Sprintf(`DELETE FROM context_blocks WHERE id = '%s'::uuid`, id),
		`ALTER TABLE context_blocks ENABLE TRIGGER trg_block_counts_del`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// The writer: its block and its counter delta, not committed yet.
	w, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Rollback(ctx) }()
	if _, err := w.Exec(ctx, `INSERT INTO context_blocks (category, title, content, scope) VALUES ('learnings', 'in-flight', 'x', $1)`, s); err != nil {
		t.Fatalf("writer insert: %v", err)
	}
	var writerPID int
	if err := w.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&writerPID); err != nil {
		t.Fatal(err)
	}

	type result struct {
		res *store.BlockCountReconcile
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := store.ReconcileBlockCounts(ctx, pool, s)
		done <- result{r, err}
	}()
	// Wait until the reconcile blocks on the writer (a lock wait whose
	// blocker is the writer's backend), then let the writer commit.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, writerPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case r := <-done:
			t.Fatalf("reconcile finished before it met the writer: %+v, %v", r.res, r.err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the reconcile never waited on the writer")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := w.Commit(ctx); err != nil {
		t.Fatalf("writer commit: %v", err)
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("reconcile: %v", r.err)
	}
	t.Logf("reconcile against the in-flight writer: %+v", *r.res)
	assertCountParity(t, pool, "after the in-flight writer", [][]string{{s}})
}
