//go:build integration

// lockBackfillPick (issue #239) takes the candidate of the pick and then its
// row lock. Two workers whose transactions overlap must end on two distinct
// blocks, the oldest two, each holding exactly one row lock: the second sees
// the first one's candidate held and picks again without it.
//
//	go test -tags=integration ./internal/events/ -run TestLockBackfillPick -count=1 -v
package events

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestLockBackfillPick(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	ids := make([]string, 3)
	for i := range ids {
		ids[i] = seedSensitiveBlock(t, pool, "lock pick "+string(rune('a'+i)), "public", "lp-owned", time.Duration(3-i)*time.Hour)
	}
	cl := store.NewPendingClasses()
	cl.PairScopes, cl.PairLevels = []string{"lp-owned"}, []string{"public"}

	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin 1: %v", err)
	}
	defer func() { _ = tx1.Rollback(ctx) }()
	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin 2: %v", err)
	}
	defer func() { _ = tx2.Rollback(ctx) }()
	// A worker that waited on a held row instead of skipping it would hang
	// here; the bound turns that into a failure.
	if _, err := tx2.Exec(ctx, `SET LOCAL lock_timeout = '2s'`); err != nil {
		t.Fatalf("lock_timeout: %v", err)
	}

	first, _, _, _, _, err := lockBackfillPick(ctx, tx1, cl)
	if err != nil || first != ids[0] {
		t.Fatalf("first worker: %q, %v; want the oldest %q", first, err, ids[0])
	}
	second, _, _, _, _, err := lockBackfillPick(ctx, tx2, cl)
	if err != nil || second != ids[1] {
		t.Fatalf("second worker: %q, %v; want the second oldest %q (the oldest is held)", second, err, ids[1])
	}
	var locked int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_blocks WHERE id = ANY($1::uuid[])
		   AND id NOT IN (SELECT id FROM context_blocks WHERE id = ANY($1::uuid[]) FOR UPDATE SKIP LOCKED)`,
		ids).Scan(&locked); err != nil {
		t.Fatalf("count held rows: %v", err)
	}
	if locked != 2 {
		t.Errorf("%d of the three blocks are held, want 2 (one row lock per worker)", locked)
	}
}
