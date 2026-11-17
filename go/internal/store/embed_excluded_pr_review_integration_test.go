//go:build integration

// Issue #213, review round on the pull request: a reconcile batch and a block
// that moves to another tenant's scope while the batch waits for it, and
// scope reassignments while a trigger waits for a group lock.
//
//	go test -tags=integration ./internal/store/ -run 'TestEmbedExcludedBatchSkipsBlockMovedAway|TestEmbedExcludedGroupMovesDuringLockWait' -count=1 -v
package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// r5Waits reports whether some session waits for the transaction xid (a row
// lock it holds), polling until the deadline or until done says the waiting
// statement ended.
func r5Waits(t *testing.T, pool *pgxpool.Pool, xid string, done func() bool) bool {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		var waiting bool
		if err := pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_locks
			WHERE locktype = 'transactionid' AND NOT granted AND transactionid::text = $1)`, xid).Scan(&waiting); err != nil {
			t.Fatalf("row lock wait probe: %v", err)
		}
		if waiting {
			return true
		}
		if done() {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// r5OwnPool opens a pool of one connection on the test database and
// returns it with the backend pid of that connection, so a test can watch the
// statement it runs there.
func r5OwnPool(t *testing.T, dsn string) (*pgxpool.Pool, int) {
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
	var pid int
	if err := p.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	return p, pid
}

// r5EndedOrWaiting waits until the statement behind done has ended or the
// backend pid waits on a lock (wait_event_type Lock); it reports whether it
// waits, the wait event and the statement that waits. Neither within 30 s
// fails the test: nothing can be concluded from a statement not seen.
func r5EndedOrWaiting(t *testing.T, pool *pgxpool.Pool, pid int, done func() bool) (waiting bool, event, query string) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		if done() {
			return false, "", ""
		}
		if err := pool.QueryRow(context.Background(), `SELECT COALESCE(max(wait_event), ''), COALESCE(max(query), '') FROM pg_stat_activity
			WHERE pid = $1 AND state = 'active' AND wait_event_type = 'Lock'`, pid).Scan(&event, &query); err != nil {
			t.Fatalf("lock wait probe: %v", err)
		}
		if event != "" {
			return true, event, query
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the statement of backend %d neither ended nor waited on a lock within 30 s", pid)
	return false, "", ""
}

// TestEmbedExcludedBatchSkipsBlockMovedAway: a reconcile batch of tenant A's
// entry selects a block of A's scope and waits for its row lock, held by a
// transaction that moves the block into tenant B's scope. After both commit,
// the block's row version is the move's: the batch re-checks the moved row
// against its own scope and name and leaves it alone. One case per batch
// statement: the pending blocks with and without a created_at and the stale
// ones (B has no row of the name, the moved block stays unmarked), and the
// clear (B excludes the name, the moved block stays marked).
func TestEmbedExcludedBatchSkipsBlockMovedAway(t *testing.T) {
	for _, dir := range []string{"mark", "mark_null_created_at", "mark_stale", "clear"} {
		t.Run(dir, func(t *testing.T) {
			pool := testdb.SetupTestDB(t)
			ctx := context.Background()
			reconcile(t, pool)
			ownerTenant(t, pool, "mv-a", "mv-a")
			ownerTenant(t, pool, "mv-b", "mv-b")
			var ids []string
			switch dir {
			case "mark":
				ids = ownerSeed(t, pool, "mv-a", "mv-x", 3)
			case "mark_null_created_at":
				ids = ownerSeed(t, pool, "mv-a", "mv-x", 3)
				if _, err := pool.Exec(ctx, `UPDATE context_blocks SET created_at = NULL WHERE id = ANY($1::uuid[])`, ids); err != nil {
					t.Fatal(err)
				}
			case "mark_stale":
				for i := 0; i < 3; i++ {
					ids = append(ids, staleFixture(t, pool, "mv-a", "mv stale "+string(rune('a'+i)), "mv-x").ID)
				}
			case "clear":
				createType(t, pool, "mv-x", "mv-b", "excluded")
				row := createType(t, pool, "mv-x", "mv-a", "excluded")
				ids = ownerSeed(t, pool, "mv-a", "mv-x", 3)
				reconcile(t, pool)
				if got := markedCount(t, pool, "mv-a", "mv-x"); got != 3 {
					t.Fatalf("fixture: %d/3 of A's mv-x marked", got)
				}
				updateType(t, pool, row.ID, "mv-a", "full-pass")
			}
			if dir != "clear" {
				createType(t, pool, "mv-x", "mv-a", "excluded")
			}
			moved := ids[0]

			mover, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mover.Rollback(ctx) }()
			if _, err := mover.Exec(ctx, `UPDATE context_blocks SET scope = 'mv-b' WHERE id = $1`, moved); err != nil {
				t.Fatal(err)
			}
			var xid string
			if err := mover.QueryRow(ctx, `SELECT xmin::text FROM context_blocks WHERE id = $1`, moved).Scan(&xid); err != nil {
				t.Fatal(err)
			}
			rdone := make(chan error, 1)
			go func() {
				_, err := store.ReconcileEmbedExcluded(ctx, pool)
				rdone <- err
			}()
			if !r5Waits(t, pool, xid, func() bool { return len(rdone) > 0 }) {
				t.Fatalf("the reconcile batch did not wait for the moving block's row lock")
			}
			if err := mover.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-rdone; err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			var after string
			var marked bool
			if err := pool.QueryRow(ctx, `SELECT xmin::text, embed_excluded FROM context_blocks WHERE id = $1`, moved).Scan(&after, &marked); err != nil {
				t.Fatal(err)
			}
			if after != xid {
				t.Errorf("the %s batch wrote the block after it moved to tenant B's scope (row version %s, the move's %s)", dir, after, xid)
			}
			if want := dir == "clear"; marked != want {
				t.Errorf("moved block's marker %v, want %v (B's answer)", marked, want)
			}
			want := 2
			if dir == "clear" {
				want = 0
			}
			if got := markedCount(t, pool, "mv-a", "mv-x"); got != want {
				t.Errorf("%d of A's two remaining mv-x blocks marked, want %d", got, want)
			}
		})
	}
}

// r5GroupHook replaces ctx_embed_excluded_group, in this test's database
// only, by a wrapper around the original: in a session with r5.hook = on it
// waits on the session advisory lock hashtext('r5hook:' || group) after
// every read of a group. Holding that lock stops a trigger between reading
// a group and taking its lock, the window in which a scope reassignment has
// to commit for the trigger to see the scope move again.
func r5GroupHook(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, st := range []string{
		`ALTER FUNCTION ctx_embed_excluded_group(text) RENAME TO r5_group_orig`,
		`CREATE FUNCTION ctx_embed_excluded_group(p_scope TEXT) RETURNS TEXT LANGUAGE plpgsql AS $$
		 DECLARE v TEXT := r5_group_orig(p_scope);
		 BEGIN
		     IF current_setting('r5.hook', true) = 'on' THEN
		         PERFORM pg_advisory_lock_shared(hashtext('r5hook:' || v));
		         PERFORM pg_advisory_unlock_shared(hashtext('r5hook:' || v));
		     END IF;
		     RETURN v;
		 END $$`,
	} {
		if _, err := pool.Exec(context.Background(), st); err != nil {
			t.Fatalf("%s: %v", st, err)
		}
	}
}

// r5MoveDuring runs write in a transaction of its own, with the group hook
// on, while the scope, owned by tenants[0], is reassigned to tenants[1] ..
// tenants[moves] one after another: the first reassignment holds tenants[0]'s
// group lock when write asks for it and commits while write waits; each
// further one commits while write stands between reading the group of the
// reassignment before and taking its lock. It returns write's error, nil
// when it committed.
func r5MoveDuring(t *testing.T, pool *pgxpool.Pool, dsn, scope string, tenants []string, moves int, write string) error {
	t.Helper()
	ctx := context.Background()
	hold, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()
	defer func() { _, _ = hold.Exec(ctx, `SELECT pg_advisory_unlock_all()`) }()
	for _, tn := range tenants[1 : moves+1] {
		if _, err := hold.Exec(ctx, `SELECT pg_advisory_lock(hashtext('r5hook:t:' || $1))`, tn); err != nil {
			t.Fatal(err)
		}
	}
	first, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Rollback(ctx) }()
	if _, err := first.Exec(ctx, `UPDATE context_tenant_scopes SET tenant_id = $1 WHERE scope = $2`, tenants[1], scope); err != nil {
		t.Fatal(err)
	}

	wpool, pid := r5OwnPool(t, dsn)
	done := make(chan error, 1)
	go func() {
		tx, err := wpool.Begin(ctx)
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SET LOCAL r5.hook = 'on'`); err != nil {
			done <- err
			return
		}
		if _, err := tx.Exec(ctx, write); err != nil {
			done <- err
			return
		}
		done <- tx.Commit(ctx)
	}()
	waiting := func() bool {
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
			if len(done) > 0 {
				return false
			}
			var w bool
			if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE pid = $1 AND locktype = 'advisory' AND NOT granted)`, pid).Scan(&w); err != nil {
				t.Fatalf("advisory wait probe: %v", err)
			}
			if w {
				return true
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("the write neither waited on an advisory lock nor ended within 30 s")
		return false
	}
	if !waiting() {
		t.Fatalf("the write did not wait for the group lock the first reassignment holds")
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= moves; i++ {
		if !waiting() {
			break // the write ended: it read no further group
		}
		if _, err := pool.Exec(ctx, `UPDATE context_tenant_scopes SET tenant_id = $1 WHERE scope = $2`, tenants[i], scope); err != nil {
			t.Fatalf("reassignment %d: %v", i, err)
		}
		if _, err := hold.Exec(ctx, `SELECT pg_advisory_unlock(hashtext('r5hook:t:' || $1))`, tenants[i-1]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := hold.Exec(ctx, `SELECT pg_advisory_unlock_all()`); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatalf("the write did not end after the last reassignment")
		return nil
	}
}

// r5Tenants creates n tenants with a home scope each and assigns scope to
// the first.
func r5Tenants(t *testing.T, pool *pgxpool.Pool, prefix, scope string, n int) []string {
	t.Helper()
	ids := make([]string, n)
	for i := range ids {
		slug := prefix + "-" + string(rune('a'+i))
		ids[i] = ownerTenant(t, pool, slug, slug)
	}
	if _, err := store.AssignTenantScope(context.Background(), pool, ids[0], scope, nil); err != nil {
		t.Fatal(err)
	}
	return ids
}

// TestEmbedExcludedGroupMovesDuringLockWait: a scope is reassigned between
// tenants while a trigger waits for a group lock.
//   - registry_two_moves: a registry write of an excluded row in the scope
//     waits for the first reassignment; a second one commits between the
//     trigger's next read of the group and its lock. The trigger reads the
//     group until it holds still and queues the final group, so the blocks
//     of the scope are marked by the reconcile under their final owner.
//   - registry_bound, block_bound: eight reassignments during one write. Both
//     triggers stop at eight group locks and fail with 40001 instead of
//     deciding or queueing for a group they hold no lock of.
func TestEmbedExcludedGroupMovesDuringLockWait(t *testing.T) {
	const registryWrite = `INSERT INTO context_block_types (name, scope, display_name, builtin, is_default, config)
		VALUES ('gm-x', 'gm-s', 'gm', false, false, '{"retrieval":{"policy":"excluded"}}')`
	t.Run("registry_two_moves", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBWithDSN(t)
		ctx := context.Background()
		reconcile(t, pool)
		tenants := r5Tenants(t, pool, "gm", "gm-s", 3)
		if _, err := pool.Exec(ctx, `INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
			SELECT 'gm', 'gm ' || i, 'x', 'gm-x', 'gm-s', 'public', now() FROM generate_series(1, 3) i`); err != nil {
			t.Fatal(err)
		}
		reconcile(t, pool)
		r5GroupHook(t, pool)
		if err := r5MoveDuring(t, pool, dsn, "gm-s", tenants, 2, registryWrite); err != nil {
			t.Fatalf("registry write: %v", err)
		}
		var queued bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM context_embed_excluded_queue WHERE name = 'gm-x' AND grp = 't:' || $1)`, tenants[2]).Scan(&queued); err != nil {
			t.Fatal(err)
		}
		if !queued {
			t.Errorf("the registry write did not queue the group of the scope's final owner")
		}
		reconcile(t, pool)
		if got := markedCount(t, pool, "gm-s", "gm-x"); got != 3 {
			t.Errorf("%d/3 blocks of gm-x in the reassigned scope marked after the reconcile", got)
		}
	})
	for _, c := range []struct{ name, write string }{
		{"registry_bound", registryWrite},
		{"block_bound", `INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
			VALUES ('gm', 'gm bound', 'x', 'checkpoint', 'gm-s', 'public', now())`},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool, dsn := testdb.SetupTestDBWithDSN(t)
			reconcile(t, pool)
			tenants := r5Tenants(t, pool, "gb", "gm-s", 9)
			r5GroupHook(t, pool)
			err := r5MoveDuring(t, pool, dsn, "gm-s", tenants, 8, c.write)
			var pe *pgconn.PgError
			if !errors.As(err, &pe) || pe.Code != "40001" {
				t.Errorf("write over eight reassignments ended with %v, want 40001", err)
			}
		})
	}
}
