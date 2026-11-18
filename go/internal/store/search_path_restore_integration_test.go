//go:build integration

package store_test

// Issue #351: pg_restore runs with search_path = '', and a trigger, CHECK,
// default, generated column or index expression is evaluated under the
// search_path of the session that writes the row. A function that names an
// object of its schema without a schema prefix then fails (42883 for a
// function, 42P01 for a table) unless it pins its own search_path
// (migration 179). TestTriggerFunctionsResolveUnderAnEmptySearchPath reads
// the rule from the catalog for every such function, so a later migration
// that adds an unpinned one turns it red; TestDataOnlyRestoreWithTriggersOn
// restores a data-only dump with the triggers on and checks what they keep.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// expressionFunction is one function that PostgreSQL calls on a row write
// in the writer's session: a trigger function, or a function in a CHECK
// constraint, a default, a generated column, an index expression or a
// trigger's WHEN clause.
type expressionFunction struct {
	sig    string
	schema string
	body   string
	config []string
	uses   string
}

// expressionFunctionsSQL lists them, without functions of an extension
// (their own search_path is the extension's business) and without
// BEGIN ATOMIC bodies (bound by OID when they were created).
const expressionFunctionsSQL = `
WITH uses AS (
    SELECT t.tgfoid AS oid, 'trigger ' || t.tgname || ' on ' || t.tgrelid::regclass::text AS use
      FROM pg_trigger t
     WHERE NOT t.tgisinternal
    UNION ALL
    SELECT d.refobjid, d.classid::regclass::text
      FROM pg_depend d
     WHERE d.refclassid = 'pg_proc'::regclass
       AND d.classid IN ('pg_attrdef'::regclass, 'pg_constraint'::regclass, 'pg_class'::regclass, 'pg_trigger'::regclass)
)
SELECT p.oid::regprocedure::text, n.nspname, p.prosrc, coalesce(p.proconfig, '{}'),
       string_agg(DISTINCT u.use, '; ' ORDER BY u.use)
  FROM uses u
  JOIN pg_proc p ON p.oid = u.oid
  JOIN pg_namespace n ON n.oid = p.pronamespace
 WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
   AND p.prosqlbody IS NULL
   AND NOT EXISTS (SELECT 1 FROM pg_depend e
                    WHERE e.classid = 'pg_proc'::regclass AND e.objid = p.oid AND e.deptype = 'e')
 GROUP BY 1, 2, 3, 4
 ORDER BY 1`

// schemaNamesSQL lists the names a function body could reach in a schema
// through the search_path: relations (and with them their row types),
// functions, and base, domain, enum, range and multirange types (vector and
// halfvec of pgvector live there). A name pg_catalog has as well (unnest of
// an extension) resolves there under an empty search_path, so it is left out.
const schemaNamesSQL = `
SELECT lower(name) FROM (
    SELECT c.relname AS name FROM pg_class c
     WHERE c.relnamespace = $1::regnamespace AND c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f')
    UNION
    SELECT p.proname FROM pg_proc p WHERE p.pronamespace = $1::regnamespace
    UNION
    SELECT t.typname FROM pg_type t
     WHERE t.typnamespace = $1::regnamespace AND t.typtype IN ('b', 'd', 'e', 'r', 'm') AND t.typcategory <> 'A'
    EXCEPT
    SELECT p.proname FROM pg_proc p WHERE p.pronamespace = 'pg_catalog'::regnamespace
    EXCEPT
    SELECT t.typname FROM pg_type t WHERE t.typnamespace = 'pg_catalog'::regnamespace
    EXCEPT
    SELECT c.relname FROM pg_class c WHERE c.relnamespace = 'pg_catalog'::regnamespace
) s`

var sqlComments = regexp.MustCompile(`(?s)--[^\n]*|/\*.*?\*/`)

// unqualifiedNames returns the schema names the body uses without a schema
// prefix, bare or double-quoted. Comments are dropped; string literals stay,
// so a name in the text of an EXECUTE counts (and so does a table name in a
// RAISE text: a pin more than needed, never one less). Operators are not
// read: an extension operator in the schema (pg_trgm's %) is reached through
// the search_path as well, and no function of the class uses one today.
func unqualifiedNames(body string, names []string) []string {
	text := strings.ToLower(sqlComments.ReplaceAllString(body, " "))
	var hits []string
	for _, n := range names {
		q := regexp.QuoteMeta(n)
		re := regexp.MustCompile(`(^|[^\w.$"])` + q + `([^\w$"]|$)|(^|[^\w.$])"` + q + `"`)
		if re.MatchString(text) {
			hits = append(hits, n)
		}
	}
	return hits
}

// calledFunctions returns the names of the schema's functions the body calls,
// with or without a schema prefix.
func calledFunctions(body string, funcs []string) []string {
	text := strings.ToLower(sqlComments.ReplaceAllString(body, " "))
	var hits []string
	for _, n := range funcs {
		q := regexp.QuoteMeta(n)
		if regexp.MustCompile(`(^|[^\w$])"?` + q + `"?\s*\(`).MatchString(text) {
			hits = append(hits, n)
		}
	}
	return hits
}

// pinsSchema: the function's own search_path holds its schema and ends with
// pg_temp (a temporary table of the writer's session cannot stand in for one
// of the schema's tables).
func pinsSchema(config []string, schema string) bool {
	path := pinnedPath(config)
	holds := false
	for _, e := range path {
		holds = holds || e == schema
	}
	return holds && path[len(path)-1] == "pg_temp"
}

// schemaFunctionsSQL lists the functions of a schema a body can call by name
// (not of an extension, not BEGIN ATOMIC).
const schemaFunctionsSQL = `
SELECT p.oid::regprocedure::text, p.proname, p.prosrc, coalesce(p.proconfig, '{}')
  FROM pg_proc p
 WHERE p.pronamespace = $1::regnamespace
   AND p.prosqlbody IS NULL
   AND NOT EXISTS (SELECT 1 FROM pg_depend e
                    WHERE e.classid = 'pg_proc'::regclass AND e.objid = p.oid AND e.deptype = 'e')`

// pinnedPath returns the elements of the function's own search_path, or nil
// when it carries none.
func pinnedPath(config []string) []string {
	for _, c := range config {
		v, ok := strings.CutPrefix(c, "search_path=")
		if !ok {
			continue
		}
		var path []string
		for _, e := range strings.Split(v, ",") {
			path = append(path, strings.Trim(strings.TrimSpace(e), `"`))
		}
		return path
	}
	return nil
}

// unresolvedFunctions applies the rule to every expression function: one
// that names an object of its schema without a prefix pins a search_path
// (pinsSchema). A function it calls runs under that pin; a function called
// by one without a pin (by a qualified name, say) runs under the writer's
// path and is checked the same way.
func unresolvedFunctions(t *testing.T, pool *pgxpool.Pool) (checked int, bad []string) {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, expressionFunctionsSQL)
	if err != nil {
		t.Fatalf("list expression functions: %v", err)
	}
	var queue []expressionFunction
	for rows.Next() {
		var f expressionFunction
		if err := rows.Scan(&f.sig, &f.schema, &f.body, &f.config, &f.uses); err != nil {
			t.Fatalf("scan expression function: %v", err)
		}
		queue = append(queue, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("list expression functions: %v", err)
	}
	checked = len(queue)
	names := map[string][]string{}
	funcs := map[string]map[string][]expressionFunction{}
	load := func(schema string) {
		if _, ok := names[schema]; ok {
			return
		}
		r, err := pool.Query(ctx, schemaNamesSQL, schema)
		if err != nil {
			t.Fatalf("list names of schema %s: %v", schema, err)
		}
		for r.Next() {
			var n string
			if err := r.Scan(&n); err != nil {
				t.Fatalf("scan name: %v", err)
			}
			names[schema] = append(names[schema], n)
		}
		r.Close()
		funcs[schema] = map[string][]expressionFunction{}
		r, err = pool.Query(ctx, schemaFunctionsSQL, schema)
		if err != nil {
			t.Fatalf("list functions of schema %s: %v", schema, err)
		}
		for r.Next() {
			f := expressionFunction{schema: schema}
			var name string
			if err := r.Scan(&f.sig, &name, &f.body, &f.config); err != nil {
				t.Fatalf("scan function: %v", err)
			}
			funcs[schema][strings.ToLower(name)] = append(funcs[schema][strings.ToLower(name)], f)
		}
		r.Close()
	}
	seen := map[string]bool{}
	for len(queue) > 0 {
		f := queue[0]
		queue = queue[1:]
		if seen[f.sig] {
			continue
		}
		seen[f.sig] = true
		load(f.schema)
		if pinsSchema(f.config, f.schema) {
			continue
		}
		if hits := unqualifiedNames(f.body, names[f.schema]); len(hits) > 0 {
			sort.Strings(hits)
			bad = append(bad, fmt.Sprintf("%s (%s): names %s without a schema, search_path %v", f.sig, f.uses, strings.Join(hits, ", "), pinnedPath(f.config)))
		}
		fnames := make([]string, 0, len(funcs[f.schema]))
		for n := range funcs[f.schema] {
			fnames = append(fnames, n)
		}
		for _, n := range calledFunctions(f.body, fnames) {
			for _, callee := range funcs[f.schema][n] {
				callee.uses = "called by " + f.sig
				queue = append(queue, callee)
			}
		}
	}
	sort.Strings(bad)
	return checked, bad
}

func TestTriggerFunctionsResolveUnderAnEmptySearchPath(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	checked, bad := unresolvedFunctions(t, pool)
	if checked < 39 {
		t.Fatalf("the catalog query found %d expression functions; the schema at 179 has 39 — the query no longer sees them", checked)
	}
	if len(bad) > 0 {
		t.Fatalf("%d of %d functions called on a row write do not resolve under an empty search_path (pg_restore, pg_dump's COPY):\n  %s",
			len(bad), checked, strings.Join(bad, "\n  "))
	}

	// The rule matches what PostgreSQL does: a trigger function that calls a
	// function of the schema without a prefix fails under an empty
	// search_path and the check names it; pinned as migration 179 pins them,
	// the same write succeeds and the check passes.
	for _, s := range []string{
		`CREATE TABLE zz351_rows (scope TEXT)`,
		`CREATE FUNCTION zz351_trigger() RETURNS trigger LANGUAGE plpgsql AS $$
         BEGIN PERFORM ctx_embed_excluded_group(NEW.scope); RETURN NEW; END $$`,
		`CREATE TRIGGER zz351_trg BEFORE INSERT ON zz351_rows FOR EACH ROW EXECUTE FUNCTION zz351_trigger()`,
		// A quoted name, and a helper reached through a qualified call from
		// a function without a pin: both resolve through the writer's path.
		`CREATE FUNCTION zz351_helper() RETURNS bigint LANGUAGE sql AS $$ SELECT count(*) FROM context_tenant_scopes $$`,
		`CREATE FUNCTION zz351_quoted() RETURNS trigger LANGUAGE plpgsql AS $$
         BEGIN PERFORM 1 FROM "context_embed_excluded_queue"; PERFORM public.zz351_helper(); RETURN NEW; END $$`,
		`CREATE TRIGGER zz351_trg_quoted BEFORE INSERT ON zz351_rows FOR EACH ROW EXECUTE FUNCTION zz351_quoted()`,
	} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("plant: %v", err)
		}
	}
	_, bad = unresolvedFunctions(t, pool)
	if len(bad) != 3 || !strings.HasPrefix(bad[0], "zz351_helper()") || !strings.HasPrefix(bad[1], "zz351_quoted()") || !strings.HasPrefix(bad[2], "zz351_trigger()") {
		t.Fatalf("the check does not name the three planted functions alone: %v", bad)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER zz351_trg_quoted ON zz351_rows`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	emptyPathInsert := func() error {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		defer conn.Release()
		if _, err := conn.Exec(ctx, `SELECT pg_catalog.set_config('search_path', '', false)`); err != nil {
			t.Fatalf("empty search_path: %v", err)
		}
		_, err = conn.Exec(ctx, `INSERT INTO public.zz351_rows VALUES ('a')`)
		if _, rerr := conn.Exec(ctx, `RESET search_path`); rerr != nil {
			t.Fatalf("reset search_path: %v", rerr)
		}
		return err
	}
	var pgErr *pgconn.PgError
	if err := emptyPathInsert(); !errors.As(err, &pgErr) || pgErr.Code != "42883" {
		t.Fatalf("unpinned planted trigger under an empty search_path: want 42883, got %v", err)
	}
	if _, err := pool.Exec(ctx, `ALTER FUNCTION zz351_trigger() SET search_path = pg_catalog, public, pg_temp`); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if err := emptyPathInsert(); err != nil {
		t.Fatalf("pinned planted trigger under an empty search_path: %v", err)
	}
	if _, bad = unresolvedFunctions(t, pool); len(bad) != 0 {
		t.Fatalf("the check still names the pinned function: %v", bad)
	}
}

// TestPinnedFunctionsKeepTheirOtherSettings: migration 179 adds search_path
// to functions that already carry settings; mark_guard_dirty keeps the two of
// migration 177 (issue #337).
func TestPinnedFunctionsKeepTheirOtherSettings(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	var cfg []string
	if err := pool.QueryRow(context.Background(),
		`SELECT coalesce(proconfig, '{}') FROM pg_proc WHERE oid = 'mark_guard_dirty()'::regprocedure`).Scan(&cfg); err != nil {
		t.Fatalf("read proconfig: %v", err)
	}
	sort.Strings(cfg)
	want := []string{"enable_nestloop=off", "jit=off", "search_path=pg_catalog, public, pg_temp"}
	if strings.Join(cfg, "|") != strings.Join(want, "|") {
		t.Fatalf("mark_guard_dirty() proconfig = %v, want %v", cfg, want)
	}
}

// restoreBlock is the part of a block that the triggers keep.
type restoreBlock struct {
	embedExcluded bool
	tsDE, tsEN    *string
	archived      bool
}

func restoreBlocks(t *testing.T, pool *pgxpool.Pool) map[string]restoreBlock {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT id::text, embed_excluded, ts_de::text, ts_en::text, is_archived FROM context_blocks`)
	if err != nil {
		t.Fatalf("read blocks: %v", err)
	}
	defer rows.Close()
	out := map[string]restoreBlock{}
	for rows.Next() {
		var id string
		var b restoreBlock
		if err := rows.Scan(&id, &b.embedExcluded, &b.tsDE, &b.tsEN, &b.archived); err != nil {
			t.Fatalf("scan block: %v", err)
		}
		out[id] = b
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read blocks: %v", err)
	}
	return out
}

// countsBy reads one count per key of a query that returns (key, count).
func countsBy(t *testing.T, pool *pgxpool.Pool, sql string) map[string]int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(), sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[k] = n
	}
	return out
}

const blockCountTruthSQL = `SELECT concat_ws('/', scope, category, is_archived, guard_status), count(*) FROM context_blocks GROUP BY 1`

const blockCountCounterSQL = `
SELECT k, sum(n)::bigint FROM (
    SELECT concat_ws('/', scope, category, is_archived, guard_status) AS k, n FROM context_block_counts
    UNION ALL
    SELECT concat_ws('/', scope, category, is_archived, guard_status), n FROM context_block_count_deltas
) s GROUP BY k HAVING sum(n) <> 0`

func TestDataOnlyRestoreWithTriggersOn(t *testing.T) {
	ctx := context.Background()
	src, srcDSN := testdb.SetupTestDBWithDSN(t)

	// The source through the production write path: a block of an embedded
	// type, a checkpoint (never embedded on the shipped registry), a
	// system-meta block, tags and metadata, and an archived block.
	write := func(title, typ string, tags []string, md map[string]any) string {
		b, err := store.UpsertBlock(ctx, src, "restore-351", title, "body of "+title+" — Wiederherstellung mit Triggern", tags, md, "rs-351", true, store.SensitivityWrite{}, typ)
		if err != nil {
			t.Fatalf("upsert %s: %v", title, err)
		}
		return b.ID
	}
	write("knowledge one", "knowledge", []string{"a", "b"}, map[string]any{"k": "v"})
	write("knowledge two", "knowledge", nil, nil)
	write("checkpoint part", "checkpoint", []string{"cp"}, map[string]any{"session": "s1"})
	write("system meta", "system-meta", nil, map[string]any{"m": 1})
	archived := write("archived one", "knowledge", []string{"old"}, nil)
	if _, err := store.DeleteBlock(ctx, src, archived, []string{"rs-351"}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := store.FoldBlockCountDeltas(ctx, src); err != nil {
		t.Fatalf("fold source deltas: %v", err)
	}
	want := restoreBlocks(t, src)

	if code, out := testdb.RunClientTool(t, srcDSN, "pg_dump", "--data-only", "--schema=public", "-Fc", "-f", "/tmp/ctx351.dump"); code != 0 {
		t.Fatalf("pg_dump exit %d: %s", code, out)
	}
	_, list := testdb.RunClientTool(t, srcDSN, "pg_restore", "--list", "/tmp/ctx351.dump")
	t.Logf("restore order (pg_restore --list):\n%s", list)

	// The target: the schema at the newest migration with every table of it
	// empty, the state a data-only restore expects. The seed rows of the
	// migrations go with the triggers off, so nothing is counted or queued.
	dst, dstDSN := testdb.SetupTestDBWithDSN(t)
	conn, err := dst.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	var tables string
	if err := conn.QueryRow(ctx, `SELECT string_agg(format('%I.%I', schemaname, tablename), ', ') FROM pg_tables WHERE schemaname = 'public'`).Scan(&tables); err != nil {
		t.Fatalf("list tables: %v", err)
	}
	for _, s := range []string{`SET session_replication_role = replica`, `TRUNCATE ` + tables, `RESET session_replication_role`} {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("empty target (%s): %v", s, err)
		}
	}
	conn.Release()

	// The one table whose COPY may fail: the registry restored above queued
	// its names, and a queued entry of the dump with the same key refuses
	// the whole COPY of that table (the dump's entries are queued again).
	code, out := testdb.RunClientTool(t, dstDSN, "pg_restore", "--data-only", "/tmp/ctx351.dump")
	t.Logf("pg_restore --data-only with triggers on: exit %d:\n%s", code, out)
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "pg_restore: error:") {
			continue
		}
		if !strings.HasPrefix(line, `pg_restore: error: COPY failed for table "context_embed_excluded_queue": ERROR:  duplicate key value`) {
			t.Fatalf("pg_restore --data-only with triggers on: %s\n%s", line, out)
		}
	}
	pre := restoreBlocks(t, dst)
	for id, w := range want {
		if g := pre[id]; g.embedExcluded != w.embedExcluded {
			t.Logf("before the reconcile: block %s embed_excluded %v, source %v", id, g.embedExcluded, w.embedExcluded)
		}
	}
	var queued int
	_ = dst.QueryRow(ctx, `SELECT count(*) FROM context_embed_excluded_queue`).Scan(&queued)
	t.Logf("embed_excluded queue after restore: %d rows", queued)
	// pg_restore loads context_blocks before context_block_types and
	// context_tenant_scopes: the trigger marks every block against an empty
	// registry, and the registry's own triggers queue the names it turns.
	// The scheduler's reconcile marks them; here it runs once by hand.
	if res, err := store.ReconcileEmbedExcluded(ctx, dst); err != nil {
		t.Fatalf("embed_excluded reconcile: %v", err)
	} else {
		t.Logf("embed_excluded reconcile: %+v", res)
	}
	var srcAudit, dstAudit int
	_ = src.QueryRow(ctx, `SELECT count(*) FROM context_settings_audit`).Scan(&srcAudit)
	_ = dst.QueryRow(ctx, `SELECT count(*) FROM context_settings_audit`).Scan(&dstAudit)
	t.Logf("context_settings_audit rows: source %d, after restore %d", srcAudit, dstAudit)
	if dstAudit < srcAudit {
		t.Errorf("context_settings_audit lost rows in the restore: source %d, after %d", srcAudit, dstAudit)
	}

	got := restoreBlocks(t, dst)
	if len(got) != len(want) {
		t.Fatalf("restored %d blocks, source has %d", len(got), len(want))
	}
	for id, w := range want {
		g, ok := got[id]
		switch {
		case !ok:
			t.Errorf("block %s missing after restore", id)
		case g.tsDE == nil || g.tsEN == nil:
			t.Errorf("block %s: full-text vectors NULL after restore", id)
		case *g.tsDE != *w.tsDE || *g.tsEN != *w.tsEN:
			t.Errorf("block %s: full-text vectors differ from the source", id)
		case g.embedExcluded != w.embedExcluded:
			t.Errorf("block %s: embed_excluded %v after restore, %v in the source", id, g.embedExcluded, w.embedExcluded)
		case g.archived != w.archived:
			t.Errorf("block %s: is_archived %v after restore, %v in the source", id, g.archived, w.archived)
		}
	}
	var rule int
	if err := dst.QueryRow(ctx, `SELECT count(*) FROM context_blocks
	                              WHERE embed_excluded IS DISTINCT FROM ctx_block_type_never_embedded(type_name, scope)
	                                AND NOT is_archived`).Scan(&rule); err != nil {
		t.Fatalf("rule check: %v", err)
	}
	if rule != 0 {
		t.Errorf("%d live blocks carry an embed_excluded the rule does not give", rule)
	}

	// The counters: the restored context_block_counts already hold the
	// source's totals and every restored block appended its delta once more.
	// The fold adds them, the reconcile corrects them.
	if _, err := store.FoldBlockCountDeltas(ctx, dst); err != nil {
		t.Fatalf("fold target deltas: %v", err)
	}
	truth := countsBy(t, dst, blockCountTruthSQL)
	counters := countsBy(t, dst, blockCountCounterSQL)
	t.Logf("after restore and fold: truth %v counters %v", truth, counters)
	if _, err := store.ReconcileBlockCounts(ctx, dst, ""); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if counters := countsBy(t, dst, blockCountCounterSQL); fmt.Sprint(counters) != fmt.Sprint(truth) {
		t.Errorf("counters after reconcile %v, real counts %v", counters, truth)
	}
}

// TestMigration179WaitsForNoWriter: migration 179 changes function settings
// only and takes no lock on a table. A writer that holds an uncommitted
// insert of context_blocks, run through the triggers it pins, does not hold
// the migration up, and its next statement in the same transaction runs the
// pinned functions.
func TestMigration179WaitsForNoWriter(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, 178)
	writer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin writer: %v", err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	insert := `INSERT INTO context_blocks (category, title, content, scope, type_name) VALUES ('w', $1, 'held by an open writer', 'w-179', 'checkpoint')`
	if _, err := writer.Exec(ctx, insert, "first"); err != nil {
		t.Fatalf("writer insert: %v", err)
	}
	migCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	t0 := time.Now()
	if err := store.RunMigrations(migCtx, pool); err != nil {
		t.Fatalf("migration 179 next to an open writer: %v", err)
	}
	took := time.Since(t0)
	if took > 2*time.Second {
		t.Fatalf("migration 179 took %v next to an open writer; it waits for none", took)
	}
	t.Logf("migration 179 next to an open writer: %v", took)
	if _, err := writer.Exec(ctx, `SELECT pg_catalog.set_config('search_path', '', true)`); err != nil {
		t.Fatalf("writer empty search_path: %v", err)
	}
	if _, err := writer.Exec(ctx, `INSERT INTO public.context_blocks (category, title, content, scope, type_name) VALUES ('w', 'second', 'after the migration', 'w-179', 'checkpoint')`); err != nil {
		t.Fatalf("writer insert after the migration under an empty search_path: %v", err)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatalf("writer commit: %v", err)
	}
	var excluded int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_blocks WHERE scope = 'w-179' AND embed_excluded`).Scan(&excluded); err != nil {
		t.Fatalf("count: %v", err)
	}
	if excluded != 2 {
		t.Fatalf("%d of the writer's two checkpoint blocks marked embed_excluded, want 2", excluded)
	}
}
