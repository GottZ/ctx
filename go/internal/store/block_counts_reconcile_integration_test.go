//go:build integration

package store_test

// Issue #266: the reconcile (store.ReconcileBlockCounts) against concurrent
// writers and against drift the triggers cannot see. The gate helpers
// (assertCountParity, counterRows) live in block_counts_parity_integration_test.go.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
)

// TestBlockCounts_ConcurrentWriters: parallel writers into ONE scope and ONE
// category (one counter key) plus a reconcile and a fold running alongside. Every
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
			if _, err := store.FoldBlockCountDeltas(ctx, pool); err != nil {
				errs <- fmt.Errorf("fold: %w", err)
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

// fold runs one delta fold or fails the test.
func fold(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := store.FoldBlockCountDeltas(context.Background(), pool); err != nil {
		t.Fatalf("fold: %v", err)
	}
}

// execAll runs statements in order or fails the test.
func execAll(t *testing.T, pool *pgxpool.Pool, sqls ...string) {
	t.Helper()
	for _, sql := range sqls {
		if _, err := pool.Exec(context.Background(), sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}

// TestBlockCounts_ReconcileRepairsDrift: writes the triggers cannot see
// (DISABLE TRIGGER, the pg_restore --disable-triggers shape) leave drift; the
// reconcile finds it per key, reports counted/actual, corrects it with delta
// rows, and prunes folded zero rows.
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
	execAll(t, pool, fmt.Sprintf(`UPDATE context_blocks SET category = 'solo' WHERE id = '%s'::uuid`, gone))
	// Invisible writes: two more blocks, and the 'solo' block disappears.
	execAll(t, pool, `ALTER TABLE context_blocks DISABLE TRIGGER trg_block_counts_ins`,
		`ALTER TABLE context_blocks DISABLE TRIGGER trg_block_counts_del`)
	f.block(t, s, "hidden-1", nil)
	f.block(t, s, "hidden-2", nil)
	execAll(t, pool, fmt.Sprintf(`DELETE FROM context_blocks WHERE id = '%s'::uuid`, gone),
		`ALTER TABLE context_blocks ENABLE TRIGGER trg_block_counts_ins`,
		`ALTER TABLE context_blocks ENABLE TRIGGER trg_block_counts_del`)
	if got, want := counterRows(t, pool, false), counterRows(t, pool, true); reflect.DeepEqual(got, want) {
		t.Fatalf("no drift injected: %v", got)
	}
	// A folded zero row to prune, keyed on guard_status NULL: a block passes
	// through the unchecked state with a fold on each side.
	z := f.block(t, s, "zero", nil)
	execAll(t, pool, fmt.Sprintf(`UPDATE context_blocks SET guard_status = NULL WHERE id = '%s'::uuid`, z))
	fold(t, pool)
	execAll(t, pool, fmt.Sprintf(`UPDATE context_blocks SET guard_status = 'active' WHERE id = '%s'::uuid`, z))
	fold(t, pool)

	res, err := store.ReconcileBlockCounts(ctx, pool, s)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	active := "active"
	want := []store.BlockCountDrift{
		{Scope: s, Category: "learnings", IsArchived: false, GuardStatus: &active, Counted: 4, Actual: 6},
		{Scope: s, Category: "solo", IsArchived: false, GuardStatus: &active, Counted: 1, Actual: 0},
	}
	if res.Scopes != 1 || !reflect.DeepEqual(res.Corrections, want) || res.Pruned != 1 || len(res.Failed) != 0 {
		t.Fatalf("reconcile = %+v (corrections %+v), want 1 scope, corrections %+v, 1 pruned", *res, res.Corrections, want)
	}
	assertCountParity(t, pool, "after reconcile", [][]string{{s}})
	// The 'solo' correction is a pending delta; folded, its row is a zero row
	// the next pass prunes.
	fold(t, pool)
	res, err = store.ReconcileBlockCounts(ctx, pool, s)
	if err != nil || len(res.Corrections) != 0 || res.Pruned != 1 {
		t.Fatalf("second pass = %+v, %v; want no correction and 1 pruned", res, err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_block_counts WHERE scope = $1`, s).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("counter rows of %s = %d (%v), want 1 (the zero rows pruned)", s, rows, err)
	}
	// A scope whose only block went past the triggers: no block left, one
	// folded counter row. Only the counter side of the scope enumeration finds it.
	empty := f.scope(t, "cnt-empty")
	e := f.block(t, empty, "lonely", nil)
	fold(t, pool)
	execAll(t, pool, `ALTER TABLE context_blocks DISABLE TRIGGER trg_block_counts_del`,
		fmt.Sprintf(`DELETE FROM context_blocks WHERE id = '%s'::uuid`, e),
		`ALTER TABLE context_blocks ENABLE TRIGGER trg_block_counts_del`)
	res, err = store.ReconcileBlockCounts(ctx, pool, "")
	wantEmpty := []store.BlockCountDrift{{Scope: empty, Category: "learnings", GuardStatus: &active, Counted: 1, Actual: 0}}
	if err != nil || res.Scopes != 3 || !reflect.DeepEqual(res.Corrections, wantEmpty) {
		t.Fatalf("all-scope reconcile = %+v (%v), %v; want 3 scopes and %+v", res, res.Corrections, err, wantEmpty)
	}
	assertCountParity(t, pool, "after the all-scope reconcile", [][]string{{s}, {empty}, {spBystander}})
}

// lockOrderCase runs the two-transaction shape of a lock-order cycle: T1 runs
// first (and holds what it took), T2 runs its statement in its own transaction
// and commits, T1 then runs second against the row T2 touched and commits.
// If T2's statement had to wait for something T1 holds after T2 locked its
// rows, T1's second statement would wait for T2 and PostgreSQL would break the
// cycle with 40P01. Both sides must finish without error.
func lockOrderCase(t *testing.T, pool *pgxpool.Pool, first, t2Stmt, second string, args ...any) {
	t.Helper()
	ctx := context.Background()
	t1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = t1.Rollback(ctx) }()
	if _, err := t1.Exec(ctx, first, args...); err != nil {
		t.Fatalf("t1 first: %v", err)
	}
	var t1PID int
	if err := t1.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&t1PID); err != nil {
		t.Fatal(err)
	}
	t2Done := make(chan error, 1)
	go func() {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t2Done <- err
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, t2Stmt, args...); err != nil {
			t2Done <- fmt.Errorf("t2 statement: %w", err)
			return
		}
		t2Done <- tx.Commit(ctx)
	}()
	// Let T2 either finish or block on T1, then run T1's second statement.
	var t2Err error
	finished := false
	deadline := time.Now().Add(5 * time.Second)
	for !finished && time.Now().Before(deadline) {
		select {
		case t2Err = <-t2Done:
			finished = true
			continue
		default:
		}
		var waiting int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, t1PID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := t1.Exec(ctx, second, args...); err != nil {
		t.Fatalf("t1 second statement: %v", err)
	}
	if err := t1.Commit(ctx); err != nil {
		t.Fatalf("t1 commit: %v", err)
	}
	if !finished {
		t2Err = <-t2Done
	}
	if t2Err != nil {
		t.Fatalf("t2: %v", t2Err)
	}
}

// TestBlockCounts_NoDeadlockOnDelete is the shape of PR #269 finding 1: T1
// inserts a block (trg_guard_dirty gives it the context_guard_state row until
// its commit), T2 deletes block A (row lock on A), T1 then deletes A too. The
// counter maintenance of T2's delete must not wait for anything T1 holds —
// with a counter row or the guard row taken at the end of T2's statement, T1's
// delete would wait for T2 and the two would deadlock.
func TestBlockCounts_NoDeadlockOnDelete(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := spSetup(t)
	s := f.scope(t, "cnt-del-order")
	a := f.block(t, s, "a", nil)
	lockOrderCase(t, f.pool,
		`INSERT INTO context_blocks (category, title, content, scope) VALUES ('learnings', 'b-'||$1::text, 'x', 'sp-a:cnt-del-order')`,
		`DELETE FROM context_blocks WHERE id = $1::uuid`,
		`DELETE FROM context_blocks WHERE id = $1::uuid`, a)
	assertCountParity(t, f.pool, "after the delete pair", [][]string{{s}})
}

// TestBlockCounts_NoDeadlockOnGuardSkippedUpdate: the same shape with an
// update that trg_guard_dirty skips (metadata-only, guard_status in OLD and NEW
// metadata, content unchanged — the guard's re-check of an edited block) but
// that changes a counter key. Such an update never took the guard row before
// migration 170, so its counter maintenance must not take anything T1 holds.
func TestBlockCounts_NoDeadlockOnGuardSkippedUpdate(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := spSetup(t)
	s := f.scope(t, "cnt-upd-order")
	a := f.block(t, s, "rechecked", nil)
	execAll(t, f.pool, fmt.Sprintf(`UPDATE context_blocks SET metadata = metadata || '{"guard_status": "active"}'::jsonb WHERE id = '%s'::uuid`, a))
	lockOrderCase(t, f.pool,
		`INSERT INTO context_blocks (category, title, content, scope) VALUES ('learnings', 'b-'||$1::text, 'x', 'sp-a:cnt-upd-order')`,
		`UPDATE context_blocks SET guard_status = 'clean', metadata = metadata || '{"guard_status": "clean"}'::jsonb WHERE id = $1::uuid`,
		`UPDATE context_blocks SET guard_status = 'needs_review', metadata = metadata || '{"guard_status": "needs_review"}'::jsonb WHERE id = $1::uuid`, a)
	assertCountParity(t, f.pool, "after the update pair", [][]string{{s}})
}

// TestBlockCounts_TwoReconcilesCorrectOnce: two reconciles that both saw the
// same drift in their lock-free check meet at the scope's advisory lock; the
// second repeats the check after the first committed and corrects nothing.
func TestBlockCounts_TwoReconcilesCorrectOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := spSetup(t)
	ctx, pool := f.ctx, f.pool
	s := f.scope(t, "cnt-twice")
	f.block(t, s, "seen", nil)
	execAll(t, pool, `ALTER TABLE context_blocks DISABLE TRIGGER trg_block_counts_ins`)
	f.block(t, s, "unseen-1", nil)
	f.block(t, s, "unseen-2", nil)
	execAll(t, pool, `ALTER TABLE context_blocks ENABLE TRIGGER trg_block_counts_ins`)

	// Hold the scope's advisory lock so both reconciles pass their check and
	// queue at the lock.
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, store.BlockCountReconcileLockSQLForTest, s); err != nil {
		t.Fatal(err)
	}
	var holderPID int
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	results := make(chan *store.BlockCountReconcile, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			r, err := store.ReconcileBlockCounts(ctx, pool, s)
			results <- r
			errs <- err
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, holderPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of 2 reconciles reached the scope lock", waiting)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	total := 0
	for i := 0; i < 2; i++ {
		r := <-results
		if err := <-errs; err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		total += len(r.Corrections)
	}
	if total != 1 {
		t.Fatalf("the two reconciles made %d corrections, want exactly 1", total)
	}
	assertCountParity(t, pool, "after two reconciles", [][]string{{s}})
}

// TestBlockCounts_ReconcileGoesOnAfterAFailedScope (PR #269 finding 3): a scope
// whose reconcile fails (here: its advisory lock is held past the pool's
// lock_timeout) is named in Failed and in the joined error, and the scopes
// after it are still corrected.
func TestBlockCounts_ReconcileGoesOnAfterAFailedScope(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := spSetup(t)
	ctx, pool := f.ctx, f.pool
	first := f.scope(t, "cnt-fail-a")
	second := f.scope(t, "cnt-fail-b")
	execAll(t, pool, `ALTER TABLE context_blocks DISABLE TRIGGER trg_block_counts_ins`)
	f.block(t, first, "unseen-a", nil)
	f.block(t, second, "unseen-b", nil)
	execAll(t, pool, `ALTER TABLE context_blocks ENABLE TRIGGER trg_block_counts_ins`)

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, store.BlockCountReconcileLockSQLForTest, first); err != nil {
		t.Fatal(err)
	}
	cfg := pool.Config().Copy()
	cfg.ConnConfig.RuntimeParams["lock_timeout"] = "300ms"
	short, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer short.Close()

	res, err := store.ReconcileBlockCounts(ctx, short, "")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" || !strings.Contains(err.Error(), first) {
		t.Fatalf("reconcile error = %v, want the lock timeout (55P03) of %s", err, first)
	}
	if res == nil || !reflect.DeepEqual(res.Failed, []string{first}) {
		t.Fatalf("failed = %+v, want [%s]", res, first)
	}
	if len(res.Corrections) != 1 || res.Corrections[0].Scope != second {
		t.Fatalf("corrections = %+v, want the one of %s", res.Corrections, second)
	}
	got, err := store.GetStats(ctx, pool, []string{second})
	if err != nil {
		t.Fatal(err)
	}
	if ref := refStats(t, pool, []string{second}); got.TotalBlocks != ref.TotalBlocks {
		t.Fatalf("%s after the pass: %d blocks counted, %d real", second, got.TotalBlocks, ref.TotalBlocks)
	}
	// With the lock released the next pass takes the failed scope too.
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if res, err := store.ReconcileBlockCounts(ctx, pool, ""); err != nil || len(res.Failed) != 0 {
		t.Fatalf("next pass = %+v, %v", res, err)
	}
	assertCountParity(t, pool, "after the next pass", [][]string{{first}, {second}})
}

// TestBlockCounts_ReconcileAgainstTruncateAndReload (PR #269 finding 2): a
// transaction that truncates context_blocks and loads blocks again while a
// reconcile corrects drift. The reconcile holds only its scope's advisory lock
// and, for its statements, ACCESS SHARE on context_blocks; the reload's trigger
// writes append-only delta rows. Neither waits for something the other holds
// after taking its own lock, so both finish, and the counters match the
// reloaded table.
func TestBlockCounts_ReconcileAgainstTruncateAndReload(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := spSetup(t)
	ctx, pool := f.ctx, f.pool
	s := f.scope(t, "cnt-trunc")
	f.block(t, s, "before", nil)
	execAll(t, pool, `ALTER TABLE context_blocks DISABLE TRIGGER trg_block_counts_ins`)
	f.block(t, s, "unseen", nil)
	execAll(t, pool, `ALTER TABLE context_blocks ENABLE TRIGGER trg_block_counts_ins`)

	// The reconcile's correction transaction holds the scope lock first …
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, store.BlockCountReconcileLockSQLForTest, s); err != nil {
		t.Fatal(err)
	}
	reconciled := make(chan error, 1)
	go func() {
		_, err := store.ReconcileBlockCounts(ctx, pool, s)
		reconciled <- err
	}()
	time.Sleep(100 * time.Millisecond)
	// … while another transaction truncates and reloads.
	reload, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reload.Rollback(ctx) }()
	if _, err := reload.Exec(ctx, `SET LOCAL lock_timeout = '10s'`); err != nil {
		t.Fatal(err)
	}
	if _, err := reload.Exec(ctx, `TRUNCATE context_blocks CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, err := reload.Exec(ctx, `INSERT INTO context_blocks (category, title, content, scope)
		SELECT 'learnings', 'reloaded ' || g, 'x', $1 FROM generate_series(1, 5) g`, s); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := reload.Commit(ctx); err != nil {
		t.Fatalf("reload commit: %v", err)
	}
	if err := <-reconciled; err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res, err := store.ReconcileBlockCounts(ctx, pool, ""); err != nil {
		t.Fatalf("follow-up reconcile: %v", err)
	} else {
		t.Logf("follow-up reconcile: %+v", *res)
	}
	assertCountParity(t, pool, "after truncate and reload", [][]string{{s}})
}

// TestBlockCounts_PruneAgainstFold (PR #269, zero-row prune vs. fold): the
// fold upserts its keys in key order (a, b, z); the reconcile's prune deletes
// a scope's zero rows in scan order, which for a sequential scan is the order
// the rows were last written (here z, b, a). With b held by a third
// transaction, the prune takes z and queues at b, the fold takes a and queues
// at b; when b is released the prune takes it and wants a, the fold wants b —
// unless the two never run side by side. Sequential scans are forced on the
// reconcile's connections: a small counter table is scanned that way anyway.
func TestBlockCounts_PruneAgainstFold(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := spSetup(t)
	ctx, pool := f.ctx, f.pool
	s := f.scope(t, "cnt-prune")
	ids := map[string]string{}
	for _, c := range []string{"a", "b", "c", "z"} {
		b, err := store.UpsertBlock(ctx, pool, c, "row "+c, "x", nil, nil, s, true, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatal(err)
		}
		ids[c] = b.ID
	}
	fold(t, pool)
	// Zero rows, each brought to 0 by a fold of its own, in the order z, b, a, c.
	for _, c := range []string{"z", "b", "a", "c"} {
		execAll(t, pool, fmt.Sprintf(`DELETE FROM context_blocks WHERE id = '%s'::uuid`, ids[c]))
		fold(t, pool)
	}
	// Pending deltas for a, b and z; c stays a bare zero row, so the check
	// reports the scope and the correction prunes.
	for _, c := range []string{"a", "b", "z"} {
		if _, err := store.UpsertBlock(ctx, pool, c, "again "+c, "x", nil, nil, s, true, store.SensitivityWrite{}, ""); err != nil {
			t.Fatal(err)
		}
	}
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `SELECT 1 FROM context_block_counts WHERE scope = $1 AND category = 'b' FOR UPDATE`, s); err != nil {
		t.Fatal(err)
	}
	var holderPID int
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	waitWaiters := func(n int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			var waiting int
			if err := pool.QueryRow(ctx,
				`SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, holderPID).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting >= n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d of %d transactions queued at the held row", waiting, n)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	cfg := pool.Config().Copy()
	cfg.ConnConfig.RuntimeParams["enable_indexscan"] = "off"
	cfg.ConnConfig.RuntimeParams["enable_bitmapscan"] = "off"
	seq, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer seq.Close()
	reconciled := make(chan error, 1)
	go func() {
		_, err := store.ReconcileBlockCounts(ctx, seq, s)
		reconciled <- err
	}()
	waitWaiters(1)
	folded := make(chan error, 1)
	go func() {
		_, err := store.FoldBlockCountDeltas(ctx, pool)
		folded <- err
	}()
	// The fold either queues at b as well or finds the fold lock taken.
	select {
	case err := <-folded:
		folded <- err
	case <-time.After(300 * time.Millisecond):
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-reconciled; err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := <-folded; err != nil {
		t.Fatalf("fold: %v", err)
	}
	fold(t, pool)
	assertCountParity(t, pool, "after prune and fold", [][]string{{s}})
}
