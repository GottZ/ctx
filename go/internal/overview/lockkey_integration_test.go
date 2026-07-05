//go:build integration

package overview_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/overview"
	"github.com/GottZ/ctx/internal/testdb"
)

// tryLock opens its OWN transaction and takes pg_try_advisory_xact_lock(key).
// The tx is returned so the lock stays held until the caller rolls back —
// exactly the persist lifetime semantics.
func tryLock(t *testing.T, pool *pgxpool.Pool, key int64) (pgx.Tx, bool) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	var locked bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, key).Scan(&locked); err != nil {
		t.Fatalf("advisory lock: %v", err)
	}
	return tx, locked
}

// TestLockKey_PartitionsPersistInParallel is the B-W4 gate on a real PG:
// two DIFFERENT tenants' persist runs hold their locks concurrently, while
// the SAME tenant keeps serializing (second taker skips — the pre-B semantics
// scoped down to one partition).
func TestLockKey_PartitionsPersistInParallel(t *testing.T) {
	pool := testdb.SetupTestDB(t)

	keyA := overview.LockKeyForScopes([]string{"private", "shared", "work"})
	keyB := overview.LockKeyForScopes([]string{"tenant-b"})
	if keyA == keyB {
		t.Fatalf("distinct scope sets share a lock key: %#x", keyA)
	}

	// Tenant A persists…
	_, lockedA := tryLock(t, pool, keyA)
	if !lockedA {
		t.Fatal("tenant A could not take its own lock on an idle DB")
	}
	// …tenant B persists AT THE SAME TIME (the B-W4 point: pre-B-W4 both
	// used the base key and one of them would skip).
	_, lockedB := tryLock(t, pool, keyB)
	if !lockedB {
		t.Fatal("tenant B blocked by tenant A — lock keys not partitioned (B1-M1)")
	}
	// A second run of tenant A must skip while the first still holds the tx.
	_, lockedA2 := tryLock(t, pool, keyA)
	if lockedA2 {
		t.Fatal("second tenant-A run took the lock concurrently — same-partition serialization broken")
	}
}
