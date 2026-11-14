//go:build integration

package store_test

// Issue #266: the reconcile (store.ReconcileBlockCounts) against concurrent
// writers and against drift the triggers cannot see. The gate helpers
// (assertCountParity, counterRows) live in block_counts_parity_integration_test.go.

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

// TestBlockCounts_ConcurrentWriters: parallel writers into ONE scope and ONE
// category (the hot counter row) plus a reconcile running alongside. Every
// write must succeed (no deadlock, no serialization failure on the writer
// side), the counters must equal the real count at the end, and a final
// reconcile must find nothing to correct — a reconcile that raced the writers
// did not inject drift.
func TestBlockCounts_ConcurrentWriters(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := spSetup(t)
	ctx, pool := f.ctx, f.pool
	s := f.scope(t, "cnt-race")
	other := f.scope(t, "cnt-race-other")
	const writers, perWriter = 8, 30

	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter+64)
	stop := make(chan struct{})
	reconciles := 0
	var reconcileMu sync.Mutex
	reconcileDone := make(chan struct{})
	go func() {
		defer close(reconcileDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := store.ReconcileBlockCounts(ctx, pool, s); err != nil {
				errs <- fmt.Errorf("reconcile: %w", err)
				return
			}
			reconcileMu.Lock()
			reconciles++
			reconcileMu.Unlock()
		}
	}()
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				title := fmt.Sprintf("w%d-%d", w, i)
				b, err := store.UpsertBlock(ctx, pool, "hot", title, "body "+title, nil, nil, s, true, store.SensitivityWrite{}, "")
				if err != nil {
					errs <- fmt.Errorf("upsert %s: %w", title, err)
					continue
				}
				switch i % 3 {
				case 1:
					if _, err := store.DeleteBlock(ctx, pool, b.ID, []string{s}); err != nil {
						errs <- fmt.Errorf("archive %s: %w", title, err)
					}
				case 2:
					to := other
					if _, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Scope: &to}, []string{s, other}); err != nil {
						errs <- fmt.Errorf("move %s: %w", title, err)
					}
				}
			}
		}(w)
	}
	wg.Wait()
	close(stop)
	<-reconcileDone
	close(errs)
	var msgs []string
	for err := range errs {
		msgs = append(msgs, err.Error())
	}
	if len(msgs) > 0 {
		sort.Strings(msgs)
		t.Fatalf("%d concurrent writes failed: %s", len(msgs), strings.Join(msgs, "; "))
	}
	reconcileMu.Lock()
	t.Logf("reconcile passes alongside the writers: %d", reconciles)
	reconcileMu.Unlock()
	assertCountParity(t, pool, "concurrent", [][]string{{s}, {other}, {s, other}})
	res, err := store.ReconcileBlockCounts(ctx, pool, "")
	if err != nil {
		t.Fatalf("final reconcile: %v", err)
	}
	if len(res.Corrections) != 0 {
		t.Fatalf("final reconcile corrected %+v — the counters drifted under concurrency", res.Corrections)
	}
}

// TestBlockCounts_ReconcileRepairsDrift: writes the triggers cannot see
// (DISABLE TRIGGER, the pg_restore --disable-triggers shape) leave drift; the
// reconcile finds it per key, reports counted/actual, corrects it, and prunes
// zero rows.
func TestBlockCounts_ReconcileRepairsDrift(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := spSetup(t)
	ctx, pool := f.ctx, f.pool
	s := f.scope(t, "cnt-drift")
	f.block(t, spBystander, "bystander", nil)
	for i := 0; i < 3; i++ {
		f.block(t, s, fmt.Sprintf("d%d", i), nil)
	}
	gone := f.block(t, s, "gone", nil)
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET category = 'solo' WHERE id = $1::uuid`, gone); err != nil {
		t.Fatal(err)
	}
	// Invisible writes: two more blocks, and the 'solo' block disappears.
	if _, err := pool.Exec(ctx, `ALTER TABLE context_blocks DISABLE TRIGGER trg_block_counts_ins`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE context_blocks DISABLE TRIGGER trg_block_counts_del`); err != nil {
		t.Fatal(err)
	}
	f.block(t, s, "hidden-1", nil)
	f.block(t, s, "hidden-2", nil)
	if _, err := pool.Exec(ctx, `DELETE FROM context_blocks WHERE id = $1::uuid`, gone); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE context_blocks ENABLE TRIGGER trg_block_counts_ins`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE context_blocks ENABLE TRIGGER trg_block_counts_del`); err != nil {
		t.Fatal(err)
	}
	if got, want := counterRows(t, pool, false), counterRows(t, pool, true); reflect.DeepEqual(got, want) {
		t.Fatalf("no drift injected: %v", got)
	}
	// A zero row to prune, keyed on guard_status NULL (the IS NOT DISTINCT
	// FROM path of the prune): a block passes through the unchecked state.
	z := f.block(t, s, "zero", nil)
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET guard_status = NULL WHERE id = $1::uuid`, z); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET guard_status = 'active' WHERE id = $1::uuid`, z); err != nil {
		t.Fatal(err)
	}

	res, err := store.ReconcileBlockCounts(ctx, pool, s)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	active := "active"
	want := []store.BlockCountDrift{
		{Scope: s, Category: "learnings", IsArchived: false, GuardStatus: &active, Counted: 4, Actual: 6},
		{Scope: s, Category: "solo", IsArchived: false, GuardStatus: &active, Counted: 1, Actual: 0},
	}
	if res.Scopes != 1 || !reflect.DeepEqual(res.Corrections, want) || res.Pruned != 1 {
		t.Fatalf("reconcile = %+v (corrections %+v), want 1 scope, corrections %+v, 1 pruned", *res, res.Corrections, want)
	}
	assertCountParity(t, pool, "after reconcile", [][]string{{s}})
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_block_counts WHERE scope = $1`, s).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("counter rows of %s = %d (%v), want 1 (the zero rows pruned)", s, rows, err)
	}
	// A scope whose only block went past the triggers: no block left, one
	// counter row. Only the counter side of the scope enumeration finds it.
	empty := f.scope(t, "cnt-empty")
	e := f.block(t, empty, "lonely", nil)
	for _, sql := range []string{
		`ALTER TABLE context_blocks DISABLE TRIGGER trg_block_counts_del`,
		fmt.Sprintf(`DELETE FROM context_blocks WHERE id = '%s'::uuid`, e),
		`ALTER TABLE context_blocks ENABLE TRIGGER trg_block_counts_del`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// All scopes: s is clean now, the bystander was never off, the empty
	// scope is found through its counter row and corrected.
	res, err = store.ReconcileBlockCounts(ctx, pool, "")
	wantEmpty := []store.BlockCountDrift{{Scope: empty, Category: "learnings", GuardStatus: &active, Counted: 1, Actual: 0}}
	if err != nil || res.Scopes != 3 || !reflect.DeepEqual(res.Corrections, wantEmpty) {
		t.Fatalf("all-scope reconcile = %+v (%v), %v; want 3 scopes and %+v", res, res.Corrections, err, wantEmpty)
	}
	assertCountParity(t, pool, "after the all-scope reconcile", [][]string{{s}, {empty}, {spBystander}})
}

// TestBlockCounts_NoDeadlockWithGuardRow: the lock order of migration 170. A
// transaction that changes a counter key through an update the guard trigger
// skips (metadata-only, guard_status already in the metadata — the guard's
// re-check of an edited block) and later needs the guard row (a first-time
// check of another block), against a concurrent insert into the same key,
// which takes the guard row first. If the counter rows were taken without the
// guard row, the two would wait on each other and one would fail with 40P01;
// with the guard row taken before the first counter row, the insert simply
// waits for the commit.
func TestBlockCounts_NoDeadlockWithGuardRow(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := spSetup(t)
	ctx, pool := f.ctx, f.pool
	s := f.scope(t, "cnt-order")
	rechecked := f.block(t, s, "rechecked", nil)
	fresh := f.block(t, s, "fresh", nil)
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET metadata = metadata || '{"guard_status": "active"}'::jsonb WHERE id = $1::uuid`, rechecked); err != nil {
		t.Fatal(err)
	}

	t1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = t1.Rollback(ctx) }()
	if _, err := t1.Exec(ctx, `SET LOCAL lock_timeout = '10s'`); err != nil {
		t.Fatal(err)
	}
	// The skip path of mark_guard_dirty: guard_status in OLD and NEW metadata,
	// content unchanged. The key moves active -> clean.
	if _, err := t1.Exec(ctx, `UPDATE context_blocks SET guard_status = 'clean',
	        metadata = metadata || '{"guard_status": "clean"}'::jsonb WHERE id = $1::uuid`, rechecked); err != nil {
		t.Fatalf("re-check update: %v", err)
	}
	var t1PID int
	if err := t1.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&t1PID); err != nil {
		t.Fatal(err)
	}

	insertErr := make(chan error, 1)
	go func() {
		_, err := store.UpsertBlock(ctx, pool, "learnings", "concurrent", "concurrent body", nil, nil, s, true, store.SensitivityWrite{}, "")
		insertErr <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, t1PID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the insert never waited on the re-check transaction")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A first-time check: mark_guard_dirty updates the guard row.
	if _, err := t1.Exec(ctx, `UPDATE context_blocks SET guard_status = 'clean',
	        metadata = metadata || '{"guard_status": "clean"}'::jsonb WHERE id = $1::uuid`, fresh); err != nil {
		t.Fatalf("first-time check in the same transaction: %v", err)
	}
	if err := t1.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := <-insertErr; err != nil {
		t.Fatalf("concurrent insert: %v", err)
	}
	assertCountParity(t, pool, "after the re-check and the insert", [][]string{{s}})
}
