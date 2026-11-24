//go:build integration

package store_test

// Issue #277, migration 194: context_pending_writes keeps no foreign key on
// context_api_keys and context_principals. TimescaleDB copied both onto every
// chunk, so the chunk lifecycle of the staging store locked the two tables
// every authentication reads: creating a chunk took SHARE ROW EXCLUSIVE on
// them, dropping one (the confirm-retention eviction) ACCESS EXCLUSIVE. What
// the keys carried is code now: a stage reads its key row FOR KEY SHARE
// (store.StagePendingWrite), the tenant prune deletes the stages of its keys
// before the keys (store.PruneTenant), every read selects by the caller's key.
//
//	go test -tags=integration ./internal/store/ -run 'TestPendingWrite.*AuthTables|TestPendingWriteStageOfAGoneKey|TestPendingWriteStageAndTenantPrune|TestPendingWriteOfADeletedKey|TestHypertablesCarryNoForeignKey' -count=1 -v

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func pwCount(t *testing.T, pool *pgxpool.Pool, where string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM context_pending_writes WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatalf("count stages %s: %v", where, err)
	}
	return n
}

// pwAuth authenticates plaintext through ctx_auth on its own connection within
// bound and reports whether it answered and whether the key was valid.
func pwAuth(t *testing.T, dsn, plaintext string, bound time.Duration) (answered, valid bool) {
	t.Helper()
	p := wlConn(t, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	err := p.QueryRow(ctx, `SELECT is_valid FROM ctx_auth($1)`, plaintext).Scan(&valid)
	if err != nil {
		if ctx.Err() != nil {
			return false, false
		}
		t.Fatalf("ctx_auth: %v", err)
	}
	return true, valid
}

// TestHypertablesCarryNoForeignKey: no TimescaleDB hypertable of the schema
// has a foreign key. A key there is copied onto every chunk; creating and
// dropping chunks then locks the referenced table, and a delete there probes
// every chunk.
func TestHypertablesCarryNoForeignKey(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	rows, err := pool.Query(context.Background(), `
		SELECT c.conname, c.conrelid::regclass::text, c.confrelid::regclass::text
		  FROM pg_constraint c
		  JOIN pg_class r ON r.oid = c.conrelid
		  JOIN pg_namespace n ON n.oid = r.relnamespace
		 WHERE c.contype = 'f'
		   AND EXISTS (SELECT 1 FROM timescaledb_information.hypertables h
		                WHERE h.hypertable_schema = n.nspname AND h.hypertable_name = r.relname)
		 ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, tbl, ref string
		if err := rows.Scan(&name, &tbl, &ref); err != nil {
			t.Fatal(err)
		}
		t.Errorf("hypertable %s carries foreign key %s onto %s: every chunk copies it", tbl, name, ref)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM timescaledb_information.hypertables WHERE hypertable_name IN ('context_pending_writes', 'context_llm_log')`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("the inventory sees %d of the two hypertables (err %v): the gate would pass on nothing", n, err)
	}
}

// TestPendingWriteChunkLifecycleLeavesTheAuthTablesAlone: a stage that
// creates a chunk takes no table lock on context_api_keys or
// context_principals beyond the row share of its key read, and an
// authentication goes through while it is open; the eviction drops an aged
// chunk and authentications go through while a transaction that read each
// table stays open (a backup, a long report).
func TestPendingWriteChunkLifecycleLeavesTheAuthTablesAlone(t *testing.T) {
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	key, plain, err := store.CreateApiKey(ctx, pool, "pw-chunks", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatal(err)
	}

	// (a) The first stage of an hour no chunk holds yet: a fresh database has
	// no chunk at all, so this stage creates one.
	if n := pwCount(t, pool, "true"); n != 0 {
		t.Fatalf("fresh database holds %d stages", n)
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
	if _, err := store.StagePendingWrite(ctx, tx, key.ID, "private", "store", "mcp", []byte(`{"op":"store"}`), "chunk-maker", time.Minute); err != nil {
		t.Fatalf("stage: %v", err)
	}
	rows, err := pool.Query(ctx, `SELECT c.relname, l.mode FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
		 WHERE l.pid = $1 AND c.relname IN ('context_api_keys', 'context_principals')
		   AND l.mode NOT IN ('AccessShareLock', 'RowShareLock') ORDER BY 1, 2`, int64(pid))
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var rel, mode string
		if err := rows.Scan(&rel, &mode); err != nil {
			t.Fatal(err)
		}
		t.Errorf("a stage creating a chunk holds %s on %s until it commits", mode, rel)
	}
	rows.Close()
	if answered, valid := pwAuth(t, dsn, plain, 3*time.Second); !answered || !valid {
		t.Errorf("an authentication beside a chunk-creating stage: answered=%v valid=%v, want both", answered, valid)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// (b) An aged chunk and a long reader of each table.
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_pending_writes (api_key_id, scope, op, origin, payload, payload_hash, created_at)
		VALUES ($1, 'private', 'store', 'mcp', '{"op":"store"}', 'aged', now() - interval '3 days')`, key.ID); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"context_api_keys", "context_principals"} {
		reader := wlConn(t, dsn)
		rtx, err := reader.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rtx.Exec(ctx, `SELECT 1 FROM `+rel+` LIMIT 1`); err != nil {
			t.Fatal(err)
		}
		ectx, cancel := context.WithTimeout(ctx, 10*time.Second)
		dropped, err := store.EvictPendingWrites(ectx, pool, 24*time.Hour)
		cancel()
		if err != nil {
			t.Errorf("eviction beside a reader of %s: %v (it waited for the reader)", rel, err)
		}
		if answered, valid := pwAuth(t, dsn, plain, 3*time.Second); !answered || !valid {
			t.Errorf("an authentication beside a reader of %s and the eviction: answered=%v valid=%v", rel, answered, valid)
		}
		_ = rtx.Rollback(ctx)
		if rel == "context_api_keys" && dropped != 1 {
			t.Errorf("eviction dropped %d chunks, want the aged one", dropped)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO context_pending_writes (api_key_id, scope, op, origin, payload, payload_hash, created_at)
			VALUES ($1, 'private', 'store', 'mcp', '{"op":"store"}', 'aged', now() - interval '3 days')`, key.ID); err != nil {
			t.Fatal(err)
		}
	}
}

// TestPendingWriteStageOfAGoneKeyStagesNothing: a stage for a key id with no
// row (the key deleted between the caller's authentication and its stage)
// writes nothing and says why.
func TestPendingWriteStageOfAGoneKeyStagesNothing(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	gone := "019a0000-0000-7000-8000-000000000277"
	_, err := store.StagePendingWrite(ctx, pool, gone, "private", "store", "mcp", []byte(`{"op":"store"}`), "orphan", time.Minute)
	if !errors.Is(err, store.ErrPendingWriteKeyGone) {
		t.Fatalf("stage for a gone key: %v, want ErrPendingWriteKeyGone", err)
	}
	if n := pwCount(t, pool, "api_key_id = $1", gone); n != 0 {
		t.Fatalf("%d stages of the gone key written", n)
	}
}

// TestPendingWriteStageAndTenantPruneAreOrdered: the two orders of a stage
// and the tenant prune's key delete leave no stage of the deleted key. A
// stage that holds the key first: the prune waits for it and deletes the
// stage with the keys. The prune first: the stage waits for it and finds its
// key gone.
func TestPendingWriteStageAndTenantPruneAreOrdered(t *testing.T) {
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	other, _, err := store.CreateApiKey(ctx, pool, "pw-other", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StagePendingWrite(ctx, pool, other.ID, "private", "store", "mcp", []byte(`{"op":"store"}`), "other", time.Minute); err != nil {
		t.Fatal(err)
	}

	t.Run("stage first", func(t *testing.T) {
		tn, err := store.CreateTenant(ctx, pool, "pw-order-a", "a")
		if err != nil {
			t.Fatal(err)
		}
		key, _, err := store.CreateApiKey(ctx, pool, "pw-a", "private", nil, tn.ID)
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
		if _, err := store.StagePendingWrite(ctx, tx, key.ID, "private", "store", "mcp", []byte(`{"op":"store"}`), "held", time.Minute); err != nil {
			t.Fatalf("stage: %v", err)
		}
		done := make(chan struct{})
		var pruneErr error
		go func() {
			defer close(done)
			pruneErr = store.PruneTenant(ctx, pool, tn.ID, store.AuditStamp{})
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
			t.Errorf("%d stages of the pruned key left", n)
		}
	})

	t.Run("prune first", func(t *testing.T) {
		tn, err := store.CreateTenant(ctx, pool, "pw-order-b", "b")
		if err != nil {
			t.Fatal(err)
		}
		key, _, err := store.CreateApiKey(ctx, pool, "pw-b", "private", nil, tn.ID)
		if err != nil {
			t.Fatal(err)
		}
		held, hold := testdb.HoldCommitPool(t, dsn, func(sql string) bool {
			return strings.Contains(sql, "DELETE FROM context_api_keys WHERE tenant_id")
		})
		pruneDone := make(chan error, 1)
		go func() { pruneDone <- store.PruneTenant(ctx, held, tn.ID, store.AuditStamp{}) }()
		pid := hold.Wait(t)
		done := make(chan struct{})
		var stageErr error
		go func() {
			defer close(done)
			_, stageErr = store.StagePendingWrite(ctx, pool, key.ID, "private", "store", "mcp", []byte(`{"op":"store"}`), "late", time.Minute)
		}()
		testdb.WaitBlockedBy(t, pool, pid, done, "the stage", "")
		hold.Release()
		if err := <-pruneDone; err != nil {
			t.Fatalf("prune: %v", err)
		}
		<-done
		if !errors.Is(stageErr, store.ErrPendingWriteKeyGone) {
			t.Errorf("a stage after the prune's key delete: %v, want ErrPendingWriteKeyGone", stageErr)
		}
		if n := pwCount(t, pool, "api_key_id = $1", key.ID); n != 0 {
			t.Errorf("%d stages of the pruned key left", n)
		}
	})

	if n := pwCount(t, pool, "api_key_id = $1", other.ID); n != 1 {
		t.Errorf("the other tenant's stage: %d rows, want 1 (the prune deletes its own tenant's stages only)", n)
	}
}

// TestPendingWriteOfADeletedKeyIsUnreachable: a key deleted by hand (no code
// path does) leaves its stages, and none of them can be read or consumed:
// the deleted key does not authenticate, and a stage is read only under the
// caller's own key id, so another key with the same payload hash finds
// nothing; the caller's own stage of the same hash is the one consumed.
// writes.confirm_retention removes such rows with their chunk.
func TestPendingWriteOfADeletedKeyIsUnreachable(t *testing.T) {
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	gone, plain, err := store.CreateApiKey(ctx, pool, "pw-gone", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatal(err)
	}
	caller, _, err := store.CreateApiKey(ctx, pool, "pw-caller", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StagePendingWrite(ctx, pool, gone.ID, "private", "store", "mcp", []byte(`{"op":"store"}`), "same-hash", time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM context_api_keys WHERE id = $1`, gone.ID); err != nil {
		t.Fatalf("delete the key by hand: %v", err)
	}
	if answered, valid := pwAuth(t, dsn, plain, 5*time.Second); !answered || valid {
		t.Fatalf("the deleted key authenticates: answered=%v valid=%v", answered, valid)
	}
	if _, err := store.LookupPendingWrite(ctx, pool, caller.ID, "same-hash"); !errors.Is(err, store.ErrPendingWriteNotFound) {
		t.Errorf("lookup by another key: %v, want ErrPendingWriteNotFound", err)
	}
	if _, err := store.ConsumePendingWrite(ctx, pool, caller.ID, "same-hash"); !errors.Is(err, store.ErrPendingWriteNotFound) {
		t.Errorf("consume by another key: %v, want ErrPendingWriteNotFound", err)
	}
	if _, err := store.StagePendingWrite(ctx, pool, caller.ID, "private", "store", "mcp", []byte(`{"op":"store"}`), "same-hash", time.Hour); err != nil {
		t.Fatalf("stage of the caller: %v", err)
	}
	pw, err := store.ConsumePendingWrite(ctx, pool, caller.ID, "same-hash")
	if err != nil {
		t.Fatalf("consume of the caller's own stage: %v", err)
	}
	if pw.APIKeyID != caller.ID {
		t.Errorf("consumed a stage of key %s, want the caller's %s", pw.APIKeyID, caller.ID)
	}
}
