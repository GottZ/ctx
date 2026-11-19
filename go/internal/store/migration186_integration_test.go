//go:build integration

package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const (
	migration186File = "186_blocks_created_at_not_null.sql"
	m186Read         = `SELECT count(*) FROM context_blocks WHERE category = 'm186'`
	m186Write        = `UPDATE context_blocks SET title = title WHERE category = 'm186' AND title = 'm186 v7'`
)

// m186Con is the not-null constraint of a column of context_blocks.
type m186Con struct {
	name      string
	validated bool
	notnull   bool // pg_attribute.attnotnull
}

func m186Constraint(t *testing.T, pool *pgxpool.Pool, col string) m186Con {
	t.Helper()
	var c m186Con
	err := pool.QueryRow(context.Background(), `
		SELECT COALESCE(c.conname, ''), COALESCE(c.convalidated, false), a.attnotnull
		  FROM pg_attribute a
		  LEFT JOIN pg_constraint c ON c.conrelid = a.attrelid AND c.contype = 'n' AND c.conkey = ARRAY[a.attnum]
		 WHERE a.attrelid = 'context_blocks'::regclass AND a.attname = $1`, col).Scan(&c.name, &c.validated, &c.notnull)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// m186Seed writes blocks without created_at or updated_at, as only a manual
// write can: a uuidv7 id without created_at, a version-4 id without
// created_at but with updated_at, a version-4 id without either, a uuidv7 id
// without updated_at, and an untouched block.
func m186Seed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	m182Exec(t, pool, `INSERT INTO context_blocks (id, category, title, content, scope, created_at, updated_at) VALUES
		(uuidv7(),           'm186', 'm186 v7',            'x', 'm186', NULL, now() - interval '3 days'),
		(gen_random_uuid(),  'm186', 'm186 v4 updated',    'x', 'm186', NULL, '2026-01-02 03:04:05+00'),
		(gen_random_uuid(),  'm186', 'm186 v4 none',       'x', 'm186', NULL, NULL),
		(uuidv7(),           'm186', 'm186 no updated_at', 'x', 'm186', '2026-02-03 04:05:06+00', NULL),
		(uuidv7(),           'm186', 'm186 whole',         'x', 'm186', '2026-03-04 05:06:07+00', '2026-03-05 05:06:07+00')`)
}

// m186IndexFiles is every index of context_blocks with its file node: a
// constraint that rewrote the table or rebuilt an index would change one.
func m186IndexFiles(t *testing.T, pool *pgxpool.Pool) map[string]uint32 {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT c.relname, c.relfilenode FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		WHERE i.indrelid = 'context_blocks'::regclass`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]uint32{}
	for rows.Next() {
		var n string
		var f uint32
		if err := rows.Scan(&n, &f); err != nil {
			t.Fatal(err)
		}
		out[n] = f
	}
	return out
}

func m186Is23502(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23502"
}

// TestMigration186 applies migration 186 (issue #237) to a database at 185.
// Below the threshold both columns become NOT NULL in one inline validation
// under ACCESS EXCLUSIVE (a reader and a writer wait): the rows without
// created_at get the uuidv7 timestamp of their id, else updated_at, else the
// time of the migration; the rows without updated_at get created_at; NOTICEs
// name them. Afterwards an insert or update leaving either column NULL is
// refused with 23502 — which the schema before it accepted. No table is
// rewritten and no index rebuilt (idx_guard_pending_type of 182 keys
// timezone('UTC', created_at)). Above the threshold created_at gets its
// constraint NOT VALID after the repair and updated_at none (a NOT VALID one
// would refuse the guard's update of a row still without updated_at); the
// runbook validates both. A second application is a no-op without a lock;
// lock_timeout bounds the wait for the lock.
func TestMigration186(t *testing.T) {
	ctx := context.Background()
	body, err := migrations.FS.ReadFile(migration186File)
	if err != nil {
		t.Fatalf("read %s: %v", migration186File, err)
	}

	t.Run("base_accepts_null", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 185)
		_, err := pool.Exec(ctx, `INSERT INTO context_blocks (category, title, content, scope, created_at) VALUES ('m186', 'm186 null', 'x', 'm186', NULL)`)
		t.Logf("at 185 an insert without created_at: %v", err)
		if err != nil {
			t.Fatalf("fixture: the schema before 186 refused the row: %v", err)
		}
	})

	t.Run("inline", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 185)
		m186Seed(t, pool)
		files := m186IndexFiles(t, pool)
		typ := m182Index(t, pool, "idx_guard_pending_type")
		run := m18xApply(t, dsn, pool, body, "context_blocks", 0, m186Read, m186Write)
		t.Logf("migration 186 inline: %s; locks %v; reader waited %v, writer waited %v; notices %v; warnings %v",
			run.took, run.modes, run.readerWaited, run.writerWaited, run.notices, run.warnings)
		if run.err != nil {
			t.Fatal(run.err)
		}
		if !m182Holds(run.modes, "AccessExclusiveLock") || !run.readerWaited || !run.writerWaited {
			t.Errorf("locks %v (reader waited %v, writer waited %v), want ACCESS EXCLUSIVE to the commit", run.modes, run.readerWaited, run.writerWaited)
		}
		if len(run.warnings) != 0 {
			t.Errorf("warnings below the threshold: %v", run.warnings)
		}
		var noticeCreated, noticeUpdated bool
		for _, n := range run.notices {
			noticeCreated = noticeCreated || strings.Contains(n, "3 row(s) had no created_at")
			noticeUpdated = noticeUpdated || strings.Contains(n, "2 row(s) had no updated_at")
		}
		if !noticeCreated || !noticeUpdated {
			t.Errorf("notices %v, want 3 rows without created_at and 2 without updated_at", run.notices)
		}
		for _, col := range []string{"created_at", "updated_at"} {
			if c := m186Constraint(t, pool, col); c.name != "context_blocks_"+col+"_not_null" || !c.validated || !c.notnull {
				t.Errorf("%s: %+v, want context_blocks_%s_not_null validated", col, c, col)
			}
		}
		// The repaired values.
		var v7ok, v4updOK, v4noneOK, updOK, wholeOK bool
		if err := pool.QueryRow(ctx, `SELECT
			bool_and(created_at = uuid_extract_timestamp(id)) FILTER (WHERE title = 'm186 v7'),
			bool_and(created_at = '2026-01-02 03:04:05+00') FILTER (WHERE title = 'm186 v4 updated'),
			bool_and(created_at > now() - interval '1 hour' AND updated_at = created_at) FILTER (WHERE title = 'm186 v4 none'),
			bool_and(updated_at = '2026-02-03 04:05:06+00') FILTER (WHERE title = 'm186 no updated_at'),
			bool_and(created_at = '2026-03-04 05:06:07+00' AND updated_at = '2026-03-05 05:06:07+00') FILTER (WHERE title = 'm186 whole')
			FROM context_blocks WHERE category = 'm186'`).Scan(&v7ok, &v4updOK, &v4noneOK, &updOK, &wholeOK); err != nil {
			t.Fatal(err)
		}
		if !v7ok || !v4updOK || !v4noneOK || !updOK || !wholeOK {
			t.Errorf("repaired values: v7 %v, v4 with updated_at %v, v4 without either %v, without updated_at %v, untouched %v", v7ok, v4updOK, v4noneOK, updOK, wholeOK)
		}
		// No table rewritten, no index rebuilt.
		for n, f := range m186IndexFiles(t, pool) {
			if files[n] != f {
				t.Errorf("index %s rebuilt: file node %d -> %d", n, files[n], f)
			}
		}
		if ix := m182Index(t, pool, "idx_guard_pending_type"); ix.oid != typ.oid || ix.def != m182IndexDef {
			t.Errorf("idx_guard_pending_type changed: %+v -> %+v", typ, ix)
		}
		// From now on a row without either value is refused.
		if _, err := pool.Exec(ctx, `INSERT INTO context_blocks (category, title, content, scope, created_at) VALUES ('m186', 'm186 null', 'x', 'm186', NULL)`); !m186Is23502(err) {
			t.Errorf("an insert without created_at: %v, want 23502", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE context_blocks SET updated_at = NULL WHERE title = 'm186 whole'`); !m186Is23502(err) {
			t.Errorf("an update to updated_at NULL: %v, want 23502", err)
		}
		// A second application: nothing to do, no lock.
		again := m18xApply(t, dsn, pool, body, "context_blocks", 0, m186Read, m186Write)
		if again.err != nil || len(again.modes) != 0 || again.writerWaited || len(again.notices)+len(again.warnings) != 0 {
			t.Errorf("second application: err %v, locks %v, writer waited %v, notices %v, warnings %v", again.err, again.modes, again.writerWaited, again.notices, again.warnings)
		}
	})

	t.Run("stock", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 185)
		m186Seed(t, pool)
		if err := store.RunMigrationsUpTo(ctx, pool, 186); err != nil {
			t.Fatalf("run migrations: %v", err)
		}
		for _, col := range []string{"created_at", "updated_at"} {
			if c := m186Constraint(t, pool, col); !c.validated || !c.notnull {
				t.Errorf("%s after the runner: %+v", col, c)
			}
		}
	})

	t.Run("above_threshold", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 185)
		m186Seed(t, pool)
		run := m18xApply(t, dsn, pool, body, "context_blocks", 600000, m186Read, m186Write)
		t.Logf("above the threshold: %s; locks %v; notices %v; warnings %v", run.took, run.modes, run.notices, run.warnings)
		if run.err != nil {
			t.Fatal(run.err)
		}
		if len(run.warnings) != 1 || m18xWarns(run, "updated_at has none at 600000 rows") != 1 {
			t.Errorf("warnings %v, want one naming the runbook", run.warnings)
		}
		if c := m186Constraint(t, pool, "created_at"); c.name != "context_blocks_created_at_not_null" || c.validated {
			t.Errorf("created_at: %+v, want the NOT VALID constraint", c)
		}
		if c := m186Constraint(t, pool, "updated_at"); c.name != "" {
			t.Errorf("updated_at: %+v, want no constraint above the threshold", c)
		}
		var nullCreated, nullUpdated int
		if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE created_at IS NULL), count(*) FILTER (WHERE updated_at IS NULL) FROM context_blocks`).Scan(&nullCreated, &nullUpdated); err != nil {
			t.Fatal(err)
		}
		if nullCreated != 0 || nullUpdated != 2 {
			t.Errorf("rows without created_at %d (want 0, repaired), without updated_at %d (want 2, left to the runbook)", nullCreated, nullUpdated)
		}
		// A row still without updated_at takes an update that does not write
		// the column (the guard and dream batches write metadata only).
		if _, err := pool.Exec(ctx, `UPDATE context_blocks SET metadata = metadata || '{"m186": 1}' WHERE title = 'm186 no updated_at'`); err != nil {
			t.Errorf("an update of a row without updated_at above the threshold: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO context_blocks (category, title, content, scope, created_at) VALUES ('m186', 'm186 null', 'x', 'm186', NULL)`); !m186Is23502(err) {
			t.Errorf("an insert without created_at above the threshold: %v, want 23502 (NOT VALID checks new rows)", err)
		}
		// The runbook of the header, statement by statement.
		for _, s := range []string{
			`UPDATE context_blocks SET updated_at = COALESCE(created_at, uuid_extract_timestamp(id), now()) WHERE updated_at IS NULL`,
			`ALTER TABLE context_blocks ADD CONSTRAINT context_blocks_updated_at_not_null NOT NULL updated_at NOT VALID`,
			`ALTER TABLE context_blocks VALIDATE CONSTRAINT context_blocks_created_at_not_null, VALIDATE CONSTRAINT context_blocks_updated_at_not_null`,
		} {
			m182Exec(t, pool, s)
		}
		for _, col := range []string{"created_at", "updated_at"} {
			if c := m186Constraint(t, pool, col); !c.validated {
				t.Errorf("%s after the runbook: %+v", col, c)
			}
		}
		again := m18xApply(t, dsn, pool, body, "context_blocks", 600000, m186Read, m186Write)
		if again.err != nil || len(again.modes) != 0 || len(again.warnings) != 0 {
			t.Errorf("after the runbook: err %v, locks %v, warnings %v", again.err, again.modes, again.warnings)
		}
	})

	// The validation can run before the upgrade: the file then changes
	// nothing.
	t.Run("validated_beforehand", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 185)
		m182Exec(t, pool, `ALTER TABLE context_blocks ALTER COLUMN created_at SET NOT NULL, ALTER COLUMN updated_at SET NOT NULL`)
		run := m18xApply(t, dsn, pool, body, "context_blocks", 0, m186Read, m186Write)
		if run.err != nil || len(run.modes) != 0 || len(run.warnings)+len(run.notices) != 0 {
			t.Errorf("over SET NOT NULL: err %v, locks %v, warnings %v, notices %v", run.err, run.modes, run.warnings, run.notices)
		}
	})

	t.Run("lock_timeout", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 185)
		m186Seed(t, pool)
		hold, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = hold.Rollback(ctx) }()
		if _, err := hold.Exec(ctx, `SELECT count(*) FROM context_blocks`); err != nil {
			t.Fatal(err)
		}
		run := m18xApply(t, dsn, pool, body, "context_blocks", 0, "", "")
		var pgErr *pgconn.PgError
		if !errors.As(run.err, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("the file beside an open reader: %v, want 55P03", run.err)
		}
		if run.took < 2500*time.Millisecond {
			t.Errorf("failed after %s, want the 3 s of lock_timeout", run.took)
		}
		_ = hold.Rollback(ctx)
		if c := m186Constraint(t, pool, "created_at"); c.name != "" {
			t.Errorf("the failed file left %+v", c)
		}
	})
}

// TestMigration186RepairReadsTheIndex (the cost pin of 186's repair): the
// rows without created_at are found through idx_context_created, beside a
// store of 200 000 rows, in every statistics state (the statement has no
// parameter, so there is one plan mode): fresh, stale (taken before the
// store grew) and never taken. Above the threshold this is all the file
// reads under its ACCESS EXCLUSIVE lock.
func TestMigration186RepairReadsTheIndex(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, 185)
	for _, s := range []string{
		`ALTER TABLE context_blocks SET (autovacuum_enabled = false)`,
		`ALTER TABLE context_blocks DISABLE TRIGGER USER`,
		`INSERT INTO context_blocks (category, title, content, scope) SELECT 'm186', 'm186 r' || g, 'x', 'm186' FROM generate_series(1, 2000) g`,
		`ANALYZE context_blocks`,
		`INSERT INTO context_blocks (category, title, content, scope) SELECT 'm186', 'm186 b' || g, 'bulk', 'm186' FROM generate_series(1, 200000) g`,
		`UPDATE context_blocks SET created_at = NULL WHERE title IN ('m186 r1', 'm186 b77')`,
		`VACUUM context_blocks`,
		`ALTER TABLE context_blocks ENABLE TRIGGER USER`,
	} {
		m182Exec(t, pool, s)
	}
	const repair = `UPDATE context_blocks SET created_at = COALESCE(uuid_extract_timestamp(id), updated_at, now()) WHERE created_at IS NULL`
	for _, st := range []struct {
		name string
		set  []string
	}{
		{"stale", nil},
		{"never taken", []string{`SELECT pg_clear_relation_stats('public', 'context_blocks')`, `SELECT pg_clear_attribute_stats('public', 'context_blocks', 'created_at', false)`}},
		{"fresh", []string{`ANALYZE context_blocks`}},
	} {
		for _, s := range st.set {
			m182Exec(t, pool, s)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var plan []byte
		err = tx.QueryRow(ctx, `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) `+repair).Scan(&plan)
		_ = tx.Rollback(ctx)
		if err != nil {
			t.Fatal(err)
		}
		p := string(plan)
		t.Logf("statistics %s: %s", st.name, p)
		if !strings.Contains(p, `"Index Name": "idx_context_created"`) || strings.Contains(p, `"Seq Scan"`) {
			t.Errorf("statistics %s: the repair does not read idx_context_created", st.name)
		}
		if !strings.Contains(p, `"Actual Rows": 2`) {
			t.Errorf("statistics %s: the repair did not find the two rows", st.name)
		}
	}
}
