//go:build integration

package store_test

// Issue #277, migration 194: context_pending_writes keeps no foreign key on
// context_api_keys and context_principals. TimescaleDB copied both onto every
// chunk, so the chunk lifecycle of the staging store locked the two tables
// every authentication reads: creating a chunk took SHARE ROW EXCLUSIVE on
// them, dropping one (the confirm-retention eviction) ACCESS EXCLUSIVE. What
// the keys carried is code now: every writer of a stage reads its key row
// FOR KEY SHARE first (store.StagePendingWrite, ConsumePendingWrite), the
// tenant prune deletes its keys and then, in the same transaction, the
// stages of those keys (store.PruneTenant), every read selects by the
// caller's key.
//
//	go test -tags=integration ./internal/store/ -run 'TestPendingWrite|TestHypertablesCarryNoForeignKey' -count=1 -v

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
		if dropped != 1 {
			t.Errorf("eviction beside a reader of %s dropped %d chunks, want the aged one", rel, dropped)
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

	// A key minted and staged while the prune runs, before its key delete:
	// the key delete takes it, and the stage delete — by the ids the key
	// delete returned — takes its stage.
	t.Run("key minted during the prune", func(t *testing.T) {
		tn, err := store.CreateTenant(ctx, pool, "pw-order-c", "c")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.CreateApiKey(ctx, pool, "pw-c", "private", nil, tn.ID); err != nil {
			t.Fatal(err)
		}
		stop := &pwPauseTracer{part: "DELETE FROM context_api_keys WHERE tenant_id", reached: make(chan struct{}), release: make(chan struct{})}
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		cfg.ConnConfig.Tracer = stop
		pp, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer pp.Close()
		// Every exit lets the paused statement go before the pool closes
		// (the deferred calls run in reverse order).
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(stop.release) }) }
		defer release()
		pruneDone := make(chan error, 1)
		go func() { pruneDone <- store.PruneTenant(ctx, pp, tn.ID, store.AuditStamp{}) }()
		select {
		case <-stop.reached:
		case <-time.After(30 * time.Second):
			t.Fatal("the prune never reached its key delete")
		}
		late, _, err := store.CreateApiKey(ctx, pool, "pw-c-late", "private", nil, tn.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.StagePendingWrite(ctx, pool, late.ID, "private", "store", "mcp", []byte(`{"op":"store"}`), "late-key", time.Minute); err != nil {
			t.Fatal(err)
		}
		release()
		if err := <-pruneDone; err != nil {
			t.Fatalf("prune: %v", err)
		}
		if n := pwCount(t, pool, "api_key_id = $1", late.ID); n != 0 {
			t.Errorf("%d stages of the key minted during the prune left", n)
		}
	})

	if n := pwCount(t, pool, "api_key_id = $1", other.ID); n != 1 {
		t.Errorf("the other tenant's stage: %d rows, want 1 (the prune deletes its own tenant's stages only)", n)
	}
}

// pwHoldLock is the advisory lock pwHoldUpdates waits on.
const pwHoldLock = 277001

// pwHoldUpdates installs a test-only trigger: an UPDATE of a stage whose
// payload hash starts with "hold-" waits, with the stage row locked, until
// the advisory lock pwHoldLock is free. It returns a one-connection pool
// holding that lock and its release. The re-arm and the consume are single
// autocommit statements; the trigger holds them inside the statement, where
// the tenant prune meets them, and leaves the statements themselves as the
// production functions send them.
func pwHoldUpdates(t *testing.T, pool *pgxpool.Pool, dsn string) (release func()) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		CREATE OR REPLACE FUNCTION pw_hold_update() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
		    IF NEW.payload_hash LIKE 'hold-%' THEN
		        PERFORM pg_advisory_xact_lock_shared(277001);
		    END IF;
		    RETURN NEW;
		END $$;
		DROP TRIGGER IF EXISTS pw_hold_update ON context_pending_writes;
		CREATE TRIGGER pw_hold_update BEFORE UPDATE ON context_pending_writes
		    FOR EACH ROW EXECUTE FUNCTION pw_hold_update();`); err != nil {
		t.Fatalf("hold trigger: %v", err)
	}
	holder := wlConn(t, dsn)
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock($1)`, pwHoldLock); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			if _, err := holder.Exec(ctx, `SELECT pg_advisory_unlock($1)`, pwHoldLock); err != nil {
				t.Errorf("release the hold: %v", err)
			}
		})
	}
	t.Cleanup(release)
	return release
}

// pwPruneTenantKey makes a tenant with one key and one open stage of hash
// through the production write paths.
func pwPruneTenantKey(t *testing.T, pool *pgxpool.Pool, slug, hash string) (tenantID, keyID string) {
	t.Helper()
	ctx := context.Background()
	tn, err := store.CreateTenant(ctx, pool, slug, slug)
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := store.CreateApiKey(ctx, pool, slug+"-key", "private", nil, tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StagePendingWrite(ctx, pool, key.ID, "private", "store", "mcp", []byte(`{"op":"store"}`), hash, time.Hour); err != nil {
		t.Fatalf("stage: %v", err)
	}
	return tn.ID, key.ID
}

// TestPendingWriteRearmAndConsumeBesideTheTenantPrune: a re-arm or a consume
// of an open stage that runs when the tenant prune deletes the stage's key
// leaves no stage of that key once the prune has committed. Both update the
// stage row; the prune's stage delete on the hypertable (Custom Scan
// ModifyHypertable, PostgreSQL 18.6 / TimescaleDB 2.31.0) that waited for
// such an update deleted nothing — a plain table deletes the updated row — so
// before this the stage outlived its key. Both read the key row FOR KEY SHARE
// before they touch the stage now: the prune's key delete waits for them, and
// its stage delete, a statement after, sees the stage as they left it.
func TestPendingWriteRearmAndConsumeBesideTheTenantPrune(t *testing.T) {
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	payload := []byte(`{"op":"store"}`)

	t.Run("re-arm in an open transaction", func(t *testing.T) {
		tn, key := pwPruneTenantKey(t, pool, "pw-rearm-tx", "rearm-tx")
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
		if _, err := store.StagePendingWrite(ctx, tx, key, "private", "store", "mcp", payload, "rearm-tx", 2*time.Hour); err != nil {
			t.Fatalf("re-arm: %v", err)
		}
		pruneDone := make(chan struct{})
		var pruneErr error
		go func() { defer close(pruneDone); pruneErr = store.PruneTenant(ctx, pool, tn, store.AuditStamp{}) }()
		testdb.WaitBlockedBy(t, pool, pid, pruneDone, "the tenant prune", "")
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		<-pruneDone
		if pruneErr != nil {
			t.Fatalf("prune: %v", pruneErr)
		}
		if n := pwCount(t, pool, "api_key_id = $1", key); n != 0 {
			t.Errorf("%d stages of the pruned key left after a re-arm in an open transaction", n)
		}
	})

	for _, op := range []string{"re-arm", "consume"} {
		t.Run(op+" held in its statement", func(t *testing.T) {
			hash := "hold-" + op
			tn, key := pwPruneTenantKey(t, pool, "pw-"+op, hash)
			release := pwHoldUpdates(t, pool, dsn)
			opPool := wlConn(t, dsn)
			var opPid uint32
			if err := opPool.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&opPid); err != nil {
				t.Fatal(err)
			}
			opDone := make(chan struct{})
			var opErr error
			go func() {
				defer close(opDone)
				if op == "re-arm" {
					_, opErr = store.StagePendingWrite(ctx, opPool, key, "private", "store", "mcp", payload, hash, 2*time.Hour)
				} else {
					_, opErr = store.ConsumePendingWrite(ctx, opPool, key, hash)
				}
			}()
			pruneDone := make(chan struct{})
			var pruneErr error
			waitHeld(t, pool, opPid, opDone)
			go func() { defer close(pruneDone); pruneErr = store.PruneTenant(ctx, pool, tn, store.AuditStamp{}) }()
			testdb.WaitBlockedBy(t, pool, opPid, pruneDone, "the tenant prune", "")
			release()
			<-opDone
			<-pruneDone
			if opErr != nil {
				t.Fatalf("%s: %v", op, opErr)
			}
			if pruneErr != nil {
				t.Fatalf("prune: %v", pruneErr)
			}
			if n := pwCount(t, pool, "api_key_id = $1", key); n != 0 {
				t.Errorf("%d stages of the pruned key left after a %s beside the prune", n, op)
			}
		})
	}
}

// TestPendingWriteRearmOfAGoneKeyExtendsNothing: a key row deleted by hand
// leaves its open stage; a re-arm of that stage under the gone key's id (a
// request authenticated before the delete) extends nothing and stages
// nothing, and a consume finds nothing — the key row is read before the stage
// is touched.
func TestPendingWriteRearmOfAGoneKeyExtendsNothing(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	key, _, err := store.CreateApiKey(ctx, pool, "pw-gone-rearm", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := store.StagePendingWrite(ctx, pool, key.ID, "private", "store", "mcp", []byte(`{"op":"store"}`), "gone-rearm", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM context_api_keys WHERE id = $1`, key.ID); err != nil {
		t.Fatalf("delete the key by hand: %v", err)
	}
	if _, err := store.StagePendingWrite(ctx, pool, key.ID, "private", "store", "mcp", []byte(`{"op":"store"}`), "gone-rearm", 10*time.Hour); !errors.Is(err, store.ErrPendingWriteKeyGone) {
		t.Errorf("re-arm under a gone key: %v, want ErrPendingWriteKeyGone", err)
	}
	var expires time.Time
	if err := pool.QueryRow(ctx, `SELECT expires_at FROM context_pending_writes WHERE id = $1`, staged.ID).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	if !expires.Equal(*staged.ExpiresAt) {
		t.Errorf("the stage of the gone key expires at %s, staged with %s: the re-arm extended it", expires, *staged.ExpiresAt)
	}
	if _, err := store.ConsumePendingWrite(ctx, pool, key.ID, "gone-rearm"); !errors.Is(err, store.ErrPendingWriteNotFound) {
		t.Errorf("consume under a gone key: %v, want ErrPendingWriteNotFound", err)
	}
	if n := pwCount(t, pool, "api_key_id = $1 AND consumed_at IS NULL", key.ID); n != 1 {
		t.Errorf("%d open stages of the gone key, want the one left by the delete", n)
	}
}

// pwRounds is how many times TestPendingWriteNoStageOutlivesItsPrunedKey
// runs each operation beside a tenant prune.
const pwRounds = 200

// TestPendingWriteNoStageOutlivesItsPrunedKey: every operation on the staged
// writes — a new stage, a re-arm, a re-arm in a staged batch's transaction
// (budget booked first), a consume, a lookup, the eviction — run on a second
// connection beside the tenant prune at a random point of it, pwRounds times
// each: once the prune has committed, no stage of a deleted key stands, and
// no pair ends in a deadlock (the operations take the key row before the
// stage rows, as the prune does).
func TestPendingWriteNoStageOutlivesItsPrunedKey(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	payload := []byte(`{"op":"store"}`)
	type opFn func(key string) error
	ops := []struct {
		name string
		run  opFn
	}{
		{"stage", func(key string) error {
			_, err := store.StagePendingWrite(ctx, pool, key, "private", "store", "mcp", payload, "new", time.Hour)
			return err
		}},
		{"re-arm", func(key string) error {
			_, err := store.StagePendingWrite(ctx, pool, key, "private", "store", "mcp", payload, "open", 2*time.Hour)
			return err
		}},
		{"re-arm in a batch", func(key string) error {
			tx, err := pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if err := store.ReserveWriteIntents(ctx, tx, store.WriteBudget{APIKeyID: key, Limit: 1000}, 1); err != nil {
				return err
			}
			if _, err := store.StagePendingWrite(ctx, tx, key, "private", "store_batch", "mcp", payload, "open", 2*time.Hour); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}},
		{"consume", func(key string) error {
			_, err := store.ConsumePendingWrite(ctx, pool, key, "open")
			return err
		}},
		{"lookup", func(key string) error {
			_, err := store.LookupPendingWrite(ctx, pool, key, "open")
			return err
		}},
		{"evict", func(string) error {
			_, err := store.EvictPendingWrites(ctx, pool, 24*time.Hour)
			return err
		}},
	}
	code := func(err error) string {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return pgErr.Code
		}
		return ""
	}
	deadlock := func(err error) bool { return code(err) == "40P01" }
	// The prune's length sets the range the operation starts in.
	tn, _ := pwPruneTenantKey(t, pool, "pw-sweep-probe", "open")
	start := time.Now()
	if err := store.PruneTenant(ctx, pool, tn, store.AuditStamp{}); err != nil {
		t.Fatal(err)
	}
	span := time.Since(start)
	for _, op := range ops {
		var deadlocks, committed int
		for i := 0; i < pwRounds; i++ {
			tn, key := pwPruneTenantKey(t, pool, fmt.Sprintf("pw-sweep-%s-%d", strings.ReplaceAll(op.name, " ", "-"), i), "open")
			if op.name == "evict" {
				if _, err := pool.Exec(ctx, `
					INSERT INTO context_pending_writes (api_key_id, scope, op, origin, payload, payload_hash, created_at)
					VALUES ($1, 'private', 'store', 'mcp', '{"op":"store"}', 'aged', now() - interval '3 days')`, key); err != nil {
					t.Fatal(err)
				}
			}
			delay := time.Duration(rand.Int64N(int64(span) + 1))
			opDone := make(chan error, 1)
			go func() {
				time.Sleep(delay)
				opDone <- op.run(key)
			}()
			pruneErr := store.PruneTenant(ctx, pool, tn, store.AuditStamp{})
			opErr := <-opDone
			switch {
			case opErr == nil, errors.Is(opErr, store.ErrPendingWriteKeyGone), errors.Is(opErr, store.ErrPendingWriteNotFound):
			case code(opErr) == "23503" && op.name == "re-arm in a batch":
				// The batch's budget booking after the key delete: its foreign
				// key on the gone key refuses it, nothing is staged.
			case deadlock(opErr):
				deadlocks++
			default:
				t.Fatalf("%s round %d: %v", op.name, i, opErr)
			}
			switch {
			case pruneErr == nil:
				committed++
				if n := pwCount(t, pool, "api_key_id = $1", key); n != 0 {
					t.Errorf("%s round %d (operation %s after the prune's start): %d stages of the pruned key left", op.name, i, delay, n)
				}
			case deadlock(pruneErr):
				deadlocks++
			default:
				t.Fatalf("%s round %d: prune: %v", op.name, i, pruneErr)
			}
		}
		t.Logf("%s: %d rounds beside the prune (span %s), %d prunes committed, %d deadlocks", op.name, pwRounds, span, committed, deadlocks)
		if deadlocks != 0 {
			t.Errorf("%s beside the prune: %d deadlocks (40P01)", op.name, deadlocks)
		}
	}
}

// waitHeld waits until the backend pid waits on an advisory lock (the hold of
// pwHoldUpdates); it fails when done closes first.
func waitHeld(t *testing.T, pool *pgxpool.Pool, pid uint32, done <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(testdb.CommitHoldBound)
	for {
		select {
		case <-done:
			t.Fatalf("pid %d finished without reaching the hold", pid)
		default:
		}
		var ev string
		if err := pool.QueryRow(context.Background(),
			`SELECT COALESCE(wait_event, '') FROM pg_stat_activity WHERE pid = $1`, int64(pid)).Scan(&ev); err != nil {
			t.Fatal(err)
		}
		if ev == "advisory" {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("pid %d never reached the hold (wait_event %q)", pid, ev)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// pwPauseTracer holds the first statement containing part until release, once
// reached is closed: a transaction paused between two of its statements.
type pwPauseTracer struct {
	part    string
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *pwPauseTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, p.part) {
		p.once.Do(func() {
			close(p.reached)
			<-p.release
		})
	}
	return ctx
}

func (p *pwPauseTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

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
	if n := pwCount(t, pool, "api_key_id = $1 AND payload_hash = 'same-hash'", gone.ID); n != 1 {
		t.Fatalf("%d stages of the deleted key, want the one it left: the premise of the test", n)
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
	own, err := store.StagePendingWrite(ctx, pool, caller.ID, "private", "store", "mcp", []byte(`{"op":"store"}`), "same-hash", time.Hour)
	if err != nil {
		t.Fatalf("stage of the caller: %v", err)
	}
	pw, err := store.ConsumePendingWrite(ctx, pool, caller.ID, "same-hash")
	if err != nil {
		t.Fatalf("consume of the caller's own stage: %v", err)
	}
	if pw.ID != own.ID {
		t.Errorf("consumed stage %s, want the caller's own %s", pw.ID, own.ID)
	}
	if n := pwCount(t, pool, "api_key_id = $1 AND consumed_at IS NULL", gone.ID); n != 1 {
		t.Errorf("%d open stages of the deleted key, want its one stage untouched", n)
	}
}
