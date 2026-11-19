//go:build integration

package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	migration182File = "182_guard_pending_type.sql"
	// m182IndexDef is idx_guard_pending_type as PostgreSQL deparses it (the
	// DO block of 182 compares this string); m181IndexDef is
	// idx_guard_pending, which 182 leaves as it is.
	m182IndexDef = "CREATE INDEX idx_guard_pending_type ON public.context_blocks USING btree (type_name, timezone('UTC'::text, created_at)) WHERE ((NOT is_archived) AND ((metadata ->> 'guard_checked_at'::text) IS NULL) AND (embedding IS NOT NULL) AND (NOT embedding_stale) AND ((category)::text <> 'index'::text) AND ((lifecycle_state)::text = 'knowledge'::text))"
	m181IndexDef = "CREATE INDEX idx_guard_pending ON public.context_blocks USING btree (created_at) WHERE ((NOT is_archived) AND ((metadata ->> 'guard_checked_at'::text) IS NULL) AND (embedding IS NOT NULL))"
	// m182Runbook is the out-of-band build of the header of 182, statement
	// by statement outside a transaction, as an operator runs it.
	m182DDL = `CREATE INDEX CONCURRENTLY idx_guard_pending_type ON context_blocks (type_name, timezone('UTC', created_at))
		  WHERE NOT is_archived AND (metadata->>'guard_checked_at') IS NULL AND embedding IS NOT NULL
		    AND NOT embedding_stale AND category <> 'index' AND lifecycle_state = 'knowledge'`
)

// m182State is an index as the catalog holds it.
type m182State struct {
	found   bool
	def     string
	oid     uint32
	valid   bool
	entries int64 // pg_class.reltuples of the index: the tuples its build indexed
}

func m182Index(t *testing.T, pool *pgxpool.Pool, name string) m182State {
	t.Helper()
	var s m182State
	var entries float64
	err := pool.QueryRow(context.Background(),
		`SELECT pg_get_indexdef(c.oid), c.oid, i.indisvalid, c.reltuples FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace JOIN pg_index i ON i.indexrelid = c.oid
		  WHERE n.nspname = 'public' AND c.relname = $1`, name).Scan(&s.def, &s.oid, &s.valid, &entries)
	if errors.Is(err, pgx.ErrNoRows) {
		return s
	}
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	s.found, s.entries = true, int64(entries)
	return s
}

func m182Runbook(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, st := range []string{`DROP INDEX CONCURRENTLY IF EXISTS idx_guard_pending_type`, m182DDL} {
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
	writerWaited bool     // a writer of context_blocks waited for the open file
	warnings     []string // WARNINGs naming idx_guard_pending_type
}

func m182Exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// m182Apply applies the file in one transaction, as the runner does, over a
// pool of its own whose notice handler collects the WARNINGs of the DO
// block; with reltuples > 0 it first sets the row estimate of context_blocks
// in the same transaction (the threshold reads it). While the file is open
// it holds, a second connection reads context_blocks and a third writes it,
// each with a short timeout.
func m182Apply(t *testing.T, dsn string, other *pgxpool.Pool, body []byte, reltuples float64) m182Run {
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
		if n.Severity == "WARNING" && strings.Contains(n.Message, "idx_guard_pending_type") {
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
	rows, err := other.Query(ctx,
		`SELECT l.mode FROM pg_locks l WHERE l.pid = $1 AND l.locktype = 'relation' AND l.relation = 'context_blocks'::regclass ORDER BY 1`, pid)
	if err != nil {
		t.Fatal(err)
	}
	if run.modes, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	rctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	var n int
	run.readerWaited = other.QueryRow(rctx, `SELECT count(*) FROM context_blocks WHERE title LIKE 'm182 %'`).Scan(&n) != nil
	cancel()
	wctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	_, werr := other.Exec(wctx, `UPDATE context_blocks SET updated_at = updated_at WHERE title = 'm182 checked'`)
	run.writerWaited = werr != nil
	cancel()
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return run
}

func m182Holds(modes []string, mode string) bool {
	for _, m := range modes {
		if m == mode {
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
	m182Exec(t, pool, `INSERT INTO context_blocks
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
		  ) AS v(k, c, ty, lc, vec, stale, arch, md)`)
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

// m182Unchanged asserts idx_guard_pending is the index of 181, the object a
// binary before 182 reads.
func m182Unchanged(t *testing.T, pool *pgxpool.Pool, before m182State) {
	t.Helper()
	if ix := m182Index(t, pool, "idx_guard_pending"); ix.def != m181IndexDef || !ix.valid || ix.oid != before.oid {
		t.Errorf("idx_guard_pending changed: %s (valid=%v, oid %d -> %d)", ix.def, ix.valid, before.oid, ix.oid)
	}
}

// TestMigration182 applies migration 182 (issue #360) to a database at 181:
// idx_guard_pending_type is built beside idx_guard_pending, which stays as
// it is, with the deparsed definition the file compares against and exactly
// the unchecked blocks with a current vector of the lifecycle knowledge
// outside category index, of every type. No block row is written. The ways
// through the DO block: below 500 000 rows (pg_class.reltuples) the index is
// built inline under SHARE (readers go on, writers wait); an index built
// out of band with the runbook is adopted and a second application changes
// nothing; above the threshold, on an INVALID index of the name and on a
// same-named index of another definition the file only warns and takes no
// lock on context_blocks.
func TestMigration182(t *testing.T) {
	ctx := context.Background()
	body, err := migrations.FS.ReadFile(migration182File)
	if err != nil {
		t.Fatalf("read %s: %v", migration182File, err)
	}

	t.Run("stock", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 181)
		want := m182Seed(t, pool)
		old := m182Index(t, pool, "idx_guard_pending")
		if old.def != m181IndexDef {
			t.Fatalf("fixture: idx_guard_pending at 181 is %s", old.def)
		}
		if ix := m182Index(t, pool, "idx_guard_pending_type"); ix.found {
			t.Fatalf("fixture: idx_guard_pending_type exists at 181: %s", ix.def)
		}
		before := m182Fingerprint(t, pool)
		if err := store.RunMigrations(ctx, pool); err != nil {
			t.Fatalf("run migrations: %v", err)
		}
		if m182Fingerprint(t, pool) != before {
			t.Errorf("migration 182 wrote rows of context_blocks")
		}
		ix := m182Index(t, pool, "idx_guard_pending_type")
		if ix.def != m182IndexDef || !ix.valid {
			t.Errorf("idx_guard_pending_type deparses as\n  %s (valid=%v)\nwant\n  %s", ix.def, ix.valid, m182IndexDef)
		}
		if ix.entries != want {
			t.Errorf("idx_guard_pending_type holds %d entries, want %d (waiting knowledge, waiting reference, unchecked type)", ix.entries, want)
		}
		m182Unchanged(t, pool, old)
		// The deparsed definition does not depend on the session time zone:
		// the file and the schema-contract manifest compare it as a string.
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, tz := range []string{"Europe/Berlin", "America/Los_Angeles", "Asia/Kathmandu"} {
			var def string
			if _, err := conn.Exec(ctx, `SET TimeZone = '`+tz+`'`); err != nil {
				t.Fatal(err)
			}
			if err := conn.QueryRow(ctx, `SELECT pg_get_indexdef('idx_guard_pending_type'::regclass)`).Scan(&def); err != nil {
				t.Fatal(err)
			}
			if def != m182IndexDef {
				t.Errorf("in time zone %s idx_guard_pending_type deparses as %s", tz, def)
			}
		}
		_, _ = conn.Exec(ctx, `RESET TimeZone`)
		conn.Release()
		// The columns the index binds, by its key and its predicate: the
		// embedding column among them, so the index is in the attnum class of
		// the re-embed cutover (its twin: events.verifySisterIndexes).
		var bound string
		if err := pool.QueryRow(ctx, `SELECT string_agg(DISTINCT a.attname, ',' ORDER BY a.attname)
			  FROM pg_index i
			  JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum > 0 AND NOT a.attisdropped
			 WHERE i.indexrelid = 'idx_guard_pending_type'::regclass
			   AND (a.attnum = ANY (i.indkey::int2[])
			        OR pg_get_expr(i.indexprs, i.indrelid) ~ ('\m' || a.attname || '\M')
			        OR pg_get_expr(i.indpred, i.indrelid) ~ ('\m' || a.attname || '\M'))`).Scan(&bound); err != nil {
			t.Fatal(err)
		}
		if want := "category,created_at,embedding,embedding_stale,is_archived,lifecycle_state,metadata,type_name"; bound != want {
			t.Errorf("idx_guard_pending_type binds %s, want %s", bound, want)
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
		if again := m182Index(t, pool, "idx_guard_pending_type"); again.oid != ix.oid || again.def != m182IndexDef {
			t.Errorf("the second application rebuilt the index: oid %d -> %d, %s", ix.oid, again.oid, again.def)
		}
	})

	// Below the threshold the file builds the index inline: SHARE on
	// context_blocks until it commits — a reader goes on, a writer waits —
	// and nothing stronger.
	t.Run("inline_below_threshold", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 181)
		m182Seed(t, pool)
		old := m182Index(t, pool, "idx_guard_pending")
		run := m182Apply(t, dsn, pool, body, 0)
		t.Logf("migration 182 inline: %s; relation locks on context_blocks held to its commit: %v; reader waited %v, writer waited %v", run.took, run.modes, run.readerWaited, run.writerWaited)
		if !m182Holds(run.modes, "ShareLock") || m182Holds(run.modes, "AccessExclusiveLock") || m182Holds(run.modes, "ExclusiveLock") {
			t.Errorf("the inline build holds %v on context_blocks, want SHARE and nothing stronger", run.modes)
		}
		if run.readerWaited {
			t.Errorf("a reader of context_blocks waited for the open build")
		}
		if !run.writerWaited {
			t.Errorf("a writer of context_blocks did not wait for the open build (SHARE)")
		}
		if len(run.warnings) != 0 {
			t.Errorf("warnings below the threshold: %v", run.warnings)
		}
		if ix := m182Index(t, pool, "idx_guard_pending_type"); ix.def != m182IndexDef || !ix.valid {
			t.Errorf("idx_guard_pending_type after the file: %s (valid=%v)", ix.def, ix.valid)
		}
		m182Unchanged(t, pool, old)
	})

	// A transaction that has read context_blocks when the file begins and
	// then writes it: SHARE does not conflict with the reader's lock, so the
	// build goes ahead, the write waits for the file's commit, and both
	// commit — no deadlock.
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
		mtx, err := mpool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = mtx.Rollback(ctx) }()
		if _, err := mtx.Exec(ctx, string(body)); err != nil {
			t.Fatalf("the migration beside a reader: %v", err)
		}
		wrote := make(chan error, 1)
		go func() {
			_, err := reader.Exec(ctx, `UPDATE context_blocks SET title = title WHERE id = $1`, id)
			wrote <- err
		}()
		select {
		case err := <-wrote:
			t.Fatalf("the reader's write went through while the build was open (%v)", err)
		case <-time.After(300 * time.Millisecond):
		}
		if err := mtx.Commit(ctx); err != nil {
			t.Fatalf("the migration's commit: %v", err)
		}
		select {
		case err := <-wrote:
			if err != nil {
				t.Fatalf("the reader's write after the build: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the reader's write did not go through after the build committed")
		}
		if err := reader.Commit(ctx); err != nil {
			t.Fatalf("the reader's commit: %v", err)
		}
		if ix := m182Index(t, pool, "idx_guard_pending_type"); ix.def != m182IndexDef || !ix.valid {
			t.Errorf("idx_guard_pending_type after the file: %s (valid=%v)", ix.def, ix.valid)
		}
	})

	t.Run("built_out_of_band", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 181)
		m182Seed(t, pool)
		m182Runbook(t, pool)
		before := m182Index(t, pool, "idx_guard_pending_type")
		run := m182Apply(t, dsn, pool, body, 0)
		if m182Holds(run.modes, "ShareLock") || run.readerWaited || run.writerWaited || len(run.warnings) != 0 {
			t.Errorf("over the index built out of band the file holds %v (reader waited %v, writer waited %v, warnings %v)", run.modes, run.readerWaited, run.writerWaited, run.warnings)
		}
		if ix := m182Index(t, pool, "idx_guard_pending_type"); ix.def != m182IndexDef || ix.oid != before.oid {
			t.Errorf("the index built out of band was not adopted: oid %d -> %d, %s", before.oid, ix.oid, ix.def)
		}
	})

	// Above the threshold, an INVALID index of the name (an aborted
	// CONCURRENTLY build) and a same-named index of another definition: one
	// WARNING, no lock on context_blocks, nothing built or dropped. The
	// runbook then gives the index of 182, and the file adopts it.
	for _, c := range []struct {
		name      string
		setup     []string
		reltuples float64
		want      string // the definition the name holds after the file
	}{
		{"above_threshold", nil, 600000, ""},
		{"invalid", []string{m182DDL[len("CREATE INDEX CONCURRENTLY "):], `UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'idx_guard_pending_type'::regclass`}, 0, m182IndexDef},
		{"foreign_definition", []string{`idx_guard_pending_type ON context_blocks (title)`}, 0, "CREATE INDEX idx_guard_pending_type ON public.context_blocks USING btree (title)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 181)
			m182Seed(t, pool)
			old := m182Index(t, pool, "idx_guard_pending")
			for _, s := range c.setup {
				if !strings.HasPrefix(s, "UPDATE") {
					s = "CREATE INDEX " + s
				}
				m182Exec(t, pool, s)
			}
			before := m182Index(t, pool, "idx_guard_pending_type")
			run := m182Apply(t, dsn, pool, body, c.reltuples)
			t.Logf("%s: %s; locks %v; warnings %v", c.name, run.took, run.modes, run.warnings)
			if len(run.warnings) != 1 {
				t.Errorf("warnings %v, want one naming idx_guard_pending_type", run.warnings)
			}
			if m182Holds(run.modes, "ShareLock") || run.readerWaited || run.writerWaited {
				t.Errorf("the file holds %v on context_blocks (reader waited %v, writer waited %v)", run.modes, run.readerWaited, run.writerWaited)
			}
			after := m182Index(t, pool, "idx_guard_pending_type")
			if after.found != before.found || after.oid != before.oid || after.def != c.want && c.want != "" {
				t.Errorf("the file changed idx_guard_pending_type: %+v -> %+v", before, after)
			}
			m182Unchanged(t, pool, old)
			m182Runbook(t, pool)
			built := m182Index(t, pool, "idx_guard_pending_type")
			if built.def != m182IndexDef || !built.valid {
				t.Fatalf("after the runbook the index is %s (valid=%v)", built.def, built.valid)
			}
			again := m182Apply(t, dsn, pool, body, c.reltuples)
			if len(again.warnings) != 0 || m182Holds(again.modes, "ShareLock") {
				t.Errorf("after the runbook the file warns %v and holds %v", again.warnings, again.modes)
			}
			if ix := m182Index(t, pool, "idx_guard_pending_type"); ix.oid != built.oid {
				t.Errorf("after the runbook the file rebuilt the index: oid %d -> %d", built.oid, ix.oid)
			}
		})
	}
}

// TestMigration182InlineBuildWriterWait is the measurement behind the
// threshold of 182 (not a gate): a store grown to each size of
// CTX_M182_ROWS (default 250000,500000,1000000) of rows of live width (about
// 1.5 KB of heap, 5 % with a vector), a writer inserting a block every 2 ms,
// and the inline build of 182 — the file's CREATE INDEX statement, in one
// transaction — three times per size; it logs the build time and the longest
// wait of the writer. Set CTX_M182_MEASURE=1.
func TestMigration182InlineBuildWriterWait(t *testing.T) {
	if os.Getenv("CTX_M182_MEASURE") == "" {
		t.Skip("measurement; set CTX_M182_MEASURE=1")
	}
	ctx := context.Background()
	sizes := []int{250000, 500000, 1000000}
	if v := os.Getenv("CTX_M182_ROWS"); v != "" {
		sizes = nil
		for _, f := range strings.Split(v, ",") {
			var n int
			if _, err := fmt.Sscan(f, &n); err != nil {
				t.Fatal(err)
			}
			sizes = append(sizes, n)
		}
	}
	// The statement the DO block of 182 runs below the threshold.
	build := "CREATE INDEX " + m182DDL[len("CREATE INDEX CONCURRENTLY "):]
	pool := testdb.SetupTestDBUpTo(t, 181)
	m182Exec(t, pool, `DROP INDEX IF EXISTS idx_embedding_hnsw`)
	have := 0
	for _, rows := range sizes {
		t0 := time.Now()
		m182Exec(t, pool, `ALTER TABLE context_blocks DISABLE TRIGGER USER`)
		m182Exec(t, pool, `ALTER TABLE context_blocks ENABLE TRIGGER trg_block_fulltext`)
		m182Exec(t, pool, `INSERT INTO context_blocks (category, title, content, scope, type_name, embedding, embed_model, metadata, created_at, updated_at)
			SELECT 'm182w', 'm182w ' || g, repeat('lorem ipsum dolor sit amet ', 40) || g, 'm182-' || (g % 100),
			       CASE WHEN g % 3 = 0 THEN 'reference' ELSE 'knowledge' END,
			       CASE WHEN g % 20 = 0 THEN array_fill(0.01::real, ARRAY[1024])::vector(1024) END, CASE WHEN g % 20 = 0 THEN 'q360' END,
			       jsonb_build_object('pad', repeat('m', 260)), now() - g * interval '1 second', now()
			  FROM generate_series($1::int + 1, $2::int) g`, have, rows)
		have = rows
		m182Exec(t, pool, `ALTER TABLE context_blocks ENABLE TRIGGER USER`)
		m182Exec(t, pool, `VACUUM ANALYZE context_blocks`)
		var heap int64
		if err := pool.QueryRow(ctx, `SELECT pg_relation_size('context_blocks')`).Scan(&heap); err != nil {
			t.Fatal(err)
		}
		t.Logf("seed to %d rows, heap %d MB, in %s", rows, heap>>20, time.Since(t0).Round(time.Second))
		for round := 1; round <= 3; round++ {
			m182Exec(t, pool, `DROP INDEX IF EXISTS idx_guard_pending_type`)
			stop := make(chan struct{})
			var longest time.Duration
			var writes int
			done := make(chan struct{})
			go func() {
				defer close(done)
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					default:
					}
					w0 := time.Now()
					if _, err := pool.Exec(ctx, `INSERT INTO context_blocks (category, title, content, scope) VALUES ('m182w', $1, 'writer', 'm182-w')`, fmt.Sprintf("m182 writer %d %d %d", rows, round, i)); err != nil {
						t.Errorf("writer: %v", err)
						return
					}
					if d := time.Since(w0); d > longest {
						longest = d
					}
					writes++
					time.Sleep(2 * time.Millisecond)
				}
			}()
			time.Sleep(200 * time.Millisecond)
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '3s'`); err != nil {
				t.Fatal(err)
			}
			b0 := time.Now()
			if _, err := tx.Exec(ctx, build); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			took := time.Since(b0)
			time.Sleep(200 * time.Millisecond)
			close(stop)
			<-done
			ix := m182Index(t, pool, "idx_guard_pending_type")
			var load string
			if b, err := os.ReadFile("/proc/loadavg"); err == nil {
				load = strings.Join(strings.Fields(string(b))[:3], " ")
			}
			t.Logf("MEASURE|rows=%d|heap_mb=%d|round=%d|build=%s|writer_longest_wait=%s|writes=%d|index_valid=%v|load=%s",
				rows, heap>>20, round, took.Round(time.Millisecond), longest.Round(time.Millisecond), writes, ix.valid, load)
			if !ix.valid || ix.def != m182IndexDef {
				t.Errorf("the build left %s (valid=%v)", ix.def, ix.valid)
			}
		}
	}
}
