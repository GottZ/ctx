//go:build integration

// Issue #547 point 3: the stage delete of the tenant prune's key step relies
// on READ COMMITTED — a stage committed while the key delete waited for its
// key is seen only by the next statement's own snapshot — and a prune of a
// tenant without keys deletes no stage, so it must not touch (and lock) the
// hypertable at all.
package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestTenantPruneUnderRepeatableReadDefault: a stage written while the
// prune's key delete waits for its key must go with the key, also where the
// session default isolation level is REPEATABLE READ (a role or database
// setting): the stage delete relies on a snapshot of its own.
func TestTenantPruneUnderRepeatableReadDefault(t *testing.T) {
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	rr := testdb.WithSessionSettings(t, pool, map[string]string{"default_transaction_isolation": "repeatable read"})
	tn, err := store.CreateTenant(ctx, pool, "p547-rr", "rr")
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := store.CreateApiKey(ctx, pool, "p547-rr", "private", nil, tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	st := wlConn(t, dsn)
	tx, err := st.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var pid uint32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StagePendingWrite(ctx, tx, key.ID, "private", "store", "mcp", []byte(`{"op":"store"}`), "p547-rr", time.Minute); err != nil {
		t.Fatalf("stage: %v", err)
	}
	done := make(chan struct{})
	var pruneErr error
	go func() {
		defer close(done)
		pruneErr = store.PruneTenant(ctx, rr, tn.ID, store.AuditStamp{})
	}()
	testdb.WaitBlockedBy(t, pool, pid, done, "the tenant prune", "")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	<-done
	if pruneErr != nil {
		t.Fatalf("prune: %v", pruneErr)
	}
	if n := pwCount(t, pool, "api_key_id = $1", key.ID); n != 0 {
		t.Errorf("%d stages of the pruned key left under a REPEATABLE READ default", n)
	}
}

// TestTenantPruneWithoutKeysLeavesThePendingWritesAlone: a tenant without
// keys has no stage, so its prune must not touch context_pending_writes —
// not even with an empty id list, which still takes the hypertable's lock.
// A transaction holding that table (an out-of-band maintenance step) must not
// hold the prune.
func TestTenantPruneWithoutKeysLeavesThePendingWritesAlone(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	tn, err := store.CreateTenant(ctx, pool, "p547-nokeys", "nokeys")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `LOCK TABLE context_pending_writes IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := store.PruneTenant(pctx, pool, tn.ID, store.AuditStamp{}); err != nil {
		t.Fatalf("prune of a tenant without keys beside a holder of context_pending_writes: %v", err)
	}
	if _, err := store.GetTenant(ctx, pool, tn.ID); !errors.Is(err, store.ErrTenantNotFound) {
		t.Errorf("tenant after the prune: %v, want ErrTenantNotFound", err)
	}
}
