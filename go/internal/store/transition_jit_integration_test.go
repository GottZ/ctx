//go:build integration

package store_test

// Issue #340: a query of a trigger function over a transition table has no
// parameters, so PL/pgSQL plans it once per backend and keeps that plan,
// sized by the transition table of the first call — JIT decision included.
// A first statement large enough for JIT left a plan that compiled again for
// every later statement of that backend. Since migration 180 every trigger
// function that reads a transition table runs with jit = off (177 did it
// for mark_guard_dirty). TestTransitionTableFunctionsRunWithoutJit reads the
// rule from the catalog, so a later trigger function of the class without
// the setting turns it red; TestTransitionTablePlansDoNotKeepJit runs the
// case on a server with JIT (the production image).

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// transitionFunctionsSQL lists every function a trigger with a transition
// table calls, with the transition table names of its triggers (not of an
// extension, not BEGIN ATOMIC — as expressionFunctionsSQL).
const transitionFunctionsSQL = `
SELECT p.oid::regprocedure::text, p.prosrc, coalesce(p.proconfig, '{}'),
       array_agg(DISTINCT lower(x.name::text) ORDER BY lower(x.name::text)),
       string_agg(DISTINCT t.tgname || ' on ' || t.tgrelid::regclass::text, '; ')
  FROM pg_trigger t
  JOIN pg_proc p ON p.oid = t.tgfoid
  CROSS JOIN LATERAL (VALUES (t.tgoldtable), (t.tgnewtable)) AS x(name)
 WHERE NOT t.tgisinternal
   AND x.name IS NOT NULL
   AND p.prosqlbody IS NULL
   AND NOT EXISTS (SELECT 1 FROM pg_depend e
                    WHERE e.classid = 'pg_proc'::regclass AND e.objid = p.oid AND e.deptype = 'e')
 GROUP BY 1, 2, 3
 ORDER BY 1`

// transitionFunctionsWithJit applies the rule: a function whose body names a
// transition table of its triggers (unqualifiedNames, the reader of the
// search_path check: comments dropped, string literals kept, so a name in
// the text of an EXECUTE counts) carries jit=off. It returns the functions
// that read a transition table and those of them without the setting.
func transitionFunctionsWithJit(t *testing.T, pool *pgxpool.Pool) (readers, bad []string) {
	t.Helper()
	rows, err := pool.Query(context.Background(), transitionFunctionsSQL)
	if err != nil {
		t.Fatalf("list transition table functions: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sig, body, uses string
		var config, tables []string
		if err := rows.Scan(&sig, &body, &config, &tables, &uses); err != nil {
			t.Fatalf("scan: %v", err)
		}
		named := unqualifiedNames(body, tables)
		if len(named) == 0 {
			continue
		}
		readers = append(readers, sig)
		if !slices.Contains(config, "jit=off") {
			bad = append(bad, fmt.Sprintf("%s (%s): reads %s, settings %v", sig, uses, strings.Join(named, ", "), config))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list transition table functions: %v", err)
	}
	return readers, bad
}

func TestTransitionTableFunctionsRunWithoutJit(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	readers, bad := transitionFunctionsWithJit(t, pool)
	// mark_guard_dirty() (177, jit = off since then) left the class with
	// migration 181 (issue #341); TestMigration181_DropsTheCounterKeepsTheTable
	// pins its settings on the way back.
	want := "context_block_counts_del() context_block_counts_ins() context_block_counts_upd() notify_project_delete()"
	if got := strings.Join(readers, " "); got != want {
		t.Fatalf("functions that read a transition table: %s, want %s — a new function of the class belongs in this list (with jit = off); one missing means the catalog query no longer sees it", got, want)
	}
	if len(bad) > 0 {
		t.Fatalf("%d trigger functions read a transition table and keep the JIT decision of their first call on a backend (jit=off missing):\n  %s",
			len(bad), strings.Join(bad, "\n  "))
	}

	// The settings of each, whole: jit=off is added, the search_path of 179
	// and the settings of 177 stay.
	pinned := "search_path=pg_catalog, public, pg_temp"
	for sig, want := range map[string][]string{
		"context_block_counts_ins()": {"jit=off", pinned},
		"context_block_counts_upd()": {"jit=off", pinned},
		"context_block_counts_del()": {"jit=off", pinned},
		"notify_project_delete()":    {"jit=off"},
	} {
		var cfg []string
		if err := pool.QueryRow(ctx, `SELECT coalesce(proconfig, '{}') FROM pg_proc WHERE oid = $1::regprocedure`, sig).Scan(&cfg); err != nil {
			t.Fatalf("read proconfig of %s: %v", sig, err)
		}
		sort.Strings(cfg)
		if strings.Join(cfg, "|") != strings.Join(want, "|") {
			t.Errorf("%s proconfig = %v, want %v", sig, cfg, want)
		}
	}

	// The rule names what it should and nothing else: a function that counts
	// its transition table is named (also through EXECUTE, also under a
	// quoted mixed-case name), one whose trigger has a transition
	// table it does not read (a notify, a name in a comment) is not, and the
	// counting one passes once it carries jit = off.
	for _, s := range []string{
		`CREATE TABLE zz340_rows (v int)`,
		`CREATE FUNCTION zz340_counts() RETURNS trigger LANGUAGE plpgsql AS $$
         BEGIN PERFORM count(*) FROM "zz_new"; RETURN NULL; END $$`,
		`CREATE TRIGGER zz340_trg AFTER INSERT ON zz340_rows REFERENCING NEW TABLE AS zz_new FOR EACH STATEMENT EXECUTE FUNCTION zz340_counts()`,
		`CREATE FUNCTION zz340_dynamic() RETURNS trigger LANGUAGE plpgsql AS $$
         BEGIN EXECUTE 'SELECT count(*) FROM zz_old'; RETURN NULL; END $$`,
		`CREATE TRIGGER zz340_trg_dynamic AFTER DELETE ON zz340_rows REFERENCING OLD TABLE AS zz_old FOR EACH STATEMENT EXECUTE FUNCTION zz340_dynamic()`,
		`CREATE FUNCTION zz340_upper() RETURNS trigger LANGUAGE plpgsql AS $$
         BEGIN PERFORM count(*) FROM "NewRows"; RETURN NULL; END $$`,
		`CREATE TRIGGER zz340_trg_upper AFTER INSERT ON zz340_rows REFERENCING NEW TABLE AS "NewRows" FOR EACH STATEMENT EXECUTE FUNCTION zz340_upper()`,
		`CREATE FUNCTION zz340_notify() RETURNS trigger LANGUAGE plpgsql AS $$
         BEGIN -- zz_upd is not read here
         PERFORM pg_notify('zz340', TG_TABLE_NAME); RETURN NULL; END $$`,
		`CREATE TRIGGER zz340_trg_notify AFTER UPDATE ON zz340_rows REFERENCING NEW TABLE AS zz_upd FOR EACH STATEMENT EXECUTE FUNCTION zz340_notify()`,
	} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("plant: %v", err)
		}
	}
	_, bad = transitionFunctionsWithJit(t, pool)
	var named []string
	for _, b := range bad {
		named = append(named, b[:strings.Index(b, " ")])
	}
	if got := strings.Join(named, " "); got != "zz340_counts() zz340_dynamic() zz340_upper()" {
		t.Fatalf("the check names %q, want zz340_counts() zz340_dynamic() zz340_upper():\n  %s", got, strings.Join(bad, "\n  "))
	}
	for _, s := range []string{`ALTER FUNCTION zz340_counts() SET jit = off`, `ALTER FUNCTION zz340_dynamic() SET jit = off`, `ALTER FUNCTION zz340_upper() SET jit = off`} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if _, bad = transitionFunctionsWithJit(t, pool); len(bad) != 0 {
		t.Fatalf("the check still names functions with jit = off: %v", bad)
	}
}

// transitionQueries marks the plans of the transition table queries in an
// auto_explain notice: the block counters of 170, the guard counter of 177,
// the project delete notification.
var transitionQueries = []string{
	"INSERT INTO context_block_count_deltas",
	"IS DISTINCT FROM 'index'",
	"SELECT DISTINCT scope, type_name",
}

// jitSession is one backend with JIT at a scaled threshold whose nested
// plans arrive as auto_explain notices.
type jitSession struct {
	t       *testing.T
	c       *pgx.Conn
	mu      sync.Mutex
	notices []string
}

// jitWrites are the kinds of write: the first call of a kind's triggers on a
// backend is a statement of 30 000 rows (large), the next one writes one row.
var jitWrites = []struct{ large, one string }{
	{`UPDATE context_blocks SET tags = ARRAY['t'] WHERE category = 'j340'`,
		`UPDATE context_blocks SET content = content || '.' WHERE category = 'j340one'`},
	{`INSERT INTO context_blocks (category, title, content, scope, type_name)
	  SELECT 'j340ins', 'j340 ins ' || g, 'x', 'private', CASE WHEN g % 10 = 0 THEN 'issue' ELSE 'knowledge' END
	    FROM generate_series(1, 30000) g`,
		`INSERT INTO context_blocks (category, title, content, scope, type_name) VALUES ('learnings', 'j340 one', 'x', 'private', 'issue')`},
	{`DELETE FROM context_blocks WHERE category = 'j340'`,
		`DELETE FROM context_blocks WHERE category = 'j340one'`},
}

// newJitSession seeds the blocks the writes name, or skips where the server
// has no JIT. jit_above_cost is scaled down to 500 for the whole session
// (production: 100 000, reached from roughly 1.25 million updated rows,
// docs/operations.md "Migration 180"): a 30 000-row statement passes it in
// the trigger queries, a one-row write passes it nowhere.
func newJitSession(t *testing.T, pool *pgxpool.Pool, dsn string) *jitSession {
	t.Helper()
	ctx := context.Background()
	var avail bool
	if err := pool.QueryRow(ctx, `SELECT pg_jit_available()`).Scan(&avail); err != nil {
		t.Fatal(err)
	}
	if !avail {
		t.Skip("the server has no JIT (pg_jit_available() is false); the production image has one: CTX_TEST_PG_IMAGE=pgvector-timescaledb:pg18")
	}
	// A VACUUM or ANALYZE of a table a cached plan depends on plans it again
	// (context_blocks, whose row type the transition tables carry, and the
	// tables the queries write); the rolled-back writes leave dead rows that
	// autovacuum would pick up at any moment. Kept off for the three, so the
	// case does not depend on its timing.
	for _, s := range []string{
		`INSERT INTO context_blocks (category, title, content, scope, type_name)
		 SELECT CASE WHEN g = 0 THEN 'j340one' ELSE 'j340' END, 'j340 ' || g, 'x', 'private',
		        CASE WHEN g % 10 = 0 THEN 'issue' ELSE 'knowledge' END
		   FROM generate_series(0, 30000) g`,
		`ANALYZE context_blocks`,
		`ALTER TABLE context_blocks SET (autovacuum_enabled = false)`,
		`ALTER TABLE context_block_count_deltas SET (autovacuum_enabled = false)`,
		`ALTER TABLE context_guard_state SET (autovacuum_enabled = false)`,
	} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	js := &jitSession{t: t}
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		js.mu.Lock()
		js.notices = append(js.notices, n.Message)
		js.mu.Unlock()
	}
	js.c, err = pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = js.c.Close(ctx) })
	js.exec(`SET jit = on`, `SET jit_above_cost = 500`,
		`LOAD 'auto_explain'`,
		`SET auto_explain.log_min_duration = 0`,
		`SET auto_explain.log_analyze = on`,
		`SET auto_explain.log_nested_statements = on`,
		`SET client_min_messages = log`)
	return js
}

func (js *jitSession) exec(sqls ...string) {
	js.t.Helper()
	for _, s := range sqls {
		if _, err := js.c.Exec(context.Background(), s); err != nil {
			js.t.Fatalf("%s: %v", s, err)
		}
	}
}

// write runs sql in a transaction it rolls back, so every run sees the seed,
// and returns the plans it logged.
func (js *jitSession) write(sql string) []string {
	js.t.Helper()
	js.exec(`BEGIN`, sql, `ROLLBACK`)
	js.mu.Lock()
	defer js.mu.Unlock()
	out := js.notices
	js.notices = nil
	return out
}

// writeLarge runs a large statement and requires that the threshold is live:
// the statement itself is compiled.
func (js *jitSession) writeLarge(sql string) {
	js.t.Helper()
	if !slices.ContainsFunc(js.write(sql), func(n string) bool { return strings.Contains(n, "JIT:") }) {
		js.t.Fatalf("no plan of %q was compiled at jit_above_cost 500; the case shows nothing", sql)
	}
}

// compiledTransitionPlans counts the plans of the transition table queries
// among notices, and those of every plan that was compiled.
func compiledTransitionPlans(notices []string) (seen map[string]int, compiled []string) {
	seen = map[string]int{}
	for _, n := range notices {
		for _, q := range transitionQueries {
			if strings.Contains(n, q) {
				seen[q]++
			}
		}
		if strings.Contains(n, "JIT:") {
			compiled = append(compiled, n)
		}
	}
	return seen, compiled
}

// TestTransitionTablePlansDoNotKeepJit: a backend whose first statement of a
// kind was large enough for JIT runs one-row writes of that kind, under the
// same settings, without compiling anything. Needs a server with JIT, as the
// production image has one (CTX_TEST_PG_IMAGE=pgvector-timescaledb:pg18);
// skipped where pg_jit_available() is false.
func TestTransitionTablePlansDoNotKeepJit(t *testing.T) {
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	js := newJitSession(t, pool, dsn)
	var all []string
	for _, w := range jitWrites {
		js.writeLarge(w.large)
		all = append(all, js.write(w.one)...)
	}
	seen, compiled := compiledTransitionPlans(all)
	for _, n := range compiled {
		t.Errorf("a one-row write on a backend whose first statement of its kind was compiled compiles again:\n%s", n)
	}
	// update: counters + guard; insert: counters + guard; delete: counters + project notification.
	if seen[transitionQueries[0]] != 3 || seen[transitionQueries[1]] != 2 || seen[transitionQueries[2]] != 1 {
		t.Fatalf("plans of the transition table queries in the one-row writes: %v, want 3 / 2 / 1", seen)
	}
}

// TestMigration180ReplansABackendThatKeptJit: the rolling case. A pool
// connection that kept compiled plans on a database at 179 compiles them on
// every one-row write; once migration 180 has run on another connection, the
// same connection runs the functions with the new setting — without a
// reconnect: a large statement followed by a one-row write of its kind
// compiles nothing in the one-row write.
func TestMigration180ReplansABackendThatKeptJit(t *testing.T) {
	ctx := context.Background()
	pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 179)
	js := newJitSession(t, pool, dsn)
	var before []string
	for _, w := range jitWrites {
		js.writeLarge(w.large)
		before = append(before, js.write(w.one)...)
	}
	if _, compiled := compiledTransitionPlans(before); len(compiled) < 4 {
		t.Fatalf("at 179 the one-row writes compiled %d plans, want the four transition table queries of 170 and 113: the case shows nothing", len(compiled))
	}
	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	var after []string
	for _, w := range jitWrites {
		js.writeLarge(w.large)
		after = append(after, js.write(w.one)...)
	}
	seen, compiled := compiledTransitionPlans(after)
	for _, n := range compiled {
		t.Errorf("after migration 180 a one-row write on the same backend compiles again:\n%s", n)
	}
	if seen[transitionQueries[0]] != 3 || seen[transitionQueries[2]] != 1 {
		t.Fatalf("plans of the transition table queries after the migration: %v", seen)
	}
}
