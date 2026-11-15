//go:build integration

// Migration 170 gates (issue #266): the backfill counts what is there, and the
// reconcile stays correct against a writer that is in flight while it runs.
//
//	go test -tags=integration -p 1 ./internal/store/ -run 'TestMigration170|TestBlockCounts_ReconcileAgainstInFlightWriter' -count=1 -v
package store_test

import (
	"context"
	"fmt"
	"reflect"
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
// reconcile runs. The reconcile's snapshot holds neither the writer's block nor
// its delta row; it corrects by -1 without waiting for the writer, the writer
// commits its +1 afterwards, and the counters end at the real count — with the
// deltas pending and after a fold.
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
	// The writer: its block and its delta row, not committed yet.
	w, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Rollback(ctx) }()
	if _, err := w.Exec(ctx, `INSERT INTO context_blocks (category, title, content, scope) VALUES ('learnings', 'in-flight', 'x', $1)`, s); err != nil {
		t.Fatalf("writer insert: %v", err)
	}
	done := make(chan error, 1)
	var res *store.BlockCountReconcile
	go func() {
		var err error
		res, err = store.ReconcileBlockCounts(ctx, pool, s)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the reconcile waited for the in-flight writer")
	}
	active := "active"
	want := []store.BlockCountDrift{{Scope: s, Category: "learnings", GuardStatus: &active, Counted: 1, Actual: 0}}
	if !reflect.DeepEqual(res.Corrections, want) {
		t.Fatalf("corrections = %+v, want %+v", res.Corrections, want)
	}
	if err := w.Commit(ctx); err != nil {
		t.Fatalf("writer commit: %v", err)
	}
	assertCountParity(t, pool, "after the in-flight writer", [][]string{{s}})
	if _, err := store.FoldBlockCountDeltas(ctx, pool); err != nil {
		t.Fatal(err)
	}
	assertCountParity(t, pool, "after the in-flight writer, folded", [][]string{{s}})
}
