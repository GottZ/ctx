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
	go func() {
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
	time.Sleep(50 * time.Millisecond)
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
	// A zero row to prune: a block in a fresh category that leaves it.
	z := f.block(t, s, "zero", nil)
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET category = 'zero-cat' WHERE id = $1::uuid`, z); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET category = 'learnings' WHERE id = $1::uuid`, z); err != nil {
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
	// All scopes: the scope enumeration finds it too, and a second pass is clean.
	res, err = store.ReconcileBlockCounts(ctx, pool, "")
	if err != nil || len(res.Corrections) != 0 || res.Scopes != 2 {
		t.Fatalf("second reconcile = %+v, %v; want 2 scopes and no correction", res, err)
	}
}
