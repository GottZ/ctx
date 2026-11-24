//go:build integration

package store_test

// Issue #277: the write-log retention (scheduler.writelog_retention_days,
// store.EvictWriteLog). The tests hold it to its four promises: 0 deletes
// nothing; a run deletes exactly the rows past the horizon, in batches whose
// cost does not grow with the table; it never waits for a row lock and holds
// a writer for one batch at most; two ctxd instances delete disjoint rows
// without waiting for each other.
//
// The rows are written in the shape of the guard's audit row
// (guard.writeAuditLog) with an explicit created_at: every production writer
// of the write log stamps now(), so no write path produces a row of an age
// past a horizon. The blocks they reference are written through
// store.UpsertBlock.
//
//	go test -tags=integration ./internal/store/ -run 'TestEvictWriteLog' -count=1 -v

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// wlBlock writes one block through the production write path.
func wlBlock(t *testing.T, pool *pgxpool.Pool, title string) string {
	t.Helper()
	b, err := store.UpsertBlock(context.Background(), pool, "decisions", title, "content of "+title, nil, nil, "private", true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("upsert block %s: %v", title, err)
	}
	return b.ID
}

// wlInsert writes n guard-shaped write-log rows referencing block (nil: none)
// with created_at = now() - age - i ms for i in 1..n (distinct, ordered).
// age "" writes created_at NULL.
func wlInsert(t *testing.T, pool *pgxpool.Pool, n int, age string, block *string) {
	t.Helper()
	created := "now() - NULLIF($3::text, '')::interval - make_interval(secs => g / 1000.0)"
	_, err := pool.Exec(context.Background(), `
		INSERT INTO context_write_log
		       (block_id, matched_block_id, decision, similarity, scope, block_title, block_category, metadata, created_at)
		SELECT $2::uuid, $2::uuid, 'clean', 0.42, 'private', 'title ' || g, 'decisions',
		       '{"threshold_duplicate": 0.95, "threshold_review": 0.85}'::jsonb, `+created+`
		  FROM generate_series(1, $1::int) g`, n, block, age)
	if err != nil {
		t.Fatalf("insert %d write-log rows (age %q): %v", n, age, err)
	}
}

func wlCount(t *testing.T, pool *pgxpool.Pool, where string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM context_write_log WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatalf("count write log %s: %v", where, err)
	}
	return n
}

// wlConn is a pool of one connection on the test database: a transaction the
// test holds open.
func wlConn(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// TestEvictWriteLogRemovesOnlyRowsPastTheHorizon: 0 deletes nothing; a
// horizon of two days deletes the rows older than it and nothing else, rows
// without a created_at included; a run past one batch deletes every row past
// the horizon, also a tie group (one guard batch's stamp) larger than a batch.
func TestEvictWriteLogRemovesOnlyRowsPastTheHorizon(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	blk := wlBlock(t, pool, "wl-horizon")

	wlInsert(t, pool, 3, "1 hour", &blk)
	wlInsert(t, pool, 3, "47 hours", &blk)
	wlInsert(t, pool, 3, "49 hours", &blk)
	wlInsert(t, pool, 3, "10 days", nil)
	wlInsert(t, pool, 1, "", nil)

	n, err := store.EvictWriteLog(ctx, pool, 0)
	if err != nil || n != 0 {
		t.Fatalf("retention 0: deleted %d (err %v), want 0: the default keeps every row", n, err)
	}
	if got := wlCount(t, pool, "true"); got != 13 {
		t.Fatalf("retention 0 left %d rows, want 13", got)
	}

	n, err = store.EvictWriteLog(ctx, pool, 2)
	if err != nil || n != 6 {
		t.Fatalf("retention 2: deleted %d (err %v), want the 6 rows older than 48 h", n, err)
	}
	if got := wlCount(t, pool, "created_at < now() - interval '48 hours'"); got != 0 {
		t.Errorf("%d rows past the horizon left", got)
	}
	if got := wlCount(t, pool, "created_at >= now() - interval '48 hours'"); got != 6 {
		t.Errorf("%d rows inside the horizon left, want 6", got)
	}
	if got := wlCount(t, pool, "created_at IS NULL"); got != 1 {
		t.Errorf("%d rows without a created_at left, want 1", got)
	}

	// Past one batch: 2.5 batches of distinct times beside 5 000 rows inside.
	past := store.WriteLogRetentionBatch*2 + store.WriteLogRetentionBatch/2
	wlInsert(t, pool, past, "5 days", nil)
	wlInsert(t, pool, 5000, "1 hour", nil)
	// A tie group: one stamp for more rows than a batch holds.
	tie := store.WriteLogRetentionBatch + 2000
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_write_log (decision, scope, block_title, created_at)
		SELECT 'clean', 'private', 'tie ' || g, now() - interval '4 days'
		  FROM generate_series(1, $1::int) g`, tie); err != nil {
		t.Fatalf("insert tie group: %v", err)
	}
	n, err = store.EvictWriteLog(ctx, pool, 2)
	if err != nil || n != int64(past+tie) {
		t.Fatalf("retention 2 over %d + %d rows past the horizon: deleted %d (err %v)", past, tie, n, err)
	}
	if got := wlCount(t, pool, "true"); got != 6+1+5000 {
		t.Errorf("%d rows left, want %d", got, 6+1+5000)
	}
}

// wlKeysetRange reports whether an EXPLAIN JSON plan reads
// idx_write_log_created with both bounds of the batch as its index
// condition: created_at >= the keyset and < the horizon.
func wlKeysetRange(plan string) bool {
	var doc []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(plan), &doc); err != nil || len(doc) != 1 {
		return false
	}
	var walk func(n map[string]any) bool
	walk = func(n map[string]any) bool {
		if n["Index Name"] == "idx_write_log_created" {
			cond, _ := n["Index Cond"].(string)
			if strings.Contains(cond, "created_at < ") && strings.Contains(cond, "created_at >= ") {
				return true
			}
		}
		kids, _ := n["Plans"].([]any)
		for _, k := range kids {
			if m, ok := k.(map[string]any); ok && walk(m) {
				return true
			}
		}
		return false
	}
	return walk(doc[0].Plan)
}

// TestEvictWriteLogBatchCostIndependentOfTableSize: a batch reads the same
// buffers beside 10 000 and beside 100 000 rows inside the horizon, and the
// third batch of a run (after two batches whose dead index entries are still
// there, no vacuum between) reads no more than the first: the selection
// starts at the keyset, not at the oldest entry of the index, whose dead
// entries the next scan would visit on the heap again (measured: about five
// buffers per deleted row at either size, so the bound is 5 %) — also beside
// an open snapshot, under which no scan may mark those entries dead — and
// reads the index as the range from its keyset to the horizon. Custom and
// generic plan, both random_page_cost values.
func TestEvictWriteLogBatchCostIndependentOfTableSize(t *testing.T) {
	ctx := context.Background()
	type cell struct{ first, third int64 }
	measure := func(inside int) map[string]cell {
		pool := testdb.SetupTestDB(t)
		wlInsert(t, pool, inside, "1 hour", nil)
		out := map[string]cell{}
		for _, rpc := range testdb.RandomPageCosts {
			p := testdb.WithRandomPageCost(t, pool, rpc)
			for _, mode := range []string{"custom", "generic", "custom beside a snapshot"} {
				past := 3 * store.WriteLogRetentionBatch
				// In an order of their own: the heap order of the rows is not
				// their order in idx_write_log_created, as on a store whose
				// freed pages new rows refill, so consecutive index entries
				// lie on other heap pages.
				if _, err := pool.Exec(ctx, `
					INSERT INTO context_write_log (decision, similarity, scope, block_title, block_category, metadata, created_at)
					SELECT 'clean', 0.42, 'private', 'title ' || g, 'decisions', '{"threshold_duplicate": 0.95}'::jsonb,
					       now() - interval '5 days' - make_interval(secs => random() * 86400)
					  FROM generate_series(1, $1::int) g`, past); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, `VACUUM (ANALYZE) context_write_log`); err != nil {
					t.Fatal(err)
				}
				// A transaction with an older snapshot open beside the run (a
				// backup, a long report): the entries a batch deletes stay
				// visible to it, so no later scan may mark them dead, and a
				// selection that started at the oldest entry would visit every
				// row the run deleted before on the heap again.
				var snap pgx.Tx
				if mode == "custom beside a snapshot" {
					var err error
					if snap, err = pool.Begin(ctx); err != nil {
						t.Fatal(err)
					}
					if _, err := snap.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ`); err != nil {
						t.Fatal(err)
					}
					if _, err := snap.Exec(ctx, `SELECT count(*) FROM context_write_log`); err != nil {
						t.Fatal(err)
					}
				}
				var horizon time.Time
				if err := pool.QueryRow(ctx, `SELECT now() - interval '2 days'`).Scan(&horizon); err != nil {
					t.Fatal(err)
				}
				// The keysets EvictWriteLog passes: created_at of the last row
				// of each batch (the times are distinct).
				keys := make([]time.Time, 3)
				for i := 1; i < 3; i++ {
					if err := pool.QueryRow(ctx, `SELECT created_at FROM context_write_log WHERE created_at < $1
						ORDER BY created_at OFFSET $2 LIMIT 1`, horizon, i*store.WriteLogRetentionBatch-1).Scan(&keys[i]); err != nil {
						t.Fatal(err)
					}
				}
				var costs [3]testdb.PlanCost
				for i := 0; i < 3; i++ {
					args := []any{horizon, keys[i], store.WriteLogRetentionBatch}
					if mode != "generic" {
						costs[i] = testdb.ExplainCustom(t, p, store.WriteLogEvictBatchSQLForTest, args...)
					} else {
						costs[i] = testdb.ExplainGeneric(t, p, store.WriteLogEvictBatchSQLForTest, args...)
					}
					// A batch deletes WriteLogRetentionBatch rows, no more.
					if got, want := wlCount(t, pool, "created_at < $1", horizon), past-(i+1)*store.WriteLogRetentionBatch; got != want {
						t.Fatalf("rpc %s %s: %d rows past the horizon after batch %d, want %d", rpc, mode, got, i+1, want)
					}
					// About five to six buffers per deleted row (measured
					// 50 010-60 575 per batch of 10 000): the bound is seven.
					if bound := int64(7*store.WriteLogRetentionBatch + 500); costs[i].Buffers > bound {
						t.Errorf("rpc %s %s: batch %d read %d buffers, more than %d", rpc, mode, i+1, costs[i].Buffers, bound)
					}
					// The selection is a range of the index from the keyset to
					// the horizon: a batch starts where the one before ended,
					// not at the entries the run has deleted (each costs a
					// leaf read, and beside an older snapshot a heap visit,
					// once per later batch; measured: 94 070 buffers for a
					// first batch passing 700 000 such entries, against 40 156).
					if !wlKeysetRange(costs[i].Plan) {
						t.Errorf("rpc %s %s: batch %d does not read idx_write_log_created as the range [keyset, horizon):\n%s", rpc, mode, i+1, costs[i].Plan)
					}
				}
				if snap != nil {
					_ = snap.Rollback(ctx)
				}
				if got := wlCount(t, pool, "created_at < $1", horizon); got != 0 {
					t.Fatalf("rpc %s %s: %d rows past the horizon left after three batches", rpc, mode, got)
				}
				k := "rpc " + rpc + " " + mode
				out[k] = cell{costs[0].Buffers, costs[2].Buffers}
				t.Logf("inside %d, %s: batch 1 %s, batch 2 %s, batch 3 %s", inside, k, costs[0], costs[1], costs[2])
			}
		}
		return out
	}
	small := measure(10000)
	large := measure(100000)
	for k, s := range small {
		l := large[k]
		for _, c := range []struct {
			name  string
			got   int64
			bound int64
		}{
			{"first batch beside 100 000 rows", l.first, s.first + s.first/20 + 50},
			{"third batch beside 10 000 rows", s.third, s.first + s.first/20 + 50},
			{"third batch beside 100 000 rows", l.third, l.first + l.first/20 + 50},
		} {
			if c.got > c.bound {
				t.Errorf("%s, %s: %d buffers, more than %d (first batch beside 10 000 rows: %d, beside 100 000: %d)",
					k, c.name, c.got, c.bound, s.first, l.first)
			}
		}
	}
}

// TestEvictWriteLogSkipsRowsAnotherTransactionHolds: rows a block delete
// beside it holds (the SET NULL of its foreign keys) and rows a batch of the
// same retention on another instance holds are passed over, not waited for:
// the run finishes while both transactions are open, deletes every other row
// past the horizon, and the held rows go with the next run (the delete's) or
// with the transaction that holds them (the other instance's batch). No row
// is deleted twice: the counts add up to the rows past the horizon.
func TestEvictWriteLogSkipsRowsAnotherTransactionHolds(t *testing.T) {
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	held := wlBlock(t, pool, "wl-held")
	wlInsert(t, pool, 100, "6 days", &held)
	wlInsert(t, pool, 3*store.WriteLogRetentionBatch, "5 days", nil)
	wlInsert(t, pool, 50, "1 hour", nil)
	pastRows := 100 + 3*store.WriteLogRetentionBatch

	// A block delete beside the retention: its SET NULL locks the 100 rows.
	del := wlConn(t, dsn)
	dtx, err := del.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dtx.Rollback(ctx) }()
	if _, err := dtx.Exec(ctx, `DELETE FROM context_blocks WHERE id = $1`, held); err != nil {
		t.Fatalf("block delete: %v", err)
	}
	// A batch of another instance, held open after its statement: 10 000 rows.
	other := wlConn(t, dsn)
	otx, err := other.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = otx.Rollback(ctx) }()
	var horizon time.Time
	if err := otx.QueryRow(ctx, `SELECT now() - interval '2 days'`).Scan(&horizon); err != nil {
		t.Fatal(err)
	}
	var otherN int64
	var otherLast *time.Time
	bctx, bcancel := context.WithTimeout(ctx, 20*time.Second)
	err = otx.QueryRow(bctx, store.WriteLogEvictBatchSQLForTest, horizon, time.Time{}, store.WriteLogRetentionBatch).Scan(&otherN, &otherLast)
	bcancel()
	if err != nil {
		t.Fatalf("held batch beside the block delete: %v — it waited for the delete's rows", err)
	}
	if otherN != store.WriteLogRetentionBatch {
		t.Fatalf("held batch deleted %d rows, want %d", otherN, store.WriteLogRetentionBatch)
	}

	// The run on this instance must not wait for either: a wait would last
	// until the deadline, which neither transaction reaches.
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	n, err := store.EvictWriteLog(rctx, pool, 2)
	cancel()
	if err != nil {
		t.Fatalf("run beside the held rows: %v (deleted %d) — it waited for a row lock", err, n)
	}
	// What the other batch took (10 000 rows, the oldest it could lock, which
	// excludes the 100 the delete holds) and the 100 held rows are left.
	if want := int64(pastRows - 100 - store.WriteLogRetentionBatch); n != want {
		t.Fatalf("run deleted %d rows, want %d (every row past the horizon but the held ones)", n, want)
	}

	if err := otx.Commit(ctx); err != nil {
		t.Fatalf("commit the other instance's batch: %v", err)
	}
	if err := dtx.Commit(ctx); err != nil {
		t.Fatalf("commit the block delete: %v", err)
	}
	n2, err := store.EvictWriteLog(ctx, pool, 2)
	if err != nil || n2 != 100 {
		t.Fatalf("next run: deleted %d (err %v), want the 100 rows the block delete held", n2, err)
	}
	if total := otherN + n + n2; total != int64(pastRows) {
		t.Errorf("deleted %d rows in all, want %d: a row was deleted twice or missed", total, pastRows)
	}
	if got := wlCount(t, pool, "created_at < $1", horizon); got != 0 {
		t.Errorf("%d rows past the horizon left", got)
	}
	if got := wlCount(t, pool, "true"); got != 50 {
		t.Errorf("%d rows left, want the 50 inside the horizon", got)
	}
}

// TestEvictWriteLogHoldsAWriterForOneBatchAtMost: while a batch is in
// progress (its statement done, its transaction not committed), a write-log
// insert of the guard's shape goes through at once, a block delete whose
// SET NULL reaches a row outside the batch goes through at once, and a block
// delete whose SET NULL reaches a row of the batch waits for exactly that
// transaction and goes through as soon as it commits.
func TestEvictWriteLogHoldsAWriterForOneBatchAtMost(t *testing.T) {
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	inBatch := wlBlock(t, pool, "wl-in-batch")
	outside := wlBlock(t, pool, "wl-outside")
	wlInsert(t, pool, 10, "9 days", &inBatch)
	wlInsert(t, pool, store.WriteLogRetentionBatch, "8 days", nil)
	wlInsert(t, pool, 10, "1 hour", &outside)

	batch := wlConn(t, dsn)
	btx, err := batch.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = btx.Rollback(ctx) }()
	var pid uint32
	if err := btx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	var n int64
	var last *time.Time
	if err := btx.QueryRow(ctx, store.WriteLogEvictBatchSQLForTest,
		time.Now().Add(-48*time.Hour), time.Time{}, store.WriteLogRetentionBatch).Scan(&n, &last); err != nil {
		t.Fatalf("batch: %v", err)
	}
	if n != store.WriteLogRetentionBatch {
		t.Fatalf("batch deleted %d rows, want %d", n, store.WriteLogRetentionBatch)
	}

	quick, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := pool.Exec(quick, `
		INSERT INTO context_write_log (block_id, matched_block_id, decision, similarity, scope, block_title, block_category, metadata)
		VALUES ($1::uuid, NULL, 'clean', 0.5, 'private', 't', 'decisions', '{}'::jsonb)`, outside); err != nil {
		t.Fatalf("a write-log insert beside the batch: %v — it waited", err)
	}
	if _, err := pool.Exec(quick, `DELETE FROM context_blocks WHERE id = $1`, outside); err != nil {
		t.Fatalf("a block delete of rows outside the batch: %v — it waited", err)
	}

	done := make(chan struct{})
	var delErr error
	go func() {
		defer close(done)
		_, delErr = pool.Exec(ctx, `DELETE FROM context_blocks WHERE id = $1`, inBatch)
	}()
	testdb.WaitBlockedBy(t, pool, pid, done, "the block delete of rows in the batch", "transactionid")
	if err := btx.Commit(ctx); err != nil {
		t.Fatalf("commit the batch: %v", err)
	}
	select {
	case <-done:
		if delErr != nil {
			t.Fatalf("block delete after the batch: %v", delErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the block delete still waits after the batch committed")
	}
	if got := wlCount(t, pool, "block_id = $1", inBatch); got != 0 {
		t.Errorf("%d rows still name the deleted block", got)
	}
}
