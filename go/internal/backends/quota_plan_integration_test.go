//go:build integration

// W06-4 gate (c): the tenant spend reads at most one hour of raw
// context_llm_log rows per window, whatever the window's span, and reads the
// rest from the hourly rollup through idx_llm_usage_key.
//
// The test drives the accountant through a recording handle and runs
// EXPLAIN (ANALYZE, FORMAT JSON) on every statement the spend issued against
// the log, with the arguments it issued — so it checks whatever SQL the
// accountant actually runs, the way the planner actually plans it, under the
// custom plan of a one-shot statement and under the generic plan a cached
// prepared statement can switch to. Before W06-4 the spend was one 30-day
// scan over the hypertable: no upper bound on created_at, every row of the
// tenant's thirty days read.
//
//	go test -tags=integration -p 1 ./internal/backends/ -run TestQuotaSpendPlan -count=1 -v
package backends_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// recordedStmt is one statement the accountant sent, with its arguments.
type recordedStmt struct {
	sql  string
	args []any
}

// recordingDB passes every call through to the pool and records it.
type recordingDB struct {
	pool  *pgxpool.Pool
	mu    sync.Mutex
	stmts []recordedStmt
}

func (r *recordingDB) record(sql string, args []any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stmts = append(r.stmts, recordedStmt{sql: sql, args: append([]any(nil), args...)})
}

func (r *recordingDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.record(sql, args)
	return r.pool.Query(ctx, sql, args...)
}

func (r *recordingDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	r.record(sql, args)
	return r.pool.QueryRow(ctx, sql, args...)
}

// planScan is what one plan reads from the raw log and how it reaches the
// rollup.
type planScan struct {
	rawRows      int64    // raw rows the scans touched: returned + removed by a filter or a recheck
	unbounded    []string // raw scan conditions without a created_at bound on both sides
	rawIndexes   []string // index of every raw scan ("" for a scan without one)
	lookups      int      // executed raw scans of a relief lookup (bounded by min(bucket_start))
	rollupIndex  bool     // the rollup was read through idx_llm_usage_key
	rollupSeq    bool     // the rollup was read by a sequential scan
	rollupAccess []string // node type and index of every rollup read
	execMs       float64
	sharedBlocks float64
}

var (
	createdLower = regexp.MustCompile(`created_at >`)
	createdUpper = regexp.MustCompile(`created_at <`)
)

// isRawRelation reports whether a plan node reads context_llm_log or one of
// its chunks (the rollup is a plain table, so every chunk here is the log's).
func isRawRelation(name string) bool {
	return name == "context_llm_log" || strings.HasPrefix(name, "_hyper_")
}

func num(n map[string]any, k string) float64 {
	v, _ := n[k].(float64)
	return v
}

func str(n map[string]any, k string) string {
	v, _ := n[k].(string)
	return v
}

// scanIndex names the index a scan node reads: its own, or for a bitmap
// heap scan the one of its bitmap index scan.
func scanIndex(n map[string]any) string {
	if idx := str(n, "Index Name"); idx != "" {
		return idx
	}
	if kids, ok := n["Plans"].([]any); ok {
		for _, k := range kids {
			if m, ok := k.(map[string]any); ok && str(m, "Node Type") == "Bitmap Index Scan" {
				return str(m, "Index Name")
			}
		}
	}
	return ""
}

func walkPlan(n map[string]any, out *planScan) {
	rel := str(n, "Relation Name")
	switch str(n, "Node Type") {
	case "Seq Scan", "Index Scan", "Index Only Scan", "Bitmap Heap Scan":
		if isRawRelation(rel) {
			out.rawRows += int64(num(n, "Actual Rows")*num(n, "Actual Loops") +
				num(n, "Rows Removed by Filter") + num(n, "Rows Removed by Index Recheck"))
			cond := str(n, "Index Cond") + " " + str(n, "Recheck Cond") + " " + str(n, "Filter")
			if !createdLower.MatchString(cond) || !createdUpper.MatchString(cond) {
				out.unbounded = append(out.unbounded, rel+": "+strings.TrimSpace(cond))
			}
			out.rawIndexes = append(out.rawIndexes, scanIndex(n))
			if strings.Contains(cond, "min(") && num(n, "Actual Loops") > 0 {
				out.lookups++
			}
		}
		if rel == "context_llm_usage_hourly" && str(n, "Node Type") == "Seq Scan" {
			out.rollupSeq = true
		}
	}
	if idx := str(n, "Index Name"); rel == "context_llm_usage_hourly" || strings.Contains(idx, "llm_usage") {
		out.rollupAccess = append(out.rollupAccess, strings.TrimSpace(str(n, "Node Type")+" "+idx+" "+str(n, "Index Cond")))
	}
	if str(n, "Index Name") == "idx_llm_usage_key" {
		out.rollupIndex = true
	}
	if kids, ok := n["Plans"].([]any); ok {
		for _, k := range kids {
			if m, ok := k.(map[string]any); ok {
				walkPlan(m, out)
			}
		}
	}
}

// planDump is where explainJSON writes every plan when CTX_W064_PLAN_DIR is
// set (the report quotes them), each under the next name of planNames.
var planNames []string

func explainJSON(t *testing.T, raw []byte) planScan {
	t.Helper()
	if dir := os.Getenv("CTX_W064_PLAN_DIR"); dir != "" && len(planNames) > 0 {
		if err := os.WriteFile(filepath.Join(dir, planNames[0]+".json"), raw, 0o600); err != nil {
			t.Fatalf("dump plan: %v", err)
		}
		planNames = planNames[1:]
	}
	var doc []map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc) != 1 {
		t.Fatalf("explain json: %v (%d documents)", err, len(doc))
	}
	plan, _ := doc[0]["Plan"].(map[string]any)
	out := planScan{execMs: num(doc[0], "Execution Time")}
	walkPlan(plan, &out)
	out.sharedBlocks = num(plan, "Shared Hit Blocks") + num(plan, "Shared Read Blocks")
	return out
}

// sqlLiteral renders one recorded argument for EXECUTE.
func sqlLiteral(t *testing.T, a any) (lit, typ string) {
	t.Helper()
	switch v := a.(type) {
	case []string:
		return "'{" + strings.Join(v, ",") + "}'", "uuid[]"
	case time.Time:
		return "'" + v.UTC().Format("2006-01-02 15:04:05.999999") + "+00'", "timestamptz"
	case string:
		return "'" + strings.ReplaceAll(v, "'", "''") + "'", "text"
	}
	t.Fatalf("no literal for argument %T", a)
	return "", ""
}

// explainCustom plans the statement once with its arguments bound.
func explainCustom(t *testing.T, pool *pgxpool.Pool, s recordedStmt) planScan {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+s.sql, s.args...).Scan(&raw); err != nil {
		t.Fatalf("explain: %v\n%s", err, s.sql)
	}
	return explainJSON(t, raw)
}

// explainGeneric plans the statement as a prepared statement under
// plan_cache_mode = force_generic_plan: the plan a cached statement of the
// driver can switch to after five executions.
func explainGeneric(t *testing.T, pool *pgxpool.Pool, s recordedStmt) planScan {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	lits, types := make([]string, len(s.args)), make([]string, len(s.args))
	for i, a := range s.args {
		lits[i], types[i] = sqlLiteral(t, a)
	}
	steps := []string{
		"SET plan_cache_mode = force_generic_plan",
		fmt.Sprintf("PREPARE w064_plan(%s) AS %s", strings.Join(types, ", "), s.sql),
	}
	for _, st := range steps {
		if _, err := conn.Exec(ctx, st); err != nil {
			t.Fatalf("%s: %v", strings.SplitN(st, "(", 2)[0], err)
		}
	}
	defer func() {
		_, _ = conn.Exec(ctx, "DEALLOCATE w064_plan")
		_, _ = conn.Exec(ctx, "RESET plan_cache_mode")
	}()
	var raw []byte
	if err := conn.QueryRow(ctx, fmt.Sprintf("EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE w064_plan(%s)",
		strings.Join(lits, ", "))).Scan(&raw); err != nil {
		t.Fatalf("explain execute: %v", err)
	}
	return explainJSON(t, raw)
}

func TestQuotaSpendPlan(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// A hundred tenants with one key each, one of them with a policy: the
	// tenant's rows are a hundredth of the log, as on a shared instance.
	// Every key logs two calls an hour over 31 days — an external one with a
	// cost at the full hour and a local one at its last microsecond — about
	// 149 000 log rows and as many rollup rows, written in time order, so a
	// heap page holds the rows of many keys, as it does when tenants share an
	// instance. The minutes are fixed whatever the clock: a window's raw edge
	// (since, next full hour) always holds the local row and never the
	// external one, so the external windows always need both relief lookups
	// and the all-locality window the cost lookup — the plans below run them.
	const tenants = 100
	keys := make([]string, 0, tenants)
	var scope string
	for i := 0; i < tenants; i++ {
		s, k := seedQuotaTenant(t, pool, fmt.Sprintf("w064-plan-%02d", i), fmt.Sprintf("w064-plan-%02d", i))
		if i == 0 {
			scope = s
		}
		keys = append(keys, k)
	}
	// The test clock is not the wall clock: it is the wall clock's minute of
	// the hour, one to two hours before anchor, the start of the
	// context_llm_log chunk (7 days, boundaries Thursday 00:00 UTC) that holds
	// now(). The seed ends at the test clock and the accountant reads its
	// windows at it, so the newest chunk holds almost seven days of the seed
	// and the oldest one about three, at every wall clock. A seed ending at
	// now() leaves the newest chunk with whatever part of its seven days has
	// passed — on Thursday morning a few rows of every key — and the plan the
	// relief lookups get on such a chunk (their created_at bound is a runtime
	// value, so every chunk is planned) reads the chunk's created_at index:
	// the right plan for a chunk of a few pages, and one that says nothing
	// about the access path at scale, which is what the index check below
	// pins. The minute of the hour stays the wall clock's, so the edges keep
	// every width (#129) — only the chunk layout is fixed. Every row the seed
	// writes lies before the test clock's next full hour, so the rollup is the
	// one a seed at now() writes, moved by the same interval: no bucket after
	// the windows' end, no window sum changed.
	var anchor time.Time
	if err := pool.QueryRow(ctx, `
		SELECT time_bucket(d.time_interval, now(), origin => 'epoch'::timestamptz)
		  FROM timescaledb_information.dimensions d
		 WHERE d.hypertable_name = 'context_llm_log' AND d.column_name = 'created_at'`).Scan(&anchor); err != nil {
		t.Fatalf("chunk start of now(): %v", err)
	}
	wall := time.Now()
	testNow := anchor.Add(-2*time.Hour + wall.Sub(wall.Truncate(time.Hour)))
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_llm_log (created_at, pipeline, model, host, duration_ms, api_key_id, cost_usd, backend_locality)
		SELECT h + v.off, 'query-synthesize', 'm', 'h', 10, k::uuid, v.cost, v.loc
		  FROM unnest($1::text[]) k,
		       generate_series(date_trunc('hour', $2::timestamptz, 'UTC') - interval '31 days', $2::timestamptz, interval '1 hour') h,
		       (VALUES (interval '0', 0.00100000::numeric, 'external'),
		               (interval '1 hour' - interval '1 microsecond', NULL::numeric, 'local')) v(off, cost, loc)
		 ORDER BY h + v.off, k`,
		keys, testNow); err != nil {
		t.Fatalf("seed log: %v", err)
	}
	var newestEnd time.Time
	if err := pool.QueryRow(ctx, `
		SELECT max(range_end) FROM timescaledb_information.chunks
		 WHERE hypertable_name = 'context_llm_log'`).Scan(&newestEnd); err != nil {
		t.Fatalf("newest chunk: %v", err)
	}
	if !newestEnd.Equal(anchor) {
		t.Fatalf("the newest chunk ends at %s, want the anchor %s — the test clock %s is off the chunk grid", newestEnd, anchor, testNow)
	}
	// The seed is discrete: every key logs at the same two instants of every
	// hour, so a seven-day chunk holds about 336 distinct created_at values
	// of a hundred rows each. Under the default statistics target those
	// equally common instants are too many for the most-common-value list
	// and land in a histogram. For the edge's strict lower bound
	// (created_at > since) the planner then subtracts the share of
	// created_at = since, one instant's worth (1/n_distinct, ~100 rows), from
	// a share the histogram interpolates at a few rows per minute: for an
	// edge shorter than about forty minutes the estimate went negative and
	// was clamped to one row while the edge holds a hundred (one instant of
	// every key), and the custom plan read it through the chunk's smaller
	// created_at index — every key's edge row instead of the tenant's, at
	// about half of all clocks (#129). Production timestamps are all but
	// unique (n_distinct ≈ −1), so that share is about one row there.
	// seedStatsTarget keeps every instant of a chunk as a most-common value
	// (and makes ANALYZE read every row of it): the planner sees the seeded
	// data exactly, at every clock, and the gate measures the access path,
	// not an estimation artifact. The check below fails the test when that
	// stops holding (a denser seed, a longer chunk interval).
	const seedStatsTarget = 1000 // ≥ a chunk's ~336 instants; 300 × target sample rows ≥ its 33 600 rows
	for _, st := range []string{
		fmt.Sprintf("ALTER TABLE context_llm_log ALTER COLUMN created_at SET STATISTICS %d", seedStatsTarget),
		"ANALYZE context_llm_log",
		"ANALYZE context_llm_usage_hourly",
	} {
		if _, err := pool.Exec(ctx, st); err != nil {
			t.Fatalf("%s: %v", st, err)
		}
	}
	var chunks, exact int
	if err := pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE s.histogram_bounds IS NULL
		                          AND abs((SELECT sum(f::float8) FROM unnest(s.most_common_freqs) f) - 1) < 1e-4)
		  FROM timescaledb_information.chunks c
		  LEFT JOIN pg_stats s ON s.schemaname = c.chunk_schema AND s.tablename = c.chunk_name
		                      AND s.attname = 'created_at'
		 WHERE c.hypertable_name = 'context_llm_log'`).Scan(&chunks, &exact); err != nil {
		t.Fatalf("read chunk statistics: %v", err)
	}
	if chunks == 0 || exact != chunks {
		t.Fatalf("created_at statistics are exact (every row in a most-common value) on %d of %d chunks of context_llm_log — the planner would estimate the discrete seed from a histogram (#129)", exact, chunks)
	}
	var perHour, inThirtyDays int64
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT max(c) FROM (SELECT count(*) AS c FROM context_llm_log WHERE api_key_id = $1::uuid
		                             GROUP BY date_trunc('hour', created_at, 'UTC')) h),
		       (SELECT count(*) FROM context_llm_log WHERE api_key_id = $1::uuid
		                          AND created_at > $2::timestamptz - interval '30 days')`, keys[0], testNow).Scan(&perHour, &inThirtyDays); err != nil {
		t.Fatalf("count tenant rows: %v", err)
	}
	setQuota(t, pool, scope, fpv(1.0), ipv(1_000_000), "external_off")

	rec := &recordingDB{pool: pool}
	a := backends.NewQuotaAccountant(rec, time.Hour)
	a.SetClockForTest(func() time.Time { return testNow })
	if err := a.RefreshNow(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// The raw part of a window is its edge — inside one clock hour — plus,
	// for each of the two reliefs (calls, cost) the edge holds no row for,
	// one lookup over one clock hour of raw rows. Three hours of the tenant's
	// rows is the ceiling for any span; the 30-day scan reads all thirty days.
	ceiling := 3 * perHour
	var spendStmts, lookups int
	for _, s := range rec.stmts {
		if !strings.Contains(s.sql, "context_llm_log") {
			continue
		}
		spendStmts++
		for _, mode := range []string{"custom", "generic"} {
			planNames = append(planNames, fmt.Sprintf("stmt%d-%s", spendStmts, mode))
			var p planScan
			if mode == "custom" {
				p = explainCustom(t, pool, s)
			} else {
				p = explainGeneric(t, pool, s)
			}
			t.Logf("statement %d (%s plan): raw rows touched %d (ceiling %d, tenant rows in 30 d %d), relief lookups run %d, rollup via idx_llm_usage_key %v, rollup seq scan %v, %0.f shared blocks, %.3f ms; rollup access %q",
				spendStmts, mode, p.rawRows, ceiling, inThirtyDays, p.lookups, p.rollupIndex, p.rollupSeq, p.sharedBlocks, p.execMs, p.rollupAccess)
			if len(p.unbounded) > 0 {
				t.Errorf("statement %d (%s plan): raw scan without a two-sided created_at bound: %q", spendStmts, mode, p.unbounded)
			}
			if p.rawRows > ceiling {
				t.Errorf("statement %d (%s plan): touched %d raw rows, ceiling is %d (three hours of the tenant's rows)", spendStmts, mode, p.rawRows, ceiling)
			}
			if !p.rollupIndex || p.rollupSeq {
				t.Errorf("statement %d (%s plan): the rollup is not read through idx_llm_usage_key (index %v, seq scan %v)", spendStmts, mode, p.rollupIndex, p.rollupSeq)
			}
			for _, idx := range p.rawIndexes {
				if !strings.HasSuffix(idx, "idx_llm_log_apikey") {
					t.Errorf("statement %d (%s plan): a raw scan reads %q, not idx_llm_log_apikey", spendStmts, mode, idx)
					break
				}
			}
			lookups += p.lookups
		}
	}
	if spendStmts == 0 {
		t.Fatal("the refresh issued no statement against context_llm_log")
	}
	t.Logf("executed relief-lookup scans over all plans: %d", lookups)
	if lookups == 0 {
		t.Error("no plan executed a relief lookup — the data no longer leaves the edges without a counted row")
	}
}
