//go:build integration

package store_test

import (
	"context"
	"errors"
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

const migration183File = "183_guard_pending_dropped.sql"

// m18xRun is what one application of a migration file in a transaction
// showed: its time, the relation locks it held on the table to its commit,
// whether a reader and a writer of that table on a second connection waited
// for it, and the WARNINGs and NOTICEs it raised.
type m18xRun struct {
	took         time.Duration
	err          error
	modes        []string
	readerWaited bool
	writerWaited bool
	warnings     []string
	notices      []string
}

// m18xApply applies body in one transaction, as the runner does, over a pool
// of its own whose notice handler collects the WARNINGs and NOTICEs; with
// reltuples > 0 it first sets the row estimate of table in the same
// transaction (the thresholds read it). While the file is open it holds,
// other reads table (readSQL) and writes it (writeSQL); m18xWaits tells
// whether each waited for the file. A failing file is rolled back and its
// error returned.
func m18xApply(t *testing.T, dsn string, other *pgxpool.Pool, body []byte, table string, reltuples float64, readSQL, writeSQL string) m18xRun {
	t.Helper()
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var run m18xRun
	cfg.ConnConfig.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		mu.Lock()
		defer mu.Unlock()
		switch n.Severity {
		case "WARNING":
			run.warnings = append(run.warnings, n.Message)
		case "NOTICE":
			run.notices = append(run.notices, n.Message)
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
		if _, err := tx.Exec(ctx, `UPDATE pg_class SET reltuples = $1 WHERE oid = $2::regclass`, reltuples, table); err != nil {
			t.Fatalf("set reltuples: %v", err)
		}
	}
	start := time.Now()
	if _, run.err = tx.Exec(ctx, string(body)); run.err != nil {
		run.took = time.Since(start)
		return run
	}
	run.took = time.Since(start)
	rows, err := other.Query(ctx,
		`SELECT l.mode FROM pg_locks l WHERE l.pid = $1 AND l.locktype = 'relation' AND l.relation = $2::regclass ORDER BY 1`, pid, table)
	if err != nil {
		t.Fatal(err)
	}
	if run.modes, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	if readSQL != "" {
		run.readerWaited = m18xWaits(t, other, readSQL, table, "AccessShareLock")
	}
	if writeSQL != "" {
		run.writerWaited = m18xWaits(t, other, writeSQL, table, "RowExclusiveLock")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return run
}

// m18xProbeWithin bounds one probe of m18xWaits: the time for its statement
// to end or for its lock request to show in pg_locks. Either happens within
// milliseconds; the bound only ends a probe that does neither.
const m18xProbeWithin = 5 * time.Second

// m18xWaits runs sql on a connection of its own from other while an open
// transaction holds its locks, and reports whether the statement waited for
// a lock of mode on table: true when pg_locks shows that request of its
// backend not granted (the statement is then cancelled on the server), false
// when the statement ends without an error first. No connection, an error of
// the statement, or neither within m18xProbeWithin fails the test with the
// requests pg_locks showed: none of them is evidence either way.
func m18xWaits(t *testing.T, other *pgxpool.Pool, sql, table, mode string) bool {
	t.Helper()
	ctx := context.Background()
	actx, acancel := context.WithTimeout(ctx, m18xProbeWithin)
	conn, err := other.Acquire(actx)
	acancel()
	if err != nil {
		t.Fatalf("probe of %s: no connection: %v", table, err)
	}
	defer conn.Release()
	var pid int
	if err := conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("probe of %s: %v", table, err)
	}
	sctx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() {
		_, err := conn.Exec(sctx, sql)
		done <- err
	}()
	// end cancels the statement on the server and returns how it ended; the
	// context is the fallback should the cancel not reach it.
	end := func() error {
		if _, err := other.Exec(ctx, `SELECT pg_cancel_backend($1)`, pid); err != nil {
			stop()
		}
		select {
		case err := <-done:
			return err
		case <-time.After(m18xProbeWithin):
			stop()
			return <-done
		}
	}
	deadline := time.Now().Add(m18xProbeWithin)
	var pending []string
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("probe of %s ended with %v before its request for %s was seen waiting (pending %v): no evidence either way", table, err, mode, pending)
			}
			return false
		default:
		}
		rows, err := other.Query(ctx, `SELECT l.locktype || ' ' || l.mode || COALESCE(' on ' || l.relation::regclass::text, ''),
			COALESCE(l.locktype = 'relation' AND l.relation = $2::regclass AND l.mode = $3, false)
			  FROM pg_locks l WHERE l.pid = $1 AND NOT l.granted ORDER BY 1`, pid, table, mode)
		if err != nil {
			serr := end()
			t.Fatalf("probe of %s: read pg_locks: %v (the probe ended with %v)", table, err, serr)
		}
		pending = pending[:0]
		waited := false
		for rows.Next() {
			var lock string
			var match bool
			if err := rows.Scan(&lock, &match); err != nil {
				rows.Close()
				serr := end()
				t.Fatalf("probe of %s: %v (the probe ended with %v)", table, err, serr)
			}
			pending = append(pending, lock)
			waited = waited || match
		}
		rows.Close()
		if waited {
			var pgErr *pgconn.PgError
			if err := end(); !errors.As(err, &pgErr) || pgErr.Code != "57014" {
				t.Errorf("probe of %s waited for %s, then ended with %v, want its cancel (57014)", table, mode, err)
			}
			return true
		}
		if time.Now().After(deadline) {
			serr := end()
			t.Fatalf("probe of %s neither ended nor waited for %s within %s: pending %v, then ended with %v", table, mode, m18xProbeWithin, pending, serr)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func m18xWarns(run m18xRun, sub string) int {
	n := 0
	for _, w := range run.warnings {
		if strings.Contains(w, sub) {
			n++
		}
	}
	return n
}

const (
	m183Read  = `SELECT count(*) FROM context_blocks WHERE title LIKE 'm182 %'`
	m183Write = `UPDATE context_blocks SET updated_at = updated_at WHERE title = 'm182 checked'`
	// The twins of idx_guard_pending a re-embed cutover leaves under its two
	// other names, as PostgreSQL deparses them.
	m183NextDef = "CREATE INDEX idx_guard_pending_next ON public.context_blocks USING btree (created_at) WHERE ((NOT is_archived) AND ((metadata ->> 'guard_checked_at'::text) IS NULL) AND (embedding_next IS NOT NULL))"
	m183OldDef  = "CREATE INDEX idx_guard_pending_old ON public.context_blocks USING btree (created_at) WHERE ((NOT is_archived) AND ((metadata ->> 'guard_checked_at'::text) IS NULL) AND (embedding_old IS NOT NULL))"
)

// m183Twins builds idx_guard_pending_next (the verify twin of a binary before
// this release, on embedding_next) and idx_guard_pending_old (the leftover of
// a swap, on embedding_old — the column exists only between a swap and its
// cleanup, so the test adds it).
func m183Twins(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	m182Exec(t, pool, `CREATE INDEX idx_guard_pending_next ON context_blocks (created_at ASC)
		WHERE NOT is_archived AND (metadata->>'guard_checked_at') IS NULL AND embedding_next IS NOT NULL`)
	m182Exec(t, pool, `ALTER TABLE context_blocks ADD COLUMN IF NOT EXISTS embedding_old vector(1024)`)
	m182Exec(t, pool, `CREATE INDEX idx_guard_pending_old ON context_blocks (created_at ASC)
		WHERE NOT is_archived AND (metadata->>'guard_checked_at') IS NULL AND embedding_old IS NOT NULL`)
	for name, want := range map[string]string{"idx_guard_pending_next": m183NextDef, "idx_guard_pending_old": m183OldDef} {
		if ix := m182Index(t, pool, name); ix.def != want {
			t.Fatalf("fixture: %s is %s", name, ix.def)
		}
	}
}

// TestMigration183 applies migration 183 (issue #360) to a database at 182:
// idx_guard_pending and its re-embed twins go, idx_guard_pending_type stays as
// it is, no block row is written. The DROP holds ACCESS EXCLUSIVE on
// context_blocks to the commit (a reader and a writer wait), with nothing to
// drop the file takes no lock. idx_guard_pending stays, with one WARNING,
// while idx_guard_pending_type does not stand as 182 builds it (missing,
// INVALID, another definition): the pick then sends its statement before
// 182, which reads idx_guard_pending. A same-named index of another
// definition is left alone with a WARNING. The runbook (build the new index,
// then drop the old one CONCURRENTLY) ends in the state of the file.
func TestMigration183(t *testing.T) {
	ctx := context.Background()
	body, err := migrations.FS.ReadFile(migration183File)
	if err != nil {
		t.Fatalf("read %s: %v", migration183File, err)
	}

	t.Run("stock", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 182)
		m182Seed(t, pool)
		typ := m182Index(t, pool, "idx_guard_pending_type")
		if old := m182Index(t, pool, "idx_guard_pending"); old.def != m181IndexDef || typ.def != m182IndexDef || !typ.valid {
			t.Fatalf("fixture at 182: %s / %s", old.def, typ.def)
		}
		before := m182Fingerprint(t, pool)
		if err := store.RunMigrationsUpTo(ctx, pool, 183); err != nil {
			t.Fatalf("run migrations: %v", err)
		}
		if m182Fingerprint(t, pool) != before {
			t.Errorf("migration 183 wrote rows of context_blocks")
		}
		if ix := m182Index(t, pool, "idx_guard_pending"); ix.found {
			t.Errorf("idx_guard_pending survived: %s", ix.def)
		}
		if ix := m182Index(t, pool, "idx_guard_pending_type"); ix.oid != typ.oid || ix.def != m182IndexDef || !ix.valid {
			t.Errorf("idx_guard_pending_type changed: oid %d -> %d, %s (valid=%v)", typ.oid, ix.oid, ix.def, ix.valid)
		}
	})

	t.Run("inline_drop", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 182)
		m182Seed(t, pool)
		m183Twins(t, pool)
		typ := m182Index(t, pool, "idx_guard_pending_type")
		run := m18xApply(t, dsn, pool, body, "context_blocks", 0, m183Read, m183Write)
		t.Logf("migration 183 inline: %s; locks on context_blocks held to its commit: %v; reader waited %v, writer waited %v; warnings %v",
			run.took, run.modes, run.readerWaited, run.writerWaited, run.warnings)
		if run.err != nil {
			t.Fatalf("apply: %v", run.err)
		}
		if !m182Holds(run.modes, "AccessExclusiveLock") || !run.readerWaited || !run.writerWaited {
			t.Errorf("the drop holds %v on context_blocks (reader waited %v, writer waited %v), want ACCESS EXCLUSIVE", run.modes, run.readerWaited, run.writerWaited)
		}
		if len(run.warnings) != 0 {
			t.Errorf("warnings: %v", run.warnings)
		}
		for _, name := range []string{"idx_guard_pending", "idx_guard_pending_next", "idx_guard_pending_old"} {
			if ix := m182Index(t, pool, name); ix.found {
				t.Errorf("%s survived: %s", name, ix.def)
			}
		}
		if ix := m182Index(t, pool, "idx_guard_pending_type"); ix.oid != typ.oid {
			t.Errorf("idx_guard_pending_type changed: oid %d -> %d", typ.oid, ix.oid)
		}
		// A second application finds nothing to drop and takes no lock.
		again := m18xApply(t, dsn, pool, body, "context_blocks", 0, m183Read, m183Write)
		if again.err != nil || len(again.modes) != 0 || again.readerWaited || again.writerWaited || len(again.warnings) != 0 {
			t.Errorf("second application: err %v, locks %v, reader waited %v, writer waited %v, warnings %v", again.err, again.modes, again.readerWaited, again.writerWaited, again.warnings)
		}
	})

	// idx_guard_pending_type does not stand as 182 builds it: the file keeps
	// idx_guard_pending (one WARNING naming it) and still drops the twins.
	// Then the runbook: build the pick index, drop the old one CONCURRENTLY;
	// the file finds nothing left.
	for _, c := range []struct {
		name  string
		setup []string
	}{
		{"type_index_missing", []string{`DROP INDEX idx_guard_pending_type`}},
		{"type_index_invalid", []string{`UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'idx_guard_pending_type'::regclass`}},
		{"type_index_not_ready", []string{`UPDATE pg_index SET indisready = false WHERE indexrelid = 'idx_guard_pending_type'::regclass`}},
		{"type_index_foreign", []string{`DROP INDEX idx_guard_pending_type`, `CREATE INDEX idx_guard_pending_type ON context_blocks (type_name, created_at)`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 182)
			m182Seed(t, pool)
			m183Twins(t, pool)
			old := m182Index(t, pool, "idx_guard_pending")
			for _, s := range c.setup {
				m182Exec(t, pool, s)
			}
			run := m18xApply(t, dsn, pool, body, "context_blocks", 0, m183Read, m183Write)
			t.Logf("%s: %s; locks %v; warnings %v", c.name, run.took, run.modes, run.warnings)
			if run.err != nil {
				t.Fatalf("apply: %v", run.err)
			}
			if len(run.warnings) != 1 || m18xWarns(run, "idx_guard_pending is kept") != 1 {
				t.Errorf("warnings %v, want one keeping idx_guard_pending", run.warnings)
			}
			if ix := m182Index(t, pool, "idx_guard_pending"); ix.oid != old.oid || ix.def != m181IndexDef {
				t.Errorf("idx_guard_pending changed: %+v -> %+v", old, ix)
			}
			for _, name := range []string{"idx_guard_pending_next", "idx_guard_pending_old"} {
				if ix := m182Index(t, pool, name); ix.found {
					t.Errorf("%s survived: %s", name, ix.def)
				}
			}
			// The runbook, in its order.
			for _, s := range []string{`DROP INDEX CONCURRENTLY IF EXISTS idx_guard_pending_type`, m182DDL, `DROP INDEX CONCURRENTLY idx_guard_pending`} {
				m182Exec(t, pool, s)
			}
			again := m18xApply(t, dsn, pool, body, "context_blocks", 0, m183Read, m183Write)
			if again.err != nil || len(again.modes) != 0 || len(again.warnings) != 0 {
				t.Errorf("after the runbook: err %v, locks %v, warnings %v", again.err, again.modes, again.warnings)
			}
			if ix := m182Index(t, pool, "idx_guard_pending_type"); ix.def != m182IndexDef || !ix.valid {
				t.Errorf("after the runbook idx_guard_pending_type is %s (valid=%v)", ix.def, ix.valid)
			}
		})
	}

	// A same-named index ctx did not build (another table, another
	// definition) is left as it is, with one WARNING each; the guard's own
	// index goes.
	t.Run("foreign_names", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 182)
		m182Seed(t, pool)
		m182Exec(t, pool, `CREATE TABLE m183_other (x int)`)
		m182Exec(t, pool, `CREATE INDEX idx_guard_pending_next ON m183_other (x)`)
		m182Exec(t, pool, `CREATE INDEX idx_guard_pending_old ON context_blocks (title)`)
		next, oldName := m182Index(t, pool, "idx_guard_pending_next"), m182Index(t, pool, "idx_guard_pending_old")
		run := m18xApply(t, dsn, pool, body, "context_blocks", 0, m183Read, m183Write)
		if run.err != nil {
			t.Fatalf("apply: %v", run.err)
		}
		if len(run.warnings) != 2 || m18xWarns(run, "is not the guard pending index") != 2 {
			t.Errorf("warnings %v, want two naming the foreign indexes", run.warnings)
		}
		if ix := m182Index(t, pool, "idx_guard_pending_next"); ix.oid != next.oid {
			t.Errorf("the foreign idx_guard_pending_next was dropped")
		}
		if ix := m182Index(t, pool, "idx_guard_pending_old"); ix.oid != oldName.oid {
			t.Errorf("the foreign idx_guard_pending_old was dropped")
		}
		if ix := m182Index(t, pool, "idx_guard_pending"); ix.found {
			t.Errorf("idx_guard_pending survived: %s", ix.def)
		}
	})

	// A relation that is not an index under one of the names is no index of
	// ctx: the file neither drops it nor fails on it (DROP INDEX on a view is
	// 42809), and the guard's own index goes.
	t.Run("view_name", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 182)
		m182Seed(t, pool)
		m182Exec(t, pool, `CREATE VIEW idx_guard_pending_next AS SELECT 1 AS x`)
		run := m18xApply(t, dsn, pool, body, "context_blocks", 0, m183Read, m183Write)
		if run.err != nil {
			t.Fatalf("apply: %v", run.err)
		}
		if len(run.warnings) != 0 {
			t.Errorf("warnings %v, want none (a view is no index ctx built)", run.warnings)
		}
		if k := m18xRelkind(t, pool, "idx_guard_pending_next"); k != "v" {
			t.Errorf("the view idx_guard_pending_next is gone: relkind %q", k)
		}
		if ix := m182Index(t, pool, "idx_guard_pending"); ix.found {
			t.Errorf("idx_guard_pending survived: %s", ix.def)
		}
	})

	// lock_timeout: a transaction that has written context_blocks and stays
	// open holds ROW EXCLUSIVE; the DROP waits 3 s for ACCESS EXCLUSIVE and
	// fails with 55P03, nothing dropped.
	t.Run("lock_timeout", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 182)
		m182Seed(t, pool)
		hold, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = hold.Rollback(ctx) }()
		if _, err := hold.Exec(ctx, m183Write); err != nil {
			t.Fatal(err)
		}
		run := m18xApply(t, dsn, pool, body, "context_blocks", 0, "", "")
		var pgErr *pgconn.PgError
		if !errors.As(run.err, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("the file beside an open writer: %v, want 55P03", run.err)
		}
		if run.took < 2500*time.Millisecond || run.took > 10*time.Second {
			t.Errorf("the file failed after %s, want about the 3 s of lock_timeout", run.took)
		}
		_ = hold.Rollback(ctx)
		if ix := m182Index(t, pool, "idx_guard_pending"); !ix.found {
			t.Errorf("idx_guard_pending is gone after the failed file")
		}
	})
}
