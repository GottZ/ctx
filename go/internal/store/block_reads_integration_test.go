//go:build integration

// Issue #345 — the read record's insert against a block that is gone, or
// whose delete is in flight on another connection.
//
//	go test -tags=integration -p 1 ./internal/store/ -run TestBlockReads -count=1 -v
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

func br345Block(t *testing.T, pool *pgxpool.Pool, title string) string {
	t.Helper()
	b, err := store.UpsertBlock(context.Background(), pool, "br345", title, "body of "+title, nil, nil, "private", true,
		store.SensitivityWrite{Value: backends.SensPublic, Manual: true}, "checkpoint")
	if err != nil {
		t.Fatalf("upsert %s: %v", title, err)
	}
	return b.ID
}

func br345Count(t *testing.T, pool *pgxpool.Pool, blockID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_access_log WHERE action = $1 AND block_id = $2::uuid`, store.ActionMCPSearch, blockID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestBlockReadsSkipGoneAndDeleting_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	key, _, err := store.CreateApiKey(ctx, pool, "br345", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	live := br345Block(t, pool, "br345 live")
	gone := br345Block(t, pool, "br345 gone")
	deleting := br345Block(t, pool, "br345 deleting")
	settle := func() time.Duration {
		t.Helper()
		start := time.Now()
		sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if err := store.SettleBlockReads(sctx, pool); err != nil {
			t.Fatalf("settle: %v", err)
		}
		return time.Since(start)
	}

	// A block deleted between the read and the insert: its row is skipped,
	// the read next to it in the same statement is booked.
	if _, err := pool.Exec(ctx, `DELETE FROM context_blocks WHERE id = $1::uuid`, gone); err != nil {
		t.Fatalf("delete: %v", err)
	}
	failedBefore, _, skippedBefore := store.BlockReadsLost()
	store.RecordBlockReads(pool, key.ID, store.ActionMCPSearch, []string{gone, live})
	settle()
	if n := br345Count(t, pool, live); n != 1 {
		t.Errorf("live block next to a deleted one: %d rows, want 1", n)
	}
	failed, _, skipped := store.BlockReadsLost()
	if failed != failedBefore {
		t.Errorf("a deleted block failed the statement: failed rows %d → %d", failedBefore, failed)
	}
	if skipped != skippedBefore+1 {
		t.Errorf("the deleted block's row: skipped %d → %d, want +1 (counted, not silent)", skippedBefore, skipped)
	}

	// A delete in flight on a second connection holds the block FOR UPDATE.
	// The insert must neither wait for it nor fail on it: it skips that block
	// and books the other one at once.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM context_blocks WHERE id = $1::uuid`, deleting); err != nil {
		t.Fatalf("delete in flight: %v", err)
	}
	store.RecordBlockReads(pool, key.ID, store.ActionMCPSearch, []string{deleting, live})
	if d := settle(); d > 2*time.Second {
		t.Errorf("the insert waited %s behind a delete in flight, want no wait", d)
	}
	if n := br345Count(t, pool, live); n != 2 {
		t.Errorf("live block next to a deleting one: %d rows in total, want 2", n)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if n := br345Count(t, pool, deleting); n != 0 {
		t.Errorf("the block whose delete was in flight got %d rows, want 0 (skipped)", n)
	}

	// A picker holding a block FOR UPDATE (the embedding backfill, the dream
	// pick) is not a delete, and its block stays: the insert neither waits
	// for it nor loses the row silently — it skips and counts it.
	held := br345Block(t, pool, "br345 held")
	tx2, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx2.Rollback(ctx) }()
	if _, err := tx2.Exec(ctx, `SELECT id FROM context_blocks WHERE id = $1::uuid FOR UPDATE`, held); err != nil {
		t.Fatalf("hold FOR UPDATE: %v", err)
	}
	_, _, skippedBefore = store.BlockReadsLost()
	store.RecordBlockReads(pool, key.ID, store.ActionMCPSearch, []string{held, live})
	if d := settle(); d > 2*time.Second {
		t.Errorf("the insert waited %s behind a FOR UPDATE holder, want no wait", d)
	}
	if n := br345Count(t, pool, live); n != 3 {
		t.Errorf("live block next to a held one: %d rows in total, want 3", n)
	}
	if _, _, skipped := store.BlockReadsLost(); skipped != skippedBefore+1 {
		t.Errorf("the held block's row: skipped %d → %d, want +1", skippedBefore, skipped)
	}
	if err := tx2.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
}
