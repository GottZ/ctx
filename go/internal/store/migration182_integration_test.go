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
	migration182File = "182_guard_pending_by_type.sql"
	// m182IndexDef and m181IndexDef: idx_guard_pending of migration 182 and
	// before it, as PostgreSQL deparses them (the DO block of 182 compares
	// these strings).
	m182IndexDef = "CREATE INDEX idx_guard_pending ON public.context_blocks USING btree (type_name, created_at) WHERE ((NOT is_archived) AND ((metadata ->> 'guard_checked_at'::text) IS NULL) AND (embedding IS NOT NULL) AND (NOT embedding_stale) AND ((category)::text <> 'index'::text) AND ((lifecycle_state)::text = 'knowledge'::text))"
	m181IndexDef = "CREATE INDEX idx_guard_pending ON public.context_blocks USING btree (created_at) WHERE ((NOT is_archived) AND ((metadata ->> 'guard_checked_at'::text) IS NULL) AND (embedding IS NOT NULL))"
)

// m182State is idx_guard_pending as the catalog holds it.
type m182State struct {
	def     string
	oid     uint32
	valid   bool
	entries int64 // pg_class.reltuples of the index: the tuples its build indexed
}

func m182Index(t *testing.T, pool *pgxpool.Pool) m182State {
	t.Helper()
	var s m182State
	var entries float64
	if err := pool.QueryRow(context.Background(),
		`SELECT pg_get_indexdef(c.oid), c.oid, i.indisvalid, c.reltuples FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace JOIN pg_index i ON i.indexrelid = c.oid
		  WHERE n.nspname = 'public' AND c.relname = 'idx_guard_pending'`).Scan(&s.def, &s.oid, &s.valid, &entries); err != nil {
		t.Fatalf("idx_guard_pending: %v", err)
	}
	s.entries = int64(entries)
	return s
}

// m182Runbook runs the out-of-band build of the header of 182, statement by
// statement outside a transaction, as an operator does.
func m182Runbook(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, st := range []string{
		`CREATE INDEX CONCURRENTLY idx_guard_pending_360 ON context_blocks (type_name, created_at)
		  WHERE NOT is_archived AND (metadata->>'guard_checked_at') IS NULL AND embedding IS NOT NULL
		    AND NOT embedding_stale AND category <> 'index' AND lifecycle_state = 'knowledge'`,
		`DROP INDEX CONCURRENTLY idx_guard_pending`,
		`ALTER INDEX idx_guard_pending_360 RENAME TO idx_guard_pending`,
	} {
		if _, err := pool.Exec(context.Background(), st); err != nil {
			t.Fatalf("runbook %q: %v", st, err)
		}
	}
}

// m182Run is what one application of the file in a transaction showed.
type m182Run struct {
	took         time.Duration
	modes        []string // relation locks on context_blocks held to the commit
	readerWaited bool     // a reader of context_blocks waited for the open file
	warnings     []string // WARNINGs naming idx_guard_pending
}

// m182Apply applies the file in one transaction, as the runner does, over a
// pool of its own whose notice handler collects the WARNINGs of the DO
// block; with reltuples > 0 it first sets the row estimate of context_blocks
// in the same transaction (the threshold reads it). While the file is open it
// holds, a second connection reads context_blocks.
func m182Apply(t *testing.T, dsn string, reader *pgxpool.Pool, body []byte, reltuples float64) m182Run {
	t.Helper()
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var run m182Run
	cfg.ConnConfig.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		mu.Lock()
		defer mu.Unlock()
		if n.Severity == "WARNING" && strings.Contains(n.Message, "idx_guard_pending") {
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

// m182Exclusive reports whether the file held ACCESS EXCLUSIVE on
// context_blocks.
func m182Exclusive(modes []string) bool {
	for _, m := range modes {
		if m == "AccessExclusiveLock" {
			return true
		}
	}
	return false
}

// m182Seed writes one block of every kind the guard pick sees: waiting
// (knowledge, reference), and with a vector but filtered by the pick — a type
// with guard.check = false, the synthesis lifecycle, a stale vector, category
// index — plus a checked, an archived and an unembedded block. It returns
// how many rows the predicate of migration 182 holds (the waiting blocks of
// every type, so tool-evidence too).
func m182Seed(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO context_blocks
			(category, title, content, scope, type_name, lifecycle_state, embedding, embed_model, embedding_stale, is_archived, metadata)
		SELECT c, 'm182 ' || k, 'content ' || k, 'm182', ty, lc,
		       CASE WHEN vec THEN array_fill(0.01::real, ARRAY[1024])::vector(1024) END,
		       CASE WHEN vec THEN 'q360' END, stale, arch, md::jsonb
		  FROM (VALUES
		    ('waiting knowledge', 'm182', 'knowledge',     'knowledge', true,  false, false, '{}'),
		    ('waiting reference', 'm182', 'reference',     'knowledge', true,  false, false, '{}'),
		    ('unchecked type',    'm182', 'tool-evidence', 'knowledge', true,  false, false, '{}'),
		    ('synthesis',         'm182', 'knowledge',     'synthesis', true,  false, false, '{}'),
		    ('stale',             'm182', 'knowledge',     'knowledge', true,  true,  false, '{}'),
		    ('category index',    'index', 'system-meta',  'knowledge', true,  false, false, '{}'),
		    ('checked',           'm182', 'knowledge',     'knowledge', true,  false, false, '{"guard_checked_at": "2026-11-18T00:00:00Z", "guard_status": "clean"}'),
		    ('archived',          'm182', 'knowledge',     'knowledge', true,  false, true,  '{}'),
		    ('no vector',         'm182', 'knowledge',     'knowledge', false, false, false, '{}')
		  ) AS v(k, c, ty, lc, vec, stale, arch, md)`); err != nil {
		t.Fatal(err)
	}
	return 3
}

func m182Fingerprint(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(),
		`SELECT md5(string_agg(id::text || ':' || xmin::text, ',' ORDER BY id)) FROM context_blocks`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestMigration182 applies migration 182 (issue #360) to a database at 181:
// idx_guard_pending becomes (type_name, created_at) with every condition of
// the guard pick the catalog can hold, and holds exactly the unchecked
// blocks with a current vector of the lifecycle knowledge outside category
// index, of every type. No block row is written. The ways through the DO
// block: below 500 000 rows (pg_class.reltuples) the index of 181, an
// INVALID index of either definition or a missing one is rebuilt inline,
// the DROP first (ACCESS EXCLUSIVE, a reader waits); an index built out of
// band with the runbook is adopted, a second application changes nothing,
// and above the threshold or on a same-named index of another definition
// the file only warns and takes no lock on context_blocks.
func TestMigration182(t *testing.T) {
	ctx := context.Background()
	body, err := migrations.FS.ReadFile(migration182File)
	if err != nil {
		t.Fatalf("read %s: %v", migration182File, err)
	}

	t.Run("stock", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 181)
		want := m182Seed(t, pool)
		if ix := m182Index(t, pool); ix.def != m181IndexDef {
			t.Fatalf("fixture: idx_guard_pending at 181 is %s", ix.def)
		}
		before := m182Fingerprint(t, pool)
		if err := store.RunMigrations(ctx, pool); err != nil {
			t.Fatalf("run migrations: %v", err)
		}
		if m182Fingerprint(t, pool) != before {
			t.Errorf("migration 182 wrote rows of context_blocks")
		}
		ix := m182Index(t, pool)
		if ix.def != m182IndexDef || !ix.valid {
			t.Errorf("idx_guard_pending deparses as\n  %s (valid=%v)\nwant\n  %s", ix.def, ix.valid, m182IndexDef)
		}
		if ix.entries != want {
			t.Errorf("idx_guard_pending holds %d entries, want %d (waiting knowledge, waiting reference, unchecked type)", ix.entries, want)
		}
		// The columns the index binds, by its key and its predicate: the
		// embedding column among them, so the index is in the attnum class of
		// the re-embed cutover (its twin: events.verifySisterIndexes).
		var bound string
		if err := pool.QueryRow(ctx, `SELECT string_agg(a.attname, ',' ORDER BY a.attname)
			  FROM pg_index i
			  JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum > 0 AND NOT a.attisdropped
			 WHERE i.indexrelid = 'idx_guard_pending'::regclass
			   AND (a.attnum = ANY (i.indkey::int2[])
			        OR pg_get_expr(i.indpred, i.indrelid) ~ ('\m' || a.attname || '\M'))`).Scan(&bound); err != nil {
			t.Fatal(err)
		}
		if want := "category,created_at,embedding,embedding_stale,is_archived,lifecycle_state,metadata,type_name"; bound != want {
			t.Errorf("idx_guard_pending binds %s, want %s", bound, want)
		}

		// Applied once more (the runner skips an applied version; the file
		// itself is idempotent).
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
		if again := m182Index(t, pool); again.oid != ix.oid || again.def != m182IndexDef {
			t.Errorf("the second application rebuilt the index: oid %d -> %d, %s", ix.oid, again.oid, again.def)
		}
	})

	t.Run("rebuild_below_threshold", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 181)
		m182Seed(t, pool)
		run := m182Apply(t, dsn, pool, body, 0)
		t.Logf("migration 182 file over the index of 181: %s; relation locks on context_blocks held to its commit: %v", run.took, run.modes)
		if !m182Exclusive(run.modes) {
			t.Errorf("the rebuild holds %v on context_blocks, want ACCESS EXCLUSIVE (DROP INDEX)", run.modes)
		}
		if !run.readerWaited {
			t.Errorf("a reader of context_blocks did not wait for the open rebuild")
		}
		if len(run.warnings) != 0 {
			t.Errorf("warnings below the threshold: %v", run.warnings)
		}
		if ix := m182Index(t, pool); ix.def != m182IndexDef || !ix.valid {
			t.Errorf("idx_guard_pending after the file: %s (valid=%v)", ix.def, ix.valid)
		}
	})

	// A transaction that has read context_blocks when the file begins and
	// then writes it: the file asks for ACCESS EXCLUSIVE (the DROP) before any
	// other lock it holds to its commit, so the write goes ahead of the waiting
	// DROP and both commit.
	t.Run("reader_that_writes", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 181)
		m182Seed(t, pool)
		var id string
		if err := pool.QueryRow(ctx, `SELECT id FROM context_blocks WHERE title LIKE 'm182 %' ORDER BY id LIMIT 1`).Scan(&id); err != nil {
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
		if ix := m182Index(t, pool); ix.def != m182IndexDef {
			t.Errorf("idx_guard_pending after the file: %s", ix.def)
		}
	})

	// An INVALID index (an aborted CREATE INDEX CONCURRENTLY under the name)
	// of either definition, or none at all: below the threshold the file
	// builds the index of 182.
	for _, c := range []struct {
		name  string
		setup []string
	}{
		{"invalid_181", []string{`UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'idx_guard_pending'::regclass`}},
		{"invalid_182", []string{`DROP INDEX idx_guard_pending`,
			`CREATE INDEX idx_guard_pending ON context_blocks (type_name, created_at)
			  WHERE NOT is_archived AND (metadata->>'guard_checked_at') IS NULL AND embedding IS NOT NULL
			    AND NOT embedding_stale AND category <> 'index' AND lifecycle_state = 'knowledge'`,
			`UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'idx_guard_pending'::regclass`}},
		{"missing", []string{`DROP INDEX idx_guard_pending`}},
	} {
		t.Run(c.name+"_below_threshold", func(t *testing.T) {
			pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 181)
			want := m182Seed(t, pool)
			for _, s := range c.setup {
				if _, err := pool.Exec(ctx, s); err != nil {
					t.Fatalf("%s: %v", s, err)
				}
			}
			run := m182Apply(t, dsn, pool, body, 0)
			if len(run.warnings) != 0 {
				t.Errorf("warnings: %v", run.warnings)
			}
			if ix := m182Index(t, pool); ix.def != m182IndexDef || !ix.valid || ix.entries != want {
				t.Errorf("idx_guard_pending after the file: %s (valid=%v, %d entries, want %d)", ix.def, ix.valid, ix.entries, want)
			}
		})
	}

	t.Run("built_out_of_band", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 181)
		m182Seed(t, pool)
		m182Runbook(t, pool)
		before := m182Index(t, pool)
		run := m182Apply(t, dsn, pool, body, 0)
		t.Logf("migration 182 file over an index built out of band: %s; relation locks on context_blocks held to its commit: %v", run.took, run.modes)
		if m182Exclusive(run.modes) || run.readerWaited || len(run.warnings) != 0 {
			t.Errorf("over the index built out of band the file holds %v (reader waited %v, warnings %v)", run.modes, run.readerWaited, run.warnings)
		}
		if ix := m182Index(t, pool); ix.def != m182IndexDef || ix.oid != before.oid {
			t.Errorf("the index built out of band was not adopted: oid %d -> %d, %s", before.oid, ix.oid, ix.def)
		}
	})

	t.Run("above_threshold", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 181)
		m182Seed(t, pool)
		run := m182Apply(t, dsn, pool, body, 600000)
		t.Logf("migration 182 file at 600 000 estimated rows: %s; relation locks on context_blocks held to its commit: %v; warnings %v", run.took, run.modes, run.warnings)
		if len(run.warnings) != 1 {
			t.Errorf("warnings %v, want one naming idx_guard_pending", run.warnings)
		}
		if m182Exclusive(run.modes) || run.readerWaited {
			t.Errorf("above the threshold the file holds %v on context_blocks (reader waited %v)", run.modes, run.readerWaited)
		}
		if ix := m182Index(t, pool); ix.def != m181IndexDef {
			t.Fatalf("above the threshold the index became %s", ix.def)
		}
		// The runbook gives the index the definition of 182; the file then
		// adopts it.
		m182Runbook(t, pool)
		built := m182Index(t, pool)
		if built.def != m182IndexDef {
			t.Fatalf("after the runbook the index is %s", built.def)
		}
		again := m182Apply(t, dsn, pool, body, 600000)
		if len(again.warnings) != 0 || m182Exclusive(again.modes) {
			t.Errorf("after the runbook the file warns %v and holds %v", again.warnings, again.modes)
		}
		if ix := m182Index(t, pool); ix.oid != built.oid {
			t.Errorf("after the runbook the file rebuilt the index: oid %d -> %d", built.oid, ix.oid)
		}
	})

	t.Run("foreign_definition", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 181)
		m182Seed(t, pool)
		const foreign = "CREATE INDEX idx_guard_pending ON public.context_blocks USING btree (title)"
		for _, s := range []string{`DROP INDEX idx_guard_pending`, `CREATE INDEX idx_guard_pending ON context_blocks (title)`} {
			if _, err := pool.Exec(ctx, s); err != nil {
				t.Fatal(err)
			}
		}
		run := m182Apply(t, dsn, pool, body, 0)
		if len(run.warnings) != 1 || m182Exclusive(run.modes) || run.readerWaited {
			t.Errorf("over a foreign definition: warnings %v, locks %v, reader waited %v; want one warning and no lock", run.warnings, run.modes, run.readerWaited)
		}
		if ix := m182Index(t, pool); ix.def != foreign {
			t.Errorf("the foreign index became %s", ix.def)
		}
	})
}
