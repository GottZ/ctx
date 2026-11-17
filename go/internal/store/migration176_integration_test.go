//go:build integration

package store_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const (
	migration176File = "176_embed_excluded_owner.sql"
	// m176IndexDef and m175IndexDef: idx_blocks_embed_excluded of migration
	// 176 and of 175, as PostgreSQL deparses them (the DO block of 176
	// compares these strings).
	m176IndexDef = "CREATE INDEX idx_blocks_embed_excluded ON public.context_blocks USING btree (type_name, scope, id) WHERE (embed_excluded AND (NOT is_archived))"
	m175IndexDef = "CREATE INDEX idx_blocks_embed_excluded ON public.context_blocks USING btree (type_name, id) WHERE (embed_excluded AND (NOT is_archived))"
)

// m176Index returns the definition and the oid of idx_blocks_embed_excluded.
func m176Index(t *testing.T, pool *pgxpool.Pool) (string, uint32) {
	t.Helper()
	var def string
	var oid uint32
	if err := pool.QueryRow(context.Background(),
		`SELECT pg_get_indexdef(c.oid), c.oid FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE n.nspname = 'public' AND c.relname = 'idx_blocks_embed_excluded'`).Scan(&def, &oid); err != nil {
		t.Fatalf("idx_blocks_embed_excluded: %v", err)
	}
	return def, oid
}

// m176Runbook runs the out-of-band build of the header of 176, statement by
// statement outside a transaction, as an operator does.
func m176Runbook(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, st := range []string{
		`CREATE INDEX CONCURRENTLY idx_blocks_embed_excluded_213 ON context_blocks (type_name, scope, id) WHERE embed_excluded AND NOT is_archived`,
		`DROP INDEX CONCURRENTLY idx_blocks_embed_excluded`,
		`ALTER INDEX idx_blocks_embed_excluded_213 RENAME TO idx_blocks_embed_excluded`,
	} {
		if _, err := pool.Exec(context.Background(), st); err != nil {
			t.Fatalf("runbook %q: %v", st, err)
		}
	}
}

// m176Run is what one application of the file in a transaction showed.
type m176Run struct {
	took         time.Duration
	modes        []string // relation locks on context_blocks held to the commit
	readerWaited bool     // a reader of context_blocks waited for the open file
	warnings     []string // WARNINGs naming idx_blocks_embed_excluded
}

// m176Apply applies the file in one transaction, as the runner does, over a
// pool of its own whose notice handler collects the WARNINGs of the DO
// block; with reltuples > 0 it first sets the row estimate of
// context_blocks in the same transaction (the threshold reads it). While the
// file is open it holds, a second connection reads context_blocks.
func m176Apply(t *testing.T, dsn string, reader *pgxpool.Pool, body []byte, reltuples float64) m176Run {
	t.Helper()
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var run m176Run
	cfg.ConnConfig.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		mu.Lock()
		defer mu.Unlock()
		if n.Severity == "WARNING" && strings.Contains(n.Message, "idx_blocks_embed_excluded") {
			run.warnings = append(run.warnings, n.Message)
		}
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var pid int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if reltuples > 0 {
		if _, err := tx.Exec(ctx, `UPDATE pg_class SET reltuples = $1 WHERE oid = 'context_blocks'::regclass`, reltuples); err != nil {
			t.Fatalf("set reltuples: %v", err)
		}
	}
	start := time.Now()
	if _, err := tx.Exec(ctx, string(body)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	run.took = time.Since(start)
	rows, err := reader.Query(ctx,
		`SELECT l.mode FROM pg_locks l WHERE l.pid = $1 AND l.locktype = 'relation' AND l.relation = 'context_blocks'::regclass ORDER BY 1`, pid)
	if err != nil {
		t.Fatal(err)
	}
	if run.modes, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	var n int
	run.readerWaited = reader.QueryRow(rctx, `SELECT count(*) FROM context_blocks WHERE id = '00000000-0000-0000-0000-000000000000'`).Scan(&n) != nil
	cancel()
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return run
}

// m176Weak reports whether every mode is at most SHARE ROW EXCLUSIVE.
func m176Weak(modes []string) bool {
	for _, mode := range modes {
		switch mode {
		case "ShareRowExclusiveLock", "AccessShareLock", "RowExclusiveLock", "ShareUpdateExclusiveLock":
		default:
			return false
		}
	}
	return true
}

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

// TestMigration176 applies migration 176 to a database at 175 with a stock
// (175's idx_blocks_embed_excluded, marked blocks in two tenants): no block
// row is written by the migration, the index gets the definition of 176
// (which binds no embedding column), the old queue moves to the new table
// with group '*', the seed queues every name with a row outside _global, and
// after a reconcile the markers follow the owner rule. A second application
// of the file changes nothing the reconcile does not set back to the same
// values and keeps the index. The ways through the index's DO block: below
// 500 000 rows the file drops 175's index and builds the new one under
// ACCESS EXCLUSIVE (a reader waits); an index built out of band with the
// runbook is adopted, and above the threshold the file only warns — in both
// the file takes no lock on context_blocks above SHARE ROW EXCLUSIVE, and on
// 175's index the reconcile clears correctly.
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
		def, oid := m176Index(t, pool)
		if def != m176IndexDef {
			t.Errorf("idx_blocks_embed_excluded deparses as\n  %s\nwant\n  %s", def, m176IndexDef)
		}
		// The columns the index binds, by its key and its predicate: no
		// embedding column, so the re-embed cutover's column renames leave it
		// as it is (not in the attnum class, no twin in verifySisterIndexes).
		var bound string
		if err := pool.QueryRow(ctx, `SELECT string_agg(a.attname, ',' ORDER BY a.attname)
			  FROM pg_index i
			  JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum > 0 AND NOT a.attisdropped
			 WHERE i.indexrelid = 'idx_blocks_embed_excluded'::regclass
			   AND (a.attnum = ANY (i.indkey::int2[])
			        OR pg_get_expr(i.indpred, i.indrelid) ~ ('\m' || a.attname || '\M'))`).Scan(&bound); err != nil {
			t.Fatal(err)
		}
		if want := "embed_excluded,id,is_archived,scope,type_name"; bound != want {
			t.Errorf("idx_blocks_embed_excluded binds %s, want %s", bound, want)
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
		if def2, oid2 := m176Index(t, pool); def2 != m176IndexDef || oid2 != oid {
			t.Errorf("the second application rebuilt the index: oid %d -> %d, %s", oid, oid2, def2)
		}
	})

	t.Run("rebuild_below_threshold", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 175)
		m176Seed(t, pool)
		if def, _ := m176Index(t, pool); def != m175IndexDef {
			t.Fatalf("fixture: idx_blocks_embed_excluded is %s", def)
		}
		run := m176Apply(t, dsn, pool, body, 0)
		t.Logf("migration 176 file over 175's index: %s; relation locks on context_blocks held to its commit: %v", run.took, run.modes)
		if m176Weak(run.modes) {
			t.Errorf("the rebuild holds %v on context_blocks, want ACCESS EXCLUSIVE (DROP INDEX)", run.modes)
		}
		if !run.readerWaited {
			t.Errorf("a reader of context_blocks did not wait for the open rebuild")
		}
		if len(run.warnings) != 0 {
			t.Errorf("warnings below the threshold: %v", run.warnings)
		}
		if def, _ := m176Index(t, pool); def != m176IndexDef {
			t.Errorf("idx_blocks_embed_excluded after the file: %s", def)
		}
	})

	// A transaction that has read context_blocks when the file begins and
	// then writes it: the file asks for ACCESS EXCLUSIVE before any weaker
	// lock on context_blocks, so the write goes ahead of the waiting DROP and
	// both commit. Had the file taken the SHARE ROW EXCLUSIVE of the trigger
	// swap first, the write would wait for it and the DROP for the read: a
	// deadlock, one of the two ended with 40P01.
	t.Run("reader_that_writes", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 175)
		m176Seed(t, pool)
		var id string
		if err := pool.QueryRow(ctx, `SELECT id FROM context_blocks WHERE category = 'm176' ORDER BY id LIMIT 1`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		reader, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = reader.Rollback(ctx) }()
		var n int
		if err := reader.QueryRow(ctx, `SELECT count(*) FROM context_blocks WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		mpool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer mpool.Close()
		conn, err := mpool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		var mpid int
		if err := conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&mpid); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			tx, err := conn.Begin(ctx)
			if err != nil {
				done <- err
				return
			}
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				_ = tx.Rollback(ctx)
				done <- err
				return
			}
			done <- tx.Commit(ctx)
		}()
		waiting := false
		for deadline := time.Now().Add(10 * time.Second); !waiting && time.Now().Before(deadline); {
			if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE pid = $1 AND NOT granted)`, mpid).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if !waiting {
				time.Sleep(10 * time.Millisecond)
			}
		}
		if !waiting {
			t.Fatalf("the migration never waited for the reader")
		}
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, werr := reader.Exec(wctx, `UPDATE context_blocks SET title = title WHERE id = $1`, id)
		cancel()
		if werr != nil {
			t.Errorf("the reader's write while the migration waits: %v", werr)
		} else if err := reader.Commit(ctx); err != nil {
			t.Errorf("the reader's commit: %v", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the migration: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("the migration did not finish")
		}
		if def, _ := m176Index(t, pool); def != m176IndexDef {
			t.Errorf("idx_blocks_embed_excluded after the file: %s", def)
		}
	})

	t.Run("built_out_of_band", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 175)
		m176Seed(t, pool)
		m176Runbook(t, pool)
		_, oid := m176Index(t, pool)
		run := m176Apply(t, dsn, pool, body, 0)
		t.Logf("migration 176 file over an index built out of band: %s; relation locks on context_blocks held to its commit: %v", run.took, run.modes)
		if !m176Weak(run.modes) {
			t.Errorf("migration 176 holds %v on context_blocks with the index in place", run.modes)
		}
		if run.readerWaited {
			t.Errorf("a reader of context_blocks waited for the open migration")
		}
		if def, oid2 := m176Index(t, pool); def != m176IndexDef || oid2 != oid {
			t.Errorf("the index built out of band was not adopted: oid %d -> %d, %s", oid, oid2, def)
		}
	})

	t.Run("above_threshold", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 175)
		m176Seed(t, pool)
		run := m176Apply(t, dsn, pool, body, 600000)
		t.Logf("migration 176 file at 600 000 estimated rows: %s; relation locks on context_blocks held to its commit: %v; warnings %v", run.took, run.modes, run.warnings)
		if len(run.warnings) != 1 {
			t.Errorf("warnings %v, want one naming idx_blocks_embed_excluded", run.warnings)
		}
		if !m176Weak(run.modes) {
			t.Errorf("migration 176 holds %v on context_blocks above the threshold", run.modes)
		}
		if run.readerWaited {
			t.Errorf("a reader of context_blocks waited for the open migration above the threshold")
		}
		if def, _ := m176Index(t, pool); def != m175IndexDef {
			t.Fatalf("above the threshold the index became %s", def)
		}
		// On 175's index the clear stays correct: the '*' entry of checkpoint
		// clears B's blocks (B's overlay lifts them), A's own row lifted
		// clears A's group.
		reconcile(t, pool)
		if got := markedCount(t, pool, "m176-b", "checkpoint"); got != 0 {
			t.Errorf("%d/20 of B's checkpoint blocks marked after the reconcile on 175's index", got)
		}
		if got := markedCount(t, pool, "m176-a", "m176-x"); got != 19 {
			t.Fatalf("fixture: %d/19 of A's m176-x marked", got)
		}
		var id string
		if err := pool.QueryRow(ctx, `SELECT id FROM context_block_types WHERE name = 'm176-x' AND scope = 'm176-a'`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		stB := scopeState(t, pool, "m176-b")
		updateType(t, pool, id, "m176-a", "full-pass")
		reconcile(t, pool)
		if got := markedCount(t, pool, "m176-a", "m176-x"); got != 0 {
			t.Errorf("%d/19 of A's m176-x marked after A's lift on 175's index", got)
		}
		if scopeState(t, pool, "m176-b") != stB {
			t.Errorf("A's lift rewrote B's rows")
		}
		// The runbook gives the index the definition of 176.
		settled := blocksFingerprint(t, pool)
		m176Runbook(t, pool)
		if def, _ := m176Index(t, pool); def != m176IndexDef {
			t.Errorf("after the runbook the index is %s", def)
		}
		reconcile(t, pool)
		if blocksFingerprint(t, pool) != settled {
			t.Errorf("a reconcile after the runbook changed rows of context_blocks")
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
