//go:build integration

// In-package test on purpose (as guard_explain_integration_test.go): it runs
// the ACTUAL pick (pickPending: guardPickLockSQL, guardPickRelockSQL) and
// pendingStatsSQL — the guard batch's pick and the statement behind manage
// guard-stats' pending_count and dirty_since since migration 181 (issue
// #341) — and the statement before migration 182, which the pick sends
// while idx_guard_pending_type does not stand (issue #360). Since migration
// 183 the schema has no idx_guard_pending; the tests build it where they
// model the state in which 183 keeps it (idx_guard_pending_type not ready).
package guard

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/testdb"
)

// pendingStatsBuffersPerWaiting bounds the shared buffers PendingStats' one
// pass may read per block the pick would take, in the whole store (custom and
// generic plan, all scopes or one). Since migration 182 the count reads
// idx_guard_pending_type with the type allowlist as its index condition
// (issue #360): the entries of the checked types that wait and the heap rows
// behind them. Measured on the production image: 0.059 per waiting block at
// 100 000 waiting (issue #341); the bound is twice that.
const pendingStatsBuffersPerWaiting = 0.12

// pendingBuffersPerScopeRow covers the one part of the count that does not
// follow the waiting blocks: a generic plan joins idx_context_scope in a
// BitmapAnd and reads its entries for every row of the caller's scopes
// (measured 0.00084 per row at 1 200 000 rows, issue #360; a plan choice
// before 182 as well). Twice that, per row of the scopes the count is asked
// for.
const pendingBuffersPerScopeRow = 0.002

// pickBuffersPerRow bounds the pick per row it locks: step 1 per row its
// branches lock (an index entry, the heap page, the lock), step 2 per row it
// locks again (the heap page of the TID, the lock). Measured on the
// production image (issue #360, phase 2): about 1.4 per row in step 1 and
// 2.0 in step 2; the bound is 2.5. pickBuffersPerType covers the descent of
// one branch into idx_guard_pending_type (root, inner, leaf page).
const (
	pickBuffersPerRow  = 2.5
	pickBuffersPerType = 4
)

// pendingStatsBufferSlack covers the fixed part: the index metapages and
// root pages, the registry-free plan's catalog reads.
const pendingStatsBufferSlack = 64

// pickProbeBuffers bounds the probe of idx_guard_pending_type at the start of
// a pick (the catalog, not the table: it does not grow with the store).
// Measured: 236 buffers on a fresh connection, 39 once its catalog caches
// are warm, 3 when the index does not exist.
const pickProbeBuffers = 300

// guardPickLimit is the batch limit of the scheduler (guardBatchLimit).
const guardPickLimit = 100

// v561PickSQL and v561PendingStatsSQL are the pick and the count of a binary
// before migration 182 (v5.61.0, 6a0c3258), as it sends them. The pick is
// the statement guardPickBefore182SQL sends, the count is pendingStatsSQL
// (the rollback image v5.65.0 sends the count of this release).
const (
	v561PickSQL = `SELECT id, type_name FROM context_blocks
			WHERE NOT is_archived
		  AND (metadata->>'guard_checked_at') IS NULL
		  AND embedding IS NOT NULL
		  AND NOT embedding_stale
		  AND category != 'index'
		  AND lifecycle_state = 'knowledge'
		  AND type_name = ANY($1::text[])
			ORDER BY created_at ASC
			LIMIT $2
			FOR NO KEY UPDATE SKIP LOCKED`
	v561PendingStatsSQL = `SELECT count(*)::int, min(updated_at) FROM context_blocks
	WHERE scope = ANY($1::text[])
	  AND NOT is_archived
		  AND (metadata->>'guard_checked_at') IS NULL
		  AND embedding IS NOT NULL
		  AND NOT embedding_stale
		  AND category != 'index'
		  AND lifecycle_state = 'knowledge'
		  AND type_name = ANY($2::text[])`
)

// pickIndex is the index of migration 182 the pick reads; oldPendingIndex is
// the one of 113_baseline, which the statement before 182 reads and which
// migration 183 drops once pickIndex stands (oldPendingIndexDDL builds it).
const (
	pickIndex          = "idx_guard_pending_type"
	oldPendingIndex    = "idx_guard_pending"
	oldPendingIndexDDL = `CREATE INDEX IF NOT EXISTS idx_guard_pending ON context_blocks (created_at)
		WHERE NOT is_archived AND (metadata->>'guard_checked_at') IS NULL AND embedding IS NOT NULL`
)

type pendingStatsCostDB struct {
	t     *testing.T
	pool  *pgxpool.Pool
	dsn   string
	types []string // the GuardCheckTypes allowlist of the shipped registry
	have  int
	// foreign counts the unchecked blocks with a vector the pick filters out
	// (seedForeign); corpus the checked blocks (seedCheckedCorpus).
	foreign, corpus int

	// The pick's own pool: one connection that sends every plan of the pick
	// as a notice (auto_explain with ANALYZE and BUFFERS), collected here.
	pickPool *pgxpool.Pool
	mu       sync.Mutex
	plans    []string

	// stats is the statistics state the measurements claim (statsFresh,
	// statsStale, statsNever; "" once rows changed and no state was claimed
	// since); checkStats proves it before every measurement. modRows counts
	// the rows the test changed since the last analyze, changeAnalyzes is the
	// table's analyze count when it last changed rows.
	stats          string
	modRows        int64
	changeAnalyzes int64
}

// The statistics states of context_blocks a measurement claims (checkStats).
const (
	statsFresh = "fresh"          // analysed after the last row the test changed
	statsStale = "stale"          // analysed, rows changed since, no analyze since
	statsNever = "never analysed" // no row estimate, no column statistics
)

func newPendingStatsCostDB(t *testing.T) *pendingStatsCostDB {
	t.Helper()
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	if err := reg.Reload(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// The HNSW index is not read by the count; dropping it in this throwaway
	// database keeps the seed of 100 000 vectors in seconds. User triggers
	// off during the seed (guard_explain_integration_test.go: the NOTIFY row
	// trigger makes a large insert quadratic); the full-text trigger stays.
	// Autovacuum off: the tests set the statistics state themselves.
	for _, s := range []string{
		`DROP INDEX IF EXISTS idx_embedding_hnsw`,
		`ALTER TABLE context_blocks DISABLE TRIGGER USER`,
		`ALTER TABLE context_blocks ENABLE TRIGGER trg_block_fulltext`,
		`ALTER TABLE context_blocks SET (autovacuum_enabled = false)`,
	} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `ALTER TABLE context_blocks ENABLE TRIGGER USER`) })
	types := reg.Snapshot().GuardCheckTypes()
	if len(types) < 3 || !contains(types, "knowledge") || !contains(types, "reference") || contains(types, "tool-evidence") {
		t.Fatalf("fixture: the shipped registry checks %v; the test needs knowledge and reference checked and tool-evidence not", types)
	}
	d := &pendingStatsCostDB{t: t, pool: pool, dsn: dsn, types: types}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		if i := strings.Index(n.Message, "{"); i >= 0 && strings.Contains(n.Message, "plan:") {
			d.mu.Lock()
			d.plans = append(d.plans, n.Message[i:])
			d.mu.Unlock()
		}
	}
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		for _, s := range []string{
			`LOAD 'auto_explain'`,
			`SET auto_explain.log_min_duration = 0`,
			`SET auto_explain.log_analyze = on`,
			`SET auto_explain.log_buffers = on`,
			`SET auto_explain.log_timing = off`,
			`SET auto_explain.log_format = 'json'`,
			`SET auto_explain.log_level = 'notice'`,
		} {
			if _, err := c.Exec(ctx, s); err != nil {
				return fmt.Errorf("%s: %w", s, err)
			}
		}
		return nil
	}
	if d.pickPool, err = pgxpool.NewWithConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.pickPool.Close)
	return d
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func (d *pendingStatsCostDB) exec(sql string, args ...any) {
	d.t.Helper()
	if _, err := d.pool.Exec(context.Background(), sql, args...); err != nil {
		d.t.Fatalf("%s: %v", sql, err)
	}
}

// write runs a statement that changes rows of context_blocks and books them.
func (d *pendingStatsCostDB) write(sql string, args ...any) {
	d.t.Helper()
	tag, err := d.pool.Exec(context.Background(), sql, args...)
	if err != nil {
		d.t.Fatalf("%s: %v", sql, err)
	}
	d.changed(tag.RowsAffected())
}

// changed books n rows the test changed in context_blocks: they count
// towards n_mod_since_analyze, and the statistics state is no longer the
// one claimed before.
func (d *pendingStatsCostDB) changed(n int64) {
	d.t.Helper()
	if n == 0 {
		return
	}
	d.modRows += n
	d.changeAnalyzes = d.statsNow().analyzes
	d.stats = ""
}

// vacuum sets the visibility map and the row estimate of the table without
// column statistics; analyze is a VACUUM ANALYZE.
func (d *pendingStatsCostDB) vacuum() { d.exec(`VACUUM context_blocks`) }
func (d *pendingStatsCostDB) analyze() {
	d.exec(`VACUUM ANALYZE context_blocks`)
	d.modRows, d.stats = 0, statsFresh
}

// leaveStale claims the statistics of the last analyze, which the rows
// changed since have outdated.
func (d *pendingStatsCostDB) leaveStale() { d.stats = statsStale }

// statsReading is the statistics state of context_blocks as the database
// reports it.
type statsReading struct {
	reltuples float64
	columns   int   // pg_stats rows of the table and its expression indexes, pg_stats_ext rows
	analyzes  int64 // analyze_count + autoanalyze_count
	modSince  int64 // n_mod_since_analyze
}

func (d *pendingStatsCostDB) statsNow() statsReading {
	d.t.Helper()
	var s statsReading
	if err := d.pool.QueryRow(context.Background(), `SELECT c.reltuples::float8,
			(SELECT count(*) FROM pg_stats WHERE schemaname = 'public' AND tablename IN (
				SELECT relname FROM pg_class WHERE oid = 'context_blocks'::regclass OR oid IN (
					SELECT indexrelid FROM pg_index WHERE indrelid = 'context_blocks'::regclass AND indexprs IS NOT NULL)))::int
			+ (SELECT count(*) FROM pg_stats_ext WHERE schemaname = 'public' AND tablename = 'context_blocks')::int,
			s.analyze_count + s.autoanalyze_count, s.n_mod_since_analyze
		FROM pg_class c JOIN pg_stat_user_tables s ON s.relid = c.oid
		WHERE c.oid = 'context_blocks'::regclass`).Scan(&s.reltuples, &s.columns, &s.analyzes, &s.modSince); err != nil {
		d.t.Fatal(err)
	}
	return s
}

// statsModWait bounds the wait for n_mod_since_analyze: a backend reports
// its writes within a second of going idle.
const statsModWait = 30 * time.Second

// checkStats proves the statistics state the next measurement claims
// (d.stats) as the database reports it, and returns it for the log:
//   - fresh: a row estimate and, on a table with rows, column statistics,
//     from an analyze after the last row the test changed;
//   - stale: a row estimate and column statistics from an analyze, rows
//     changed since (n_mod_since_analyze reaches the rows the test changed
//     since), and no analyze since;
//   - never analysed: no row estimate, no column statistics.
//
// An ANALYZE between the state and the measurement (such as one after an
// index build) shows here.
func (d *pendingStatsCostDB) checkStats(what string) string {
	d.t.Helper()
	s := d.statsNow()
	fail := func(why string) {
		d.t.Helper()
		d.t.Errorf("%s: the statistics claimed %q are not: %s (reltuples %.0f, %d column statistics, %d analyzes, %d at the last change, n_mod_since_analyze %d, %d rows changed since the last analyze)",
			what, d.stats, why, s.reltuples, s.columns, s.analyzes, d.changeAnalyzes, s.modSince, d.modRows)
	}
	switch d.stats {
	case statsNever:
		if s.reltuples >= 0 || s.columns > 0 {
			fail("a row estimate or column statistics stand")
		}
	case statsFresh:
		switch {
		case s.reltuples < 0 || (s.reltuples > 0 && s.columns == 0):
			fail("not analysed")
		case s.analyzes <= d.changeAnalyzes:
			fail("rows changed after the last analyze")
		}
	case statsStale:
		switch {
		case s.reltuples < 0 || s.columns == 0:
			fail("not analysed")
		case d.modRows == 0:
			fail("no row changed since the last analyze")
		case s.analyzes != d.changeAnalyzes:
			fail("analysed after the rows changed")
		default:
			// n_mod_since_analyze only grows until the next analyze.
			for deadline := time.Now().Add(statsModWait); s.modSince < d.modRows && time.Now().Before(deadline); s = d.statsNow() {
				time.Sleep(100 * time.Millisecond)
			}
			if s.modSince < d.modRows {
				fail("n_mod_since_analyze does not count the rows changed since the last analyze")
			}
		}
	default:
		d.t.Fatalf("%s: the measurement claims no statistics state", what)
	}
	return fmt.Sprintf("stats %s (reltuples %.0f, %d column statistics, %d analyzes, n_mod_since_analyze %d)", d.stats, s.reltuples, s.columns, s.analyzes, s.modSince)
}

// clearStats removes every column and expression statistic of context_blocks
// and its indexes and the row estimate: the state of a table never analysed.
func (d *pendingStatsCostDB) clearStats() {
	d.t.Helper()
	ctx := context.Background()
	d.exec(`DROP STATISTICS IF EXISTS q360_guard_checked_at`)
	rows, err := d.pool.Query(ctx, `SELECT c.relname, a.attname FROM pg_class c JOIN pg_attribute a ON a.attrelid = c.oid
		 WHERE a.attnum > 0 AND NOT a.attisdropped
		   AND (c.oid = 'context_blocks'::regclass
		        OR c.oid IN (SELECT indexrelid FROM pg_index WHERE indrelid = 'context_blocks'::regclass AND indexprs IS NOT NULL))`)
	if err != nil {
		d.t.Fatal(err)
	}
	type col struct{ rel, att string }
	var cols []col
	for rows.Next() {
		var c col
		if err := rows.Scan(&c.rel, &c.att); err != nil {
			d.t.Fatal(err)
		}
		cols = append(cols, c)
	}
	rows.Close()
	d.exec(`SELECT pg_clear_relation_stats('public', 'context_blocks')`)
	for _, c := range cols {
		d.exec(`SELECT pg_clear_attribute_stats('public', $1::text, $2::text, false)`, c.rel, c.att)
	}
	d.stats = statsNever
}

// expressionStats creates a statistics object on the verdict key and
// analyses it: the state in which the planner estimates the unchecked share
// of the store instead of a fixed 0.5 % (issue #360, phase 2 "+x").
func (d *pendingStatsCostDB) expressionStats() {
	d.exec(`CREATE STATISTICS IF NOT EXISTS q360_guard_checked_at ON ((metadata->>'guard_checked_at')) FROM context_blocks`)
	d.exec(`ANALYZE context_blocks`)
	d.modRows, d.stats = 0, statsFresh
}

// seed adds waiting blocks over 100 scopes (pc-0 … pc-99) and, if checked > 0,
// as many checked blocks (not waiting) in the same scopes. Waiting blocks
// alternate between knowledge and reference, two checked types.
func (d *pendingStatsCostDB) seed(waiting, checked int) {
	d.t.Helper()
	if waiting > 0 {
		d.write(`INSERT INTO context_blocks (category, title, content, scope, type_name, embedding, embed_model)
			SELECT 'pc', 'pc-w-' || ($1::int + g), 'waiting ' || g, 'pc-' || (($1::int + g) % 100),
			       CASE WHEN g % 2 = 0 THEN 'knowledge' ELSE 'reference' END,
			       array_fill(0.01::real, ARRAY[1024])::vector(1024), 'q341'
			  FROM generate_series(1, $2::int) g`, d.have, waiting)
		d.have += waiting
	}
	if checked > 0 {
		d.write(`INSERT INTO context_blocks (category, title, content, scope, type_name, metadata)
			SELECT 'pc', 'pc-c-' || g, 'checked ' || g, 'pc-' || (g % 100), 'knowledge',
			       jsonb_build_object('guard_checked_at', '2026-11-18T00:00:00Z', 'guard_status', 'clean')
			  FROM generate_series(1, $1::int) g`, checked)
	}
	d.vacuum()
}

// seedForeign adds n unchecked blocks with a vector that the pick filters
// out, over the same 100 scopes and OLDER than every waiting block (a walk
// in created_at order meets them first), in the four kinds the pick's
// predicate has beyond the predicate of idx_guard_pending: 50 % a type with
// guard.check = false (tool-evidence), 30 % a lifecycle other than knowledge
// (synthesis), 10 % a stale vector, 10 % category index (system-meta, a
// checked type).
func (d *pendingStatsCostDB) seedForeign(n int) {
	d.t.Helper()
	d.write(`INSERT INTO context_blocks (category, title, content, scope, type_name, embedding, embed_model,
			lifecycle_state, embedding_stale, created_at, updated_at)
		SELECT CASE WHEN m = 9 THEN 'index' ELSE 'pc' END,
		       'pc-f-' || g, 'foreign ' || g, 'pc-' || (g % 100),
		       CASE WHEN m < 5 THEN 'tool-evidence' WHEN m = 9 THEN 'system-meta' ELSE 'knowledge' END,
		       array_fill(0.01::real, ARRAY[1024])::vector(1024), 'q360',
		       CASE WHEN m BETWEEN 5 AND 7 THEN 'synthesis' ELSE 'knowledge' END,
		       m = 8,
		       now() - interval '300 days' + g * interval '1 millisecond',
		       now() - interval '300 days' + g * interval '1 millisecond'
		  FROM (SELECT g, g % 10 AS m FROM generate_series($1::int + 1, $1::int + $2::int) g) s`, d.foreign, n)
	d.foreign += n
	d.vacuum()
}

// seedCheckedCorpus adds n checked blocks with a vector over the same 100
// scopes, older than every waiting block: the bulk of a grown store, which
// neither the pick nor the count may walk.
func (d *pendingStatsCostDB) seedCheckedCorpus(n int) {
	d.t.Helper()
	d.write(`INSERT INTO context_blocks (category, title, content, scope, type_name, embedding, embed_model, metadata, created_at, updated_at)
		SELECT 'pc', 'pc-k-' || ($1::int + g), 'checked ' || g, 'pc-' || (g % 100), CASE WHEN g % 3 = 0 THEN 'reference' ELSE 'knowledge' END,
		       array_fill(0.01::real, ARRAY[1024])::vector(1024), 'q360',
		       jsonb_build_object('guard_checked_at', '2026-11-18T00:00:00Z', 'guard_status', 'clean'),
		       now() - interval '200 days' + g * interval '1 millisecond', now()
		  FROM generate_series(1, $2::int) g`, d.have, n)
	d.have += n
	d.corpus += n
	d.vacuum()
}

type pendingStatsRun struct {
	buffers int64
	ms      float64
	n       int
	plan    string
	conds   string // "index: condition" of every index node
	stats   string // the statistics state it ran in (checkStats)
}

// explain runs a prepared statement once under EXPLAIN (ANALYZE, BUFFERS) as
// a custom plan or under plan_cache_mode = force_generic_plan, and returns
// the plan's buffers and shape; args are SQL literals.
func (d *pendingStatsCostDB) explain(stmt, ptypes, args string, generic bool) pendingStatsRun {
	d.t.Helper()
	stats := d.checkStats(fmt.Sprintf("explain (generic=%v)", generic))
	ctx := context.Background()
	conn, err := d.pool.Acquire(ctx)
	if err != nil {
		d.t.Fatal(err)
	}
	defer conn.Release()
	mode := "force_custom_plan"
	if generic {
		mode = "force_generic_plan"
	}
	if _, err := conn.Exec(ctx, "SET plan_cache_mode = "+mode); err != nil {
		d.t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(ctx, "RESET plan_cache_mode; DEALLOCATE q360") }()
	if _, err := conn.Exec(ctx, "PREPARE q360("+ptypes+") AS "+stmt); err != nil {
		d.t.Fatal(err)
	}
	var raw []byte
	if err := conn.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE q360("+args+")").Scan(&raw); err != nil {
		d.t.Fatal(err)
	}
	var out []struct {
		Plan          map[string]any `json:"Plan"`
		ExecutionTime float64        `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out) != 1 {
		d.t.Fatalf("explain json: %v", err)
	}
	r := pendingStatsRun{ms: out[0].ExecutionTime, plan: planNodes(out[0].Plan), conds: strings.Join(indexConds(out[0].Plan), "; "), stats: stats}
	r.buffers = planBuffers(out[0].Plan)
	return r
}

func planBuffers(pl map[string]any) int64 {
	hit, _ := pl["Shared Hit Blocks"].(float64)
	read, _ := pl["Shared Read Blocks"].(float64)
	return int64(hit + read)
}

func lit(xs []string) string { return "'{" + strings.Join(xs, ",") + "}'::text[]" }

// runCount explains PendingStats' statement (or another count of the same
// parameters) and reads its result.
func (d *pendingStatsCostDB) runCount(stmt string, scopes []string, generic bool) pendingStatsRun {
	d.t.Helper()
	r := d.explain(stmt, "text[], text[]", lit(scopes)+", "+lit(d.types), generic)
	if err := d.pool.QueryRow(context.Background(), stmt, scopes, d.types).Scan(&r.n, new(*time.Time)); err != nil {
		d.t.Fatal(err)
	}
	return r
}

// runOldPick explains the pick of the binary before 182 with the batch
// limit; the result is the number of rows the plan returned.
func (d *pendingStatsCostDB) runOldPick(generic bool) pendingStatsRun {
	d.t.Helper()
	r := d.explain(v561PickSQL, "text[], int", lit(d.types)+", "+fmt.Sprint(guardPickLimit), generic)
	rows, err := d.pool.Query(context.Background(), v561PickSQL, d.types, guardPickLimit)
	if err != nil {
		d.t.Fatal(err)
	}
	for rows.Next() {
		r.n++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		d.t.Fatal(err)
	}
	return r
}

// pickStep is one statement of the pick as auto_explain reported it.
type pickStep struct {
	step    int // 1 = guardPickLockSQL, 2 = guardPickRelockSQL
	buffers int64
	rows    int // rows the statement returned
	plan    string
	conds   string
}

// pickMeasure is one run of pickPending: what it took and what each of its
// statements read.
type pickMeasure struct {
	taken int
	steps []pickStep
	wal   int64  // WAL bytes written while the run picked (row locks)
	stats string // the statistics state it ran in (checkStats)
}

func (m pickMeasure) buffers() int64 {
	var b int64
	for _, s := range m.steps {
		b += s.buffers
	}
	return b
}

// statementBuffers is buffers without the probe of idx_guard_pending_type.
func (m pickMeasure) statementBuffers() int64 {
	var b int64
	for _, s := range m.steps {
		if s.step != 0 {
			b += s.buffers
		}
	}
	return b
}

// probeBuffers is what the probe of idx_guard_pending_type read.
func (m pickMeasure) probeBuffers() int64 {
	return m.buffers() - m.statementBuffers()
}

func (m pickMeasure) String() string {
	var parts []string
	for _, s := range m.steps {
		if s.step == 0 {
			parts = append(parts, fmt.Sprintf("probe: %d buffers", s.buffers))
			continue
		}
		parts = append(parts, fmt.Sprintf("step %d: %d rows %d buffers %s [%s]", s.step, s.rows, s.buffers, s.plan, s.conds))
	}
	return strings.Join(parts, " | ") + fmt.Sprintf(" | WAL %d bytes | %s", m.wal, m.stats)
}

// pick plan modes: a custom plan, a generic plan, and the plan the driver's
// cached statement runs on after five executions (plan_cache_mode auto).
var pickModes = []string{"custom", "generic", "cached"}

// runPick runs pickPending in a transaction it rolls back, on the pick's own
// connection, in the given plan mode, and returns what auto_explain reported
// for the run (the sixth one in mode cached).
func (d *pendingStatsCostDB) runPick(mode string) pickMeasure {
	d.t.Helper()
	stats := d.checkStats("pick (" + mode + ")")
	ctx := context.Background()
	conn, err := d.pickPool.Acquire(ctx)
	if err != nil {
		d.t.Fatal(err)
	}
	defer conn.Release()
	// A fresh connection per measurement: no statement cached by an earlier
	// cell of the test.
	defer conn.Conn().Close(ctx) //nolint:errcheck
	setting := map[string]string{"custom": "force_custom_plan", "generic": "force_generic_plan", "cached": "auto"}[mode]
	if _, err := conn.Exec(ctx, "SET plan_cache_mode = "+setting); err != nil {
		d.t.Fatal(err)
	}
	runs := 1
	if mode == "cached" {
		runs = 6
	}
	var m pickMeasure
	for i := 0; i < runs; i++ {
		d.mu.Lock()
		d.plans = nil
		d.mu.Unlock()
		var lsn0, lsn1 string
		if err := d.pool.QueryRow(ctx, `SELECT pg_current_wal_insert_lsn()::text`).Scan(&lsn0); err != nil {
			d.t.Fatal(err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			d.t.Fatal(err)
		}
		blocks, err := pickPending(ctx, tx, d.types, guardPickLimit)
		if err == nil {
			err = d.pool.QueryRow(ctx, `SELECT pg_current_wal_insert_lsn()::text`).Scan(&lsn1)
		}
		_ = tx.Rollback(ctx)
		if err != nil {
			d.t.Fatalf("pick (%s): %v", mode, err)
		}
		m = pickMeasure{taken: len(blocks), stats: stats}
		if err := d.pool.QueryRow(ctx, `SELECT pg_wal_lsn_diff($1::pg_lsn, $2::pg_lsn)::bigint`, lsn1, lsn0).Scan(&m.wal); err != nil {
			d.t.Fatal(err)
		}
		d.mu.Lock()
		plans := append([]string(nil), d.plans...)
		d.mu.Unlock()
		for _, raw := range plans {
			var out struct {
				Query string         `json:"Query Text"`
				Plan  map[string]any `json:"Plan"`
			}
			if err := json.Unmarshal([]byte(raw), &out); err != nil {
				d.t.Fatalf("auto_explain json: %v: %s", err, raw)
			}
			s := pickStep{buffers: planBuffers(out.Plan), plan: planNodes(out.Plan), conds: strings.Join(indexConds(out.Plan), "; ")}
			if r, ok := out.Plan["Actual Rows"].(float64); ok {
				s.rows = int(r)
			}
			switch {
			case strings.Contains(out.Query, "FROM pg_index"):
				s.step = 0 // the probe of idx_guard_pending_type
			case strings.Contains(out.Query, "CROSS JOIN LATERAL"):
				s.step = 1
			case strings.Contains(out.Query, "ctid = ANY"):
				s.step = 2
			case strings.Contains(out.Query, "ORDER BY created_at ASC"):
				s.step = 182 // the statement before 182 (no usable index)
			default:
				continue
			}
			m.steps = append(m.steps, s)
		}
	}
	if len(m.steps) < 2 || m.steps[0].step != 0 || (m.steps[1].step != 1 && m.steps[1].step != 182) {
		d.t.Fatalf("pick (%s): auto_explain reported no probe and step 1: %v", mode, m)
	}
	return m
}

func planNodes(pl map[string]any) string {
	s := fmt.Sprint(pl["Node Type"])
	if ix, ok := pl["Index Name"]; ok {
		s += "(" + fmt.Sprint(ix) + ")"
	}
	if kids, ok := pl["Plans"].([]any); ok {
		var ks []string
		for _, k := range kids {
			ks = append(ks, planNodes(k.(map[string]any)))
		}
		s += "[" + strings.Join(ks, ",") + "]"
	}
	return s
}

// indexConds lists "index: Index Cond" for every index node of the plan.
func indexConds(pl map[string]any) []string {
	var out []string
	if ix, ok := pl["Index Name"]; ok {
		out = append(out, fmt.Sprintf("%v: %v", ix, pl["Index Cond"]))
	}
	if kids, ok := pl["Plans"].([]any); ok {
		for _, k := range kids {
			out = append(out, indexConds(k.(map[string]any))...)
		}
	}
	return out
}

// indexesOf lists the index names of a plan's index nodes.
func indexesOf(conds string) []string {
	var out []string
	for _, c := range strings.Split(conds, "; ") {
		if i := strings.Index(c, ":"); i > 0 {
			out = append(out, c[:i])
		}
	}
	return out
}

func allPendingScopes() []string {
	s := make([]string, 100)
	for i := range s {
		s[i] = fmt.Sprintf("pc-%d", i)
	}
	return s
}

// waitingIn counts the blocks the pick would take within scopes (nil: the
// whole store), by the pick's own predicate, and the rows of those scopes.
func (d *pendingStatsCostDB) waitingIn(scopes []string) (waiting, rows int) {
	d.t.Helper()
	q := `SELECT count(*) FILTER (WHERE ` + guardPendingWhere("$2") + `)::int, count(*)::int
		FROM context_blocks WHERE ($1::text[] IS NULL OR scope = ANY($1::text[]))`
	if err := d.pool.QueryRow(context.Background(), q, scopes, d.types).Scan(&waiting, &rows); err != nil {
		d.t.Fatal(err)
	}
	return waiting, rows
}

// waitingPerType counts the blocks the pick would take, per checked type.
func (d *pendingStatsCostDB) waitingPerType() map[string]int {
	d.t.Helper()
	rows, err := d.pool.Query(context.Background(), `SELECT type_name, count(*)::int FROM context_blocks
		WHERE `+guardPendingWhere("$1")+` GROUP BY 1`, d.types)
	if err != nil {
		d.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var typ string
		var n int
		if err := rows.Scan(&typ, &n); err != nil {
			d.t.Fatal(err)
		}
		out[typ] = n
	}
	return out
}

// hideIndex makes the planner pass over an index (pg_index.indisvalid =
// false; writes still maintain it) and returns the call that shows it again.
func (d *pendingStatsCostDB) hideIndex(name string) func() {
	d.exec(`UPDATE pg_index SET indisvalid = false WHERE indexrelid = $1::regclass`, name)
	return func() { d.exec(`UPDATE pg_index SET indisvalid = true WHERE indexrelid = $1::regclass`, name) }
}

// withOldIndex builds idx_guard_pending as 113 defines it — the state in which
// migration 183 keeps it, because idx_guard_pending_type does not stand — and
// returns the call that drops it again (a store at 182, where it exists,
// keeps it). No ANALYZE: it would turn a stale or never analysed store
// fresh under the label of the state the measurement claims (checkStats
// proves the state is kept through the build).
func (d *pendingStatsCostDB) withOldIndex() func() {
	d.t.Helper()
	var had bool
	if err := d.pool.QueryRow(context.Background(), `SELECT to_regclass('public.idx_guard_pending') IS NOT NULL`).Scan(&had); err != nil {
		d.t.Fatal(err)
	}
	if had {
		return func() {}
	}
	d.exec(oldPendingIndexDDL)
	return func() { d.exec(`DROP INDEX idx_guard_pending`) }
}

// check runs the count in four shapes (all 100 scopes / one scope, custom /
// generic), for the statement of this binary and the literal of the binary
// before migration 182 (the same statement, which the rollback image sends
// too), on the schema as it stands: since migration 183 without
// idx_guard_pending, so the count reads the waiting entries of
// idx_guard_pending_type only, within the bound for the waiting blocks and
// the rows of the scopes asked for — never the foreign entries the old index
// held, never a scan of the store.
func (d *pendingStatsCostDB) check(label string, wantAll, wantOne int) {
	d.t.Helper()
	waiting, _ := d.waitingIn(nil)
	for _, c := range []struct {
		name   string
		scopes []string
		want   int
	}{{"all scopes", allPendingScopes(), wantAll}, {"one scope of 100", []string{"pc-7"}, wantOne}} {
		_, rows := d.waitingIn(c.scopes)
		bound := int64(float64(waiting)*pendingStatsBuffersPerWaiting+float64(rows)*pendingBuffersPerScopeRow) + pendingStatsBufferSlack
		for _, st := range []struct{ name, sql string }{{"count", pendingStatsSQL}, {"count of v5.61", v561PendingStatsSQL}} {
			for _, generic := range []bool{false, true} {
				r := d.runCount(st.sql, c.scopes, generic)
				d.t.Logf("%s, %s, %s, generic=%v: n=%d waiting=%d foreign=%d corpus=%d scope rows=%d buffers=%d (bound %d) plan %s [%s]",
					label, st.name, c.name, generic, r.n, waiting, d.foreign, d.corpus, rows, r.buffers, bound, r.plan, r.conds)
				if r.n != c.want {
					d.t.Errorf("%s, %s, %s, generic=%v: counted %d, want %d (%s)", label, st.name, c.name, generic, r.n, c.want, r.plan)
				}
				if r.buffers > bound {
					d.t.Errorf("%s, %s, %s, generic=%v: %d buffers for %d waiting blocks beside %d foreign entries and %d checked blocks — over the bound %d (%.2f per waiting block + %.3f per row of the scopes + %d): the count reads more than the waiting entries (%s [%s])",
						label, st.name, c.name, generic, r.buffers, waiting, d.foreign, d.corpus, bound, pendingStatsBuffersPerWaiting, pendingBuffersPerScopeRow, pendingStatsBufferSlack, r.plan, r.conds)
				}
				if strings.Contains(r.conds, oldPendingIndex+":") {
					d.t.Errorf("%s, %s, %s, generic=%v: the count reads %s: %s [%s]", label, st.name, c.name, generic, oldPendingIndex, r.plan, r.conds)
				}
				if waiting+d.foreign >= 1000 && strings.Contains(r.plan, "Seq Scan") {
					d.t.Errorf("%s, %s, %s, generic=%v: plan seq-scans: %s", label, st.name, c.name, generic, r.plan)
				}
			}
		}
	}
}

// checkPick runs the pick of this binary in every plan mode against the
// bound for the rows it locks: per checked type at most the batch limit in
// step 1, the batch in step 2. The bound does not grow with the foreign
// entries or the checked corpus, so a plan that walks either goes over it.
// Step 1 reads idx_guard_pending_type and nothing else, step 2 is a TID
// scan, and the batch is the wantTaken oldest waiting blocks. Then the pick
// with idx_guard_pending_type hidden (checkPickBefore182).
func (d *pendingStatsCostDB) checkPick(label string, wantTaken int) {
	d.t.Helper()
	perType := d.waitingPerType()
	locked := 0
	for _, n := range perType {
		locked += min(n, guardPickLimit)
	}
	waiting, _ := d.waitingIn(nil)
	relocked := 0
	if locked > guardPickLimit {
		relocked = guardPickLimit
	}
	bound := int64(float64(locked+relocked)*pickBuffersPerRow) + int64(len(d.types))*pickBuffersPerType + pendingStatsBufferSlack
	for _, mode := range pickModes {
		m := d.runPick(mode)
		d.t.Logf("%s, pick, %s: took=%d waiting=%d foreign=%d corpus=%d buffers=%d (bound %d) %s",
			label, mode, m.taken, waiting, d.foreign, d.corpus, m.buffers(), bound, m)
		if m.taken != wantTaken {
			d.t.Errorf("%s, pick, %s: took %d, want %d", label, mode, m.taken, wantTaken)
		}
		if m.statementBuffers() > bound {
			d.t.Errorf("%s, pick, %s: %d buffers to take %d of %d waiting blocks beside %d foreign entries and %d checked blocks — over the bound %d (%.1f per row locked + %d per type + %d): the pick walks entries it never takes. %s",
				label, mode, m.statementBuffers(), m.taken, waiting, d.foreign, d.corpus, bound, pickBuffersPerRow, pickBuffersPerType, pendingStatsBufferSlack, m)
		}
		if m.probeBuffers() > pickProbeBuffers {
			d.t.Errorf("%s, pick, %s: the probe of idx_guard_pending_type read %d buffers, over %d", label, mode, m.probeBuffers(), pickProbeBuffers)
		}
		for _, s := range m.steps {
			switch s.step {
			case 1:
				for _, ix := range indexesOf(s.conds) {
					if ix != pickIndex {
						d.t.Errorf("%s, pick, %s: step 1 reads %s, want %s only: %s [%s]", label, mode, ix, pickIndex, s.plan, s.conds)
					}
				}
				if waiting+d.foreign >= 1000 && strings.Contains(s.plan, "Seq Scan") {
					d.t.Errorf("%s, pick, %s: step 1 seq-scans: %s", label, mode, s.plan)
				}
			case 2:
				for _, ix := range indexesOf(s.conds) {
					if ix != "context_blocks_pkey" {
						d.t.Errorf("%s, pick, %s: step 2 reads %s, want a TID scan or the primary key: %s", label, mode, ix, s.plan)
					}
				}
				if strings.Contains(s.plan, "Seq Scan") || strings.Contains(s.plan, "Bitmap") {
					d.t.Errorf("%s, pick, %s: step 2 is %s, want a TID scan or the primary key", label, mode, s.plan)
				}
			case 182:
				d.t.Errorf("%s, pick, %s: the statement before 182 ran although idx_guard_pending_type stands", label, mode)
			}
		}
	}
	d.checkPickBefore182(label)
}

// checkPickBefore182: with idx_guard_pending_type hidden (above the
// threshold of 182 before its out-of-band build, an INVALID or foreign index
// under the name), the pick sends its statement before 182. Migration 183
// keeps idx_guard_pending in exactly that state, and on it the pick costs
// what that statement costs on idx_guard_pending alone, in every plan mode.
// Without idx_guard_pending (the order 183 refuses) the statement walks the
// store; that cost is logged, the reason for the order (docs/operations.md,
// "Migration 183").
func (d *pendingStatsCostDB) checkPickBefore182(label string) {
	d.t.Helper()
	show := d.hideIndex(pickIndex)
	defer show()
	for _, mode := range pickModes {
		m := d.runPick(mode)
		d.t.Logf("%s, pick without idx_guard_pending_type and without idx_guard_pending, %s: took=%d buffers=%d %s", label, mode, m.taken, m.buffers(), m)
	}
	drop := d.withOldIndex()
	defer drop()
	for _, mode := range pickModes {
		old := d.runOldPick(mode == "generic")
		m := d.runPick(mode)
		d.t.Logf("%s, pick without idx_guard_pending_type, idx_guard_pending kept, %s: took=%d buffers=%d (the statement before 182 alone %d) %s", label, mode, m.taken, m.buffers(), old.buffers, m)
		if len(m.steps) != 2 || m.steps[1].step != 182 {
			d.t.Errorf("%s, pick without idx_guard_pending_type, %s: want the probe and the statement before 182, got %s", label, mode, m)
		}
		if m.statementBuffers() > old.buffers+pendingStatsBufferSlack || m.probeBuffers() > pickProbeBuffers {
			d.t.Errorf("%s, pick without idx_guard_pending_type, %s: %d buffers and a probe of %d, the statement before 182 alone %d", label, mode, m.statementBuffers(), m.probeBuffers(), old.buffers)
		}
		if w, _ := d.waitingIn(nil); w+d.foreign+d.corpus >= 1000 &&
			(strings.Contains(m.steps[1].conds, "idx_context_created") || strings.Contains(m.steps[1].plan, "Seq Scan")) {
			d.t.Errorf("%s, pick without idx_guard_pending_type, %s: the statement before 182 walks the store beside idx_guard_pending: %s", label, mode, m)
		}
	}
}

// TestPendingStatsCostFollowsTheWaitingBlocks: Pflichttest 8 of issue #341.
// First a store where 1 % of the blocks wait (1 000 of 100 000, the checked
// ones in the same scopes): the count stays within the bound of the 1 000
// waiting blocks, far below one page per 50 blocks of the corpus. Then
// 10 000 and 100 000 waiting blocks: the backlog pin of issue #360 — the
// pick locks at most the batch limit per checked type, whatever the
// backlog.
func TestPendingStatsCostFollowsTheWaitingBlocks(t *testing.T) {
	d := newPendingStatsCostDB(t)
	d.seed(1000, 99000)
	d.analyze()
	d.check("1 % of 100 000 waiting", 1000, 10)
	d.checkPick("1 % of 100 000 waiting", guardPickLimit)
	d.seed(9000, 0)
	d.analyze()
	d.check("10 000 waiting", 10000, 100)
	d.checkPick("10 000 waiting", guardPickLimit)
	d.seed(90000, 0)
	d.analyze()
	d.check("100 000 waiting", 100000, 1000)
	d.checkPick("100 000 waiting", guardPickLimit)
}

// TestGuardPendingCostBesideForeignEntries (issue #360): the pick and the
// count beside unchecked blocks with a vector that the pick filters out, of
// all four kinds (a type with guard.check = false, another lifecycle, a stale
// vector, category index), older than every waiting block. On
// idx_guard_pending they are entries every pick walked (measured before 182:
// the idle pick 585 buffers beside 10 000, 5 832 beside 100 000). The pick
// reads the waiting blocks only, flat over 0 / 10 000 / 100 000 foreign
// entries, and so does the count. Last the backlog: 10 000 waiting beside
// 100 000 foreign.
func TestGuardPendingCostBesideForeignEntries(t *testing.T) {
	d := newPendingStatsCostDB(t)
	d.analyze()
	d.checkPick("nothing waits, nothing foreign", 0)
	for _, n := range []int{10000, 90000} {
		d.seedForeign(n)
		d.analyze()
		d.checkPick(fmt.Sprintf("nothing waits beside %d foreign", d.foreign), 0)
	}
	d.seed(100, 0)
	d.analyze()
	d.check(fmt.Sprintf("100 waiting beside %d foreign", d.foreign), 100, 1)
	d.checkPick(fmt.Sprintf("100 waiting beside %d foreign", d.foreign), guardPickLimit)
	d.seed(9900, 0)
	d.analyze()
	d.check(fmt.Sprintf("backlog of 10 000 waiting beside %d foreign", d.foreign), 10000, 100)
	d.checkPick(fmt.Sprintf("backlog of 10 000 waiting beside %d foreign", d.foreign), guardPickLimit)
}

// TestGuardPendingCostHundredWaitingBesideForeignEntries: the count and the
// pick with 100 waiting blocks alone and beside 10 000 foreign entries
// (issue #360; before migration 182 the pick read 691 buffers there).
func TestGuardPendingCostHundredWaitingBesideForeignEntries(t *testing.T) {
	d := newPendingStatsCostDB(t)
	d.seed(100, 0)
	d.analyze()
	d.check("100 waiting alone", 100, 1)
	d.checkPick("100 waiting alone", guardPickLimit)
	d.seedForeign(10000)
	d.analyze()
	d.check("100 waiting beside 10 000 foreign", 100, 1)
	d.checkPick("100 waiting beside 10 000 foreign", guardPickLimit)
}

// TestGuardPendingCostBesideACheckedCorpus (issue #360): 100 waiting blocks,
// the newest of the store, beside 10 000 foreign entries and 200 000 checked
// blocks with a vector — the cell that stopped the first build of #360,
// whose pick (ordered by created_at on a type-led index) walked
// idx_context_created through the corpus. The waiting blocks are always the
// newest (a block waits right after its vector is written), so a walk of the
// store in created_at order passes the whole corpus before the first of
// them. The corpus is written last, so its rows lie behind the waiting ones
// on the heap although they are older. The pick and the count must read the
// waiting entries, not the corpus, in every plan mode (custom, generic, the
// driver's cached statement) and statistics state: stale (analysed before
// the corpus was written), never analysed, fresh, and fresh with a
// statistics object on the verdict key (the state in which a created_at
// walk looks cheapest). With idx_guard_pending_type hidden, the statement
// before 182 keeps idx_guard_pending in each state (checkPickBefore182).
func TestGuardPendingCostBesideACheckedCorpus(t *testing.T) {
	d := newPendingStatsCostDB(t)
	d.seedForeign(10000)
	d.seed(100, 0)
	d.analyze()
	d.seedCheckedCorpus(200000)
	for _, st := range []struct {
		name string
		set  func()
	}{
		{"stale", d.leaveStale},
		{"never analysed", d.clearStats},
		{"fresh", d.analyze},
		{"fresh with a statistic on the verdict key", d.expressionStats},
	} {
		st.set()
		label := "100 waiting beside 10 000 foreign and 200 000 checked, statistics " + st.name
		d.check(label, 100, 1)
		d.checkPick(label, guardPickLimit)
	}
}

// TestPendingStatsTimeAtAMillionWaiting is the one-off measurement at the
// target scale (not a time gate): set CTX_GUARD_PENDING_MEASURE=1.
func TestPendingStatsTimeAtAMillionWaiting(t *testing.T) {
	if os.Getenv("CTX_GUARD_PENDING_MEASURE") == "" {
		t.Skip("measurement; set CTX_GUARD_PENDING_MEASURE=1")
	}
	d := newPendingStatsCostDB(t)
	for d.have < 1_000_000 {
		d.seed(100_000, 0)
	}
	d.analyze()
	for _, c := range []struct {
		name   string
		scopes []string
	}{{"all scopes", allPendingScopes()}, {"one scope of 100", []string{"pc-7"}}} {
		for _, generic := range []bool{false, true} {
			var ms []float64
			var last pendingStatsRun
			for i := 0; i < 5; i++ {
				last = d.runCount(pendingStatsSQL, c.scopes, generic)
				ms = append(ms, last.ms)
			}
			sort.Float64s(ms)
			t.Logf("1 000 000 waiting, %s, generic=%v: n=%d median %.1f ms (runs %v) buffers=%d plan %s", c.name, generic, last.n, ms[2], ms, last.buffers, last.plan)
		}
	}
}

// TestGuardPickCostAtTheTargetScale is the one-off measurement of the cell
// named in issue #360 (not a gate): one million foreign entries beside one
// million checked blocks, rows as wide as the live ones, the heap in
// created_at order (CTX_GUARD_PICK_ORDER=random: in random order). It logs
// the pick of this binary as the schema stands since 183 and beside
// idx_guard_pending, and the statement before 182 with idx_guard_pending_type
// hidden, without idx_guard_pending (the order migration 183 refuses) and
// with it kept, per statistics state (proven before every measurement,
// checkStats; stale: analysed before the waiting rows changed) and plan
// mode. Set
// CTX_GUARD_PICK_MEASURE=1; CTX_GUARD_PICK_FOREIGN / _CORPUS change the
// sizes.
func TestGuardPickCostAtTheTargetScale(t *testing.T) {
	if os.Getenv("CTX_GUARD_PICK_MEASURE") == "" {
		t.Skip("measurement; set CTX_GUARD_PICK_MEASURE=1")
	}
	size := func(env string, def int) int {
		if v := os.Getenv(env); v != "" {
			var n int
			if _, err := fmt.Sscan(v, &n); err == nil {
				return n
			}
		}
		return def
	}
	foreign, corpus := size("CTX_GUARD_PICK_FOREIGN", 1_000_000), size("CTX_GUARD_PICK_CORPUS", 1_000_000)
	order := "g"
	if os.Getenv("CTX_GUARD_PICK_ORDER") == "random" {
		order = "random()"
	}
	d := newPendingStatsCostDB(t)
	// A dimensionless one-element vector keeps the heap row as wide as the
	// TOAST pointer of a live 4 KB vector and the seed in minutes; the pads
	// give live row widths (about 5 rows per heap page; live 5.6).
	d.exec(`ALTER TABLE context_blocks ALTER COLUMN embedding TYPE vector`)
	const pad = `repeat('lorem ipsum dolor sit amet ', 10)`
	const metaPad = `repeat('m', 260)`
	conn, err := d.pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seed := func(sql string, args ...any) {
		t.Helper()
		t0 := time.Now()
		tag, err := conn.Exec(context.Background(), sql, args...)
		if err != nil {
			t.Fatal(err)
		}
		d.changed(tag.RowsAffected())
		t.Logf("seed %s", time.Since(t0).Round(time.Second))
	}
	_, _ = conn.Exec(context.Background(), `SET work_mem = '256MB'`)
	seed(`INSERT INTO context_blocks (category, title, content, scope, type_name, embedding, embed_model, metadata,
			lifecycle_state, embedding_stale, created_at, updated_at)
		SELECT CASE WHEN m = 9 THEN 'index' ELSE 'pc' END, 'pc-f-' || g, 'foreign ' || g || ' ' || `+pad+`, 'pc-' || (g % 100),
		       CASE WHEN m < 5 THEN 'tool-evidence' WHEN m = 9 THEN 'system-meta' ELSE 'knowledge' END,
		       '[0.01]'::vector, 'q360', jsonb_build_object('pad', `+metaPad+`),
		       CASE WHEN m BETWEEN 5 AND 7 THEN 'synthesis' ELSE 'knowledge' END, m = 8, ts, ts
		  FROM (SELECT g, g % 10 AS m, now() - interval '400 days' + g * interval '10 milliseconds' AS ts
		          FROM generate_series(1, $1::int) g ORDER BY `+order+`) s`, foreign)
	d.foreign = foreign
	seed(`INSERT INTO context_blocks (category, title, content, scope, type_name, embedding, embed_model, metadata, created_at, updated_at)
		SELECT 'pc', 'pc-k-' || g, 'checked ' || g || ' ' || `+pad+`, 'pc-' || (g % 100),
		       CASE WHEN g % 3 = 0 THEN 'reference' ELSE 'knowledge' END, '[0.01]'::vector, 'q360',
		       jsonb_build_object('guard_checked_at', '2026-11-18T00:00:00Z', 'guard_status', 'clean', 'pad', `+metaPad+`), ts, ts
		  FROM (SELECT g, now() - interval '200 days' + g * interval '10 milliseconds' AS ts
		          FROM generate_series(1, $1::int) g ORDER BY `+order+`) s`, corpus)
	d.corpus = corpus
	conn.Release()
	d.analyze()
	waiting := func(n int) {
		t.Helper()
		d.write(`DELETE FROM context_blocks WHERE title LIKE 'pc-w-%'`)
		if n > 0 {
			d.write(`INSERT INTO context_blocks (category, title, content, scope, type_name, embedding, embed_model, metadata, created_at, updated_at)
				SELECT 'pc', 'pc-w-' || g, 'waiting ' || g || ' ' || `+pad+`, 'pc-' || (g % 100),
				       CASE WHEN g % 2 = 0 THEN 'knowledge' ELSE 'reference' END, '[0.01]'::vector, 'q360',
				       jsonb_build_object('pad', `+metaPad+`), now() + g * interval '1 millisecond', now() + g * interval '1 millisecond'
				  FROM generate_series(1, $1::int) g`, n)
		}
		d.exec(`VACUUM (INDEX_CLEANUP ON) context_blocks`)
	}
	var heapPages, rows float64
	if err := d.pool.QueryRow(context.Background(), `SELECT relpages, reltuples FROM pg_class WHERE oid = 'context_blocks'::regclass`).Scan(&heapPages, &rows); err != nil {
		t.Fatal(err)
	}
	t.Logf("store: %.0f rows on %.0f heap pages (%.1f rows per page), foreign %d, checked %d, heap order %s", rows, heapPages, rows/heapPages, foreign, corpus, order)
	// W=0 last: its stale cell keeps the statistics taken beside the
	// 10 000 waiting blocks of the cell before (a drained backlog).
	for _, w := range []int{100, 10000, 0} {
		for _, st := range []struct {
			name string
			set  func()
		}{
			{"stale", d.leaveStale},
			{"never", d.clearStats},
			{"fresh", d.analyze},
		} {
			if st.name == "stale" {
				d.analyze()
				waiting(w)
			} else {
				waiting(w)
			}
			st.set()
			label := fmt.Sprintf("W=%d stats=%s", w, st.name)
			for _, schema := range []string{"with idx_guard_pending", "as is (183)"} {
				show := func() {}
				if schema == "with idx_guard_pending" {
					show = d.withOldIndex()
				}
				for _, mode := range pickModes {
					m := d.runPick(mode)
					t.Logf("MEASURE|%s|pick|%s|%s|took=%d|buffers=%d|%s", label, schema, mode, m.taken, m.buffers(), m)
				}
				show()
			}
			for _, generic := range []bool{false, true} {
				show := d.hideIndex(pickIndex)
				walk := d.runOldPick(generic)
				drop := d.withOldIndex()
				kept := d.runOldPick(generic)
				drop()
				show()
				t.Logf("MEASURE|%s|statement before 182 without idx_guard_pending_type|generic=%v|without idx_guard_pending %d buffers %s %s|idx_guard_pending kept %d buffers %s %s", label, generic, walk.buffers, walk.plan, walk.stats, kept.buffers, kept.plan, kept.stats)
			}
		}
	}
}

// runBuffers sums the shared buffers auto_explain reported for every
// statement of fn, run on the pick's own connection in a transaction that is
// rolled back (the data stays as it was for the next measurement).
func (d *pendingStatsCostDB) runBuffers(fn func(ctx context.Context, tx pgx.Tx) (int, error)) (int64, int) {
	d.t.Helper()
	d.checkStats("guard run")
	ctx := context.Background()
	conn, err := d.pickPool.Acquire(ctx)
	if err != nil {
		d.t.Fatal(err)
	}
	defer conn.Release()
	d.mu.Lock()
	d.plans = nil
	d.mu.Unlock()
	tx, err := conn.Begin(ctx)
	if err != nil {
		d.t.Fatal(err)
	}
	n, err := fn(ctx, tx)
	_ = tx.Rollback(ctx)
	if err != nil {
		d.t.Fatal(err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var total int64
	for _, raw := range d.plans {
		var out struct {
			Plan map[string]any `json:"Plan"`
		}
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			d.t.Fatal(err)
		}
		total += planBuffers(out.Plan)
	}
	return total, n
}

// TestGuardRunBuffersOverAHundredBlocks measures one guard run over 100
// waiting blocks with a vector each, end to end (the pick and every
// statement of the hundred checks, verdicts and audit rows), with the pick
// of this binary and with the pick before migration 182 (v5.61.0) followed
// by the same checks — alone and beside 10 000 foreign entries, with
// idx_guard_pending built as it stood before migration 183. Logged, not a
// gate; the pick's own bounds are the pins above.
func TestGuardRunBuffersOverAHundredBlocks(t *testing.T) {
	d := newPendingStatsCostDB(t)
	set := func() *blocktype.Set {
		reg := blocktype.NewRegistry()
		if err := reg.Reload(context.Background(), d.pool); err != nil {
			t.Fatal(err)
		}
		return reg.Snapshot()
	}()
	d.exec(`ALTER TABLE context_blocks ENABLE TRIGGER USER`)
	d.exec(`CREATE INDEX idx_embedding_hnsw ON context_blocks USING hnsw ((embedding::halfvec(1024)) halfvec_cosine_ops) WITH (m = 16, ef_construction = 64)`)
	vec := `(SELECT array_agg(random() - 0.5)::real[] FROM generate_series(1, 1024 + 0 * g))::vector(1024)`
	d.write(`INSERT INTO context_blocks (category, title, content, scope, type_name, embedding, embed_model, metadata)
		SELECT 'pc', 'pc-k-' || g, 'checked ' || g, 'private', 'knowledge', `+vec+`, 'q360',
		       jsonb_build_object('guard_checked_at', '2026-11-18T00:00:00Z', 'guard_status', 'clean')
		  FROM generate_series(1, 1000) g`)
	d.write(`INSERT INTO context_blocks (category, title, content, scope, type_name, embedding, embed_model)
		SELECT 'pc', 'pc-w-' || g, 'waiting ' || g, 'private', CASE WHEN g % 2 = 0 THEN 'knowledge' ELSE 'reference' END, `+vec+`, 'q360'
		  FROM generate_series(1, 100) g`)
	defer d.withOldIndex()()
	d.analyze()
	oldRun := func(ctx context.Context, tx pgx.Tx) (int, error) {
		rows, err := tx.Query(ctx, v561PickSQL, d.types, guardPickLimit)
		if err != nil {
			return 0, err
		}
		var blocks []pendingBlock
		for rows.Next() {
			var b pendingBlock
			if err := rows.Scan(&b.id, &b.typeName); err != nil {
				return 0, err
			}
			blocks = append(blocks, b)
		}
		rows.Close()
		n := 0
		for _, b := range blocks {
			if processBlock(ctx, tx, b, set) {
				n++
			}
		}
		return n, rows.Err()
	}
	newRun := func(ctx context.Context, tx pgx.Tx) (int, error) {
		blocks, err := pickPending(ctx, tx, d.types, guardPickLimit)
		if err != nil {
			return 0, err
		}
		n := 0
		for _, b := range blocks {
			if processBlock(ctx, tx, b, set) {
				n++
			}
		}
		return n, nil
	}
	for _, foreign := range []int{0, 10000} {
		if foreign > d.foreign {
			d.seedForeign(foreign - d.foreign)
			d.analyze()
		}
		for i := 0; i < 2; i++ { // the second round reads a warm cache
			ob, on := d.runBuffers(oldRun)
			nb, nn := d.runBuffers(newRun)
			t.Logf("MEASURE|run over 100 blocks beside %d foreign, round %d: pick before 182 + checks %d buffers (%d processed); this pick + checks %d buffers (%d processed); difference %+d (%+.2f %%)",
				d.foreign, i+1, ob, on, nb, nn, nb-ob, 100*float64(nb-ob)/float64(ob))
			if on != 100 || nn != 100 {
				t.Errorf("processed %d / %d blocks, want 100 each", on, nn)
			}
		}
	}
}
