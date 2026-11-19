//go:build integration

// Issue #360: since migration 182 the pick (pickPending) locks the oldest
// waiting blocks of each checked type in a range of idx_guard_pending_type,
// keeps the oldest over all types, gives the other locks back and locks the
// kept blocks again. The order a batch takes its blocks in and how runs at
// the same time (several ctx instances on one database, FOR NO KEY UPDATE
// SKIP LOCKED since #359) split the waiting blocks stay what they were with
// one statement over the created_at key: the oldest over all checked types
// first, no block in two runs, and a run takes nothing only when every
// waiting block is held.
//
// In-package test on purpose: it runs the ACTUAL pickPending.
package guard

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestGuardPickTwoRunsOldestFirstOverAllTypes(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	if err := reg.Reload(ctx, pool); err != nil {
		t.Fatal(err)
	}
	types := reg.Snapshot().GuardCheckTypes()
	if len(types) < 3 {
		t.Fatalf("fixture: the shipped registry checks %v, the test needs three types", types)
	}
	// 250 waiting blocks one millisecond apart, their type turning over three
	// checked types against the order of the type key (the newest-named type
	// first), and between them blocks the pick filters out: a type with
	// guard.check = false and the synthesis lifecycle.
	if _, err := pool.Exec(ctx, `INSERT INTO context_blocks (category, title, content, scope, type_name, embedding, embed_model, created_at, updated_at)
		SELECT 'po', 'po-w-' || g, 'waiting ' || g, 'po-' || (g % 7), ($1::text[])[3 - g % 3],
		       array_fill(0.01::real, ARRAY[1024])::vector(1024), 'q360',
		       timestamptz '2026-11-01 00:00:00+00' + g * interval '1 millisecond',
		       timestamptz '2026-11-01 00:00:00+00' + g * interval '1 millisecond'
		  FROM generate_series(1, 250) g`, types[:3]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO context_blocks (category, title, content, scope, type_name, lifecycle_state, embedding, embed_model, created_at, updated_at)
		SELECT 'po', 'po-f-' || g, 'foreign ' || g, 'po-' || (g % 7),
		       CASE WHEN g % 2 = 0 THEN 'tool-evidence' ELSE ($1::text[])[1] END,
		       CASE WHEN g % 2 = 0 THEN 'knowledge' ELSE 'synthesis' END,
		       array_fill(0.01::real, ARRAY[1024])::vector(1024), 'q360',
		       timestamptz '2026-11-01 00:00:00+00' + g * interval '1 millisecond' + interval '500 microseconds',
		       timestamptz '2026-11-01 00:00:00+00' + g * interval '1 millisecond' + interval '500 microseconds'
		  FROM generate_series(1, 250) g`, types[:3]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
		t.Fatal(err)
	}
	var want []string
	rows, err := pool.Query(ctx, `SELECT id::text FROM context_blocks WHERE title LIKE 'po-w-%' ORDER BY created_at`)
	if err != nil {
		t.Fatal(err)
	}
	if want, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || len(want) != 250 {
		t.Fatalf("fixture: %d waiting blocks (%v)", len(want), err)
	}

	pick := func(label string) (pgx.Tx, []string) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		blocks, err := pickPending(ctx, tx, types, guardPickLimit)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		var ids []string
		for _, b := range blocks {
			ids = append(ids, b.id)
		}
		// The pick turns sorts off for its first step only: whichever way it
		// ended, the batch's own statements plan with sorts as configured.
		var sortSetting string
		if err := tx.QueryRow(ctx, `SHOW enable_sort`).Scan(&sortSetting); err != nil || sortSetting != "on" {
			t.Errorf("%s: enable_sort after the pick is %q (%v), want on", label, sortSetting, err)
		}
		return tx, ids
	}
	same := func(label string, got, want []string) {
		t.Helper()
		if !slices.Equal(got, want) {
			first := func(xs []string) string {
				if len(xs) > 3 {
					return fmt.Sprint(xs[:3], "…")
				}
				return fmt.Sprint(xs)
			}
			t.Errorf("%s took %d blocks %s, want the %d blocks %s in created_at order", label, len(got), first(got), len(want), first(want))
		}
	}

	tx1, run1 := pick("run 1")
	same("run 1", run1, want[:100])
	tx2, run2 := pick("run 2 while run 1 holds its batch")
	same("run 2", run2, want[100:200])
	tx3, run3 := pick("run 3 while runs 1 and 2 hold theirs")
	same("run 3", run3, want[200:250])
	tx4, run4 := pick("run 4 while every waiting block is held")
	same("run 4", run4, nil)
	_ = tx4.Rollback(ctx)

	// Run 1 writes its verdicts and commits; runs 2 and 3 end without one.
	if _, err := tx1.Exec(ctx, `UPDATE context_blocks SET metadata = metadata || jsonb_build_object('guard_checked_at', now(), 'guard_status', 'clean')
		WHERE id = ANY($1::uuid[])`, run1); err != nil {
		t.Fatal(err)
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx5, run5 := pick("run 5 after run 1 committed, runs 2 and 3 still open")
	same("run 5", run5, nil)
	_ = tx5.Rollback(ctx)
	_ = tx2.Rollback(ctx)
	_ = tx3.Rollback(ctx)
	_, run6 := pick("run 6 after runs 2 and 3 ended")
	same("run 6", run6, want[100:200])
}

// TestGuardPickSecondRunPassesHeldBlocksOfOneType: more blocks of one checked
// type wait than a batch takes. A run that holds the oldest batch does not
// make the next run come back empty: the lock sits in the type's branch, so
// SKIP LOCKED passes over the held blocks and the branch goes on to the next
// of that type (a lock above the branch would read the same oldest batch of
// the type, find all of it held and return nothing).
func TestGuardPickSecondRunPassesHeldBlocksOfOneType(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	if err := reg.Reload(ctx, pool); err != nil {
		t.Fatal(err)
	}
	types := reg.Snapshot().GuardCheckTypes()
	if _, err := pool.Exec(ctx, `INSERT INTO context_blocks (category, title, content, scope, type_name, embedding, embed_model, created_at, updated_at)
		SELECT 'po', 'po-k-' || g, 'waiting ' || g, 'po', 'knowledge',
		       array_fill(0.01::real, ARRAY[1024])::vector(1024), 'q360',
		       timestamptz '2026-11-01 00:00:00+00' + g * interval '1 millisecond',
		       timestamptz '2026-11-01 00:00:00+00' + g * interval '1 millisecond'
		  FROM generate_series(1, 250) g`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT id::text FROM context_blocks WHERE title LIKE 'po-k-%' ORDER BY created_at`)
	if err != nil {
		t.Fatal(err)
	}
	want, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(want) != 250 {
		t.Fatalf("fixture: %d waiting blocks (%v)", len(want), err)
	}
	for i, w := range [][]string{want[:100], want[100:200], want[200:]} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		blocks, err := pickPending(ctx, tx, types, guardPickLimit)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, b := range blocks {
			got = append(got, b.id)
		}
		if !slices.Equal(got, w) {
			t.Errorf("run %d while the runs before it hold their batches took %d blocks, want the next %d in created_at order", i+1, len(got), len(w))
		}
	}
}

// TestGuardPickIndexProbe: the pick reads idx_guard_pending_type only while it
// stands as migration 182 builds it — the probe holds on a fresh database,
// fails for an INVALID index and for another index under the name, and the
// pick then takes the same blocks with its statement before 182.
func TestGuardPickIndexProbe(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	if err := reg.Reload(ctx, pool); err != nil {
		t.Fatal(err)
	}
	types := reg.Snapshot().GuardCheckTypes()
	if _, err := pool.Exec(ctx, `INSERT INTO context_blocks (category, title, content, scope, type_name, embedding, embed_model, created_at, updated_at)
		SELECT 'po', 'po-p-' || g, 'waiting ' || g, 'po', CASE WHEN g % 2 = 0 THEN 'knowledge' ELSE 'reference' END,
		       array_fill(0.01::real, ARRAY[1024])::vector(1024), 'q360',
		       timestamptz '2026-11-01 00:00:00+00' + g * interval '1 millisecond',
		       timestamptz '2026-11-01 00:00:00+00' + g * interval '1 millisecond'
		  FROM generate_series(1, 5) g`); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT id::text FROM context_blocks WHERE title LIKE 'po-p-%' ORDER BY created_at LIMIT 3`)
	if err != nil {
		t.Fatal(err)
	}
	want, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	probe := func(label string, wantReady bool) {
		t.Helper()
		var ready bool
		if err := pool.QueryRow(ctx, guardPickIndexReadySQL).Scan(&ready); err != nil {
			t.Fatal(err)
		}
		if ready != wantReady {
			t.Errorf("%s: the probe says ready=%v, want %v", label, ready, wantReady)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		blocks, err := pickPending(ctx, tx, types, 3)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		var got []string
		for _, b := range blocks {
			got = append(got, b.id)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: the pick took %v, want the three oldest %v", label, got, want)
		}
	}
	probe("the index of 182", true)
	if _, err := pool.Exec(ctx, `UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'idx_guard_pending_type'::regclass`); err != nil {
		t.Fatal(err)
	}
	probe("an INVALID index", false)
	for _, s := range []string{`DROP INDEX idx_guard_pending_type`, `CREATE INDEX idx_guard_pending_type ON context_blocks (type_name, created_at)`} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	probe("another index under the name", false)
}
