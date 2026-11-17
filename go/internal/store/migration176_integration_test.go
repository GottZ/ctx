//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const migration176File = "176_embed_excluded_owner.sql"

// m176Seed writes a database at migration 175 with a stock: two tenants,
// tenant A's own excluded type m176-x, tenant B's overlay that lifts the
// _global-excluded checkpoint, a row of m176-x in a tenantless scope, blocks
// of both names with and without a vector, and a queue that is not empty
// (B's overlay queued checkpoint under the 175 trigger).
func m176Seed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := reconcile175(ctx, pool); err != nil {
		t.Fatalf("drain the 175 queue: %v", err)
	}
	ownerTenant(t, pool, "m176-a", "m176-a")
	ownerTenant(t, pool, "m176-b", "m176-b")
	createType(t, pool, "m176-x", "m176-a", "excluded")
	createType(t, pool, "m176-x", "m176-free", "full-pass")
	for _, s := range []string{"m176-a", "m176-b"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
			 SELECT 'm176', 'm176 ' || n || ' ' || i, 'x', n, $1, 'public', now()
			   FROM unnest(ARRAY['m176-x', 'checkpoint']) n, generate_series(1, 20) i`, s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET embedding = array_fill(0.1::real, ARRAY[1024])::vector, embed_model = 'test'
		  WHERE category = 'm176' AND title LIKE '% 1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcile175(ctx, pool); err != nil {
		t.Fatalf("175 reconcile: %v", err)
	}
	// B's overlay lifts checkpoint: under the 175 rule for every tenant. It
	// queues checkpoint, and the queue is left as it is.
	createType(t, pool, "checkpoint", "m176-b", "full-pass")
}

func blocksFingerprint(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(),
		`SELECT md5(string_agg(id::text || ':' || xmin::text || ':' || embed_excluded::text || ':' || (embedding IS NOT NULL)::text, ',' ORDER BY id))
		   FROM context_blocks`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestMigration176 applies migration 176 to a database at 175 with a stock:
// no block row is written by the migration, the old queue moves to the new
// table with group '*', the seed queues every name with a row outside
// _global, and after a reconcile the markers follow the owner rule. A second
// application of the file changes nothing the reconcile does not set back to
// the same values. The file takes no lock on context_blocks above SHARE ROW
// EXCLUSIVE.
func TestMigration176(t *testing.T) {
	ctx := context.Background()
	body, err := migrations.FS.ReadFile(migration176File)
	if err != nil {
		t.Fatalf("read %s: %v", migration176File, err)
	}

	t.Run("stock", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 175)
		m176Seed(t, pool)
		var oldQueued int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_embed_excluded_reconcile`).Scan(&oldQueued); err != nil {
			t.Fatal(err)
		}
		if oldQueued == 0 {
			t.Fatalf("fixture: the 175 queue is empty")
		}
		before := blocksFingerprint(t, pool)
		if err := store.RunMigrations(ctx, pool); err != nil {
			t.Fatalf("run migrations: %v", err)
		}
		if after := blocksFingerprint(t, pool); after != before {
			t.Errorf("migration 176 wrote rows of context_blocks")
		}
		var left int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_embed_excluded_reconcile`).Scan(&left); err != nil {
			t.Fatal(err)
		}
		if left != 0 {
			t.Errorf("%d rows left in the old queue", left)
		}
		rows, err := pool.Query(ctx, `SELECT name || '/' || grp FROM context_embed_excluded_queue ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			got = append(got, s)
		}
		rows.Close()
		if want := "checkpoint/*,m176-x/*"; joinComma(got) != want {
			t.Errorf("queue after the migration %v, want %s", got, want)
		}

		reconcile(t, pool)
		for _, c := range []struct {
			scope, typ string
			want       int
		}{
			{"m176-a", "m176-x", 19},    // A's own excluded type; the block with a vector keeps false
			{"m176-b", "m176-x", 0},     // B has no row of the name
			{"m176-a", "checkpoint", 20}, // B's overlay no longer lifts A's blocks (the one with a vector was marked at its insert, before the vector)
			{"m176-b", "checkpoint", 0},  // B's own overlay lifts B's
		} {
			if got := markedCount(t, pool, c.scope, c.typ); got != c.want {
				t.Errorf("%s %s: %d marked, want %d", c.scope, c.typ, got, c.want)
			}
		}
		var vectors int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_blocks WHERE category = 'm176' AND embedding IS NOT NULL AND NOT embed_excluded`).Scan(&vectors); err != nil {
			t.Fatal(err)
		}
		if vectors != 3 {
			t.Errorf("%d blocks with a vector and no marker, want 3 (a vector is kept; the checkpoint block of A was marked before it got one)", vectors)
		}

		// Applied once more (the runner skips an applied version; the file
		// itself is idempotent).
		settled := blocksFingerprint(t, pool)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("second application: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		reconcile(t, pool)
		if again := blocksFingerprint(t, pool); again != settled {
			t.Errorf("a second application of 176 and a reconcile changed rows of context_blocks")
		}
	})

	t.Run("locks", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 175)
		m176Seed(t, pool)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		var pid int
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply: %v", err)
		}
		took := time.Since(start)
		rows, err := pool.Query(ctx,
			`SELECT c.relname, l.mode FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
			  WHERE l.pid = $1 AND l.locktype = 'relation'
			    AND c.relname IN ('context_blocks', 'context_block_types', 'context_tenant_scopes',
			                      'context_embed_excluded_reconcile', 'context_embed_excluded_queue')
			  ORDER BY 1, 2`, pid)
		if err != nil {
			t.Fatal(err)
		}
		held := map[string][]string{}
		for rows.Next() {
			var rel, mode string
			if err := rows.Scan(&rel, &mode); err != nil {
				t.Fatal(err)
			}
			held[rel] = append(held[rel], mode)
		}
		rows.Close()
		t.Logf("migration 176 file: %s; relation locks held to its commit: %v", took, held)
		for _, mode := range held["context_blocks"] {
			switch mode {
			case "ShareRowExclusiveLock", "AccessShareLock", "RowExclusiveLock", "ShareUpdateExclusiveLock":
			default:
				t.Errorf("migration 176 holds %s on context_blocks", mode)
			}
		}
		// A reader of context_blocks is not blocked while the file is open.
		rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		var n int
		if err := pool.QueryRow(rctx, `SELECT count(*) FROM context_blocks`).Scan(&n); err != nil {
			t.Errorf("a reader of context_blocks waits for the open migration: %v", err)
		}
	})
}

func joinComma(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out
}
