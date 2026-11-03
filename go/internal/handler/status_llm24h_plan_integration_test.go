//go:build integration

// W06-5 gate (d): both llm_24h readers read at most one hour of raw
// context_llm_log rows — the window's edge, bounded on both sides — and the
// rest of the 24 hours from the hourly rollup through idx_llm_usage_bucket
// (the tenant view may also probe idx_llm_usage_key), never by a sequential
// scan. Before W06-5 both were a 24-hour scan over the hypertable with a
// lower created_at bound only; the test runs its check on those queries too
// (the reference SQL) and requires it to fail there, so the check cannot
// pass vacuously.
//
// Every statement the readers issue is recorded with its arguments and run as
// EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON), under the custom plan of a
// one-shot statement and under the generic plan a cached prepared statement
// of the driver can switch to.
//
//	go test -tags=integration -p 1 ./internal/handler/ -run TestStatusLLM24hPlan -count=1 -v
//
// CTX_W065_PLAN_DIR=<dir> writes every plan as JSON into dir.
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/llmusage"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// llmPlanStmt is one statement a reader sent, with its arguments.
type llmPlanStmt struct {
	sql  string
	args []any
}

// llmPlanRecorder passes every query through to the pool and records it.
type llmPlanRecorder struct {
	pool  *pgxpool.Pool
	stmts []llmPlanStmt
}

func (r *llmPlanRecorder) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.stmts = append(r.stmts, llmPlanStmt{sql: sql, args: append([]any(nil), args...)})
	return r.pool.Query(ctx, sql, args...)
}

// llmPlanRead is what one plan reads from the raw log and how it reaches the
// rollup.
type llmPlanRead struct {
	rawRows      int64    // raw rows the scans touched: returned + removed by a filter or a recheck
	unbounded    []string // raw scan conditions without a created_at bound on both sides
	rollupSeq    bool     // the rollup was read by a sequential scan
	rollupIdx    []string // index of every rollup scan ("" for none)
	rollupAccess []string // node type, index and condition of every rollup scan
	execMs       float64
	sharedBlocks float64
}

func llmPlanNum(n map[string]any, k string) float64 {
	v, _ := n[k].(float64)
	return v
}

func llmPlanStr(n map[string]any, k string) string {
	v, _ := n[k].(string)
	return v
}

// llmPlanIndex names the index a scan node reads: its own, or for a bitmap
// heap scan the one of its bitmap index scan (a BitmapOr names them all).
func llmPlanIndex(n map[string]any) string {
	if idx := llmPlanStr(n, "Index Name"); idx != "" {
		return idx
	}
	var names []string
	if kids, ok := n["Plans"].([]any); ok {
		for _, k := range kids {
			if m, ok := k.(map[string]any); ok {
				if idx := llmPlanIndex(m); idx != "" {
					names = append(names, idx)
				}
			}
		}
	}
	return strings.Join(names, "+")
}

func llmPlanWalk(n map[string]any, out *llmPlanRead) {
	rel := llmPlanStr(n, "Relation Name")
	switch llmPlanStr(n, "Node Type") {
	case "Seq Scan", "Index Scan", "Index Only Scan", "Bitmap Heap Scan":
		cond := strings.TrimSpace(llmPlanStr(n, "Index Cond") + " " + llmPlanStr(n, "Recheck Cond") + " " + llmPlanStr(n, "Filter"))
		switch {
		case rel == "context_llm_log" || strings.HasPrefix(rel, "_hyper_"):
			out.rawRows += int64(llmPlanNum(n, "Actual Rows")*llmPlanNum(n, "Actual Loops") +
				llmPlanNum(n, "Rows Removed by Filter") + llmPlanNum(n, "Rows Removed by Index Recheck"))
			if !strings.Contains(cond, "created_at >") || !strings.Contains(cond, "created_at <") {
				out.unbounded = append(out.unbounded, rel+": "+cond)
			}
		case rel == "context_llm_usage_hourly":
			idx := llmPlanIndex(n)
			out.rollupSeq = out.rollupSeq || llmPlanStr(n, "Node Type") == "Seq Scan"
			out.rollupIdx = append(out.rollupIdx, idx)
			out.rollupAccess = append(out.rollupAccess, strings.TrimSpace(llmPlanStr(n, "Node Type")+" "+idx+" "+cond))
		}
	}
	if kids, ok := n["Plans"].([]any); ok {
		for _, k := range kids {
			if m, ok := k.(map[string]any); ok {
				llmPlanWalk(m, out)
			}
		}
	}
}

func llmPlanParse(t *testing.T, name string, raw []byte) llmPlanRead {
	t.Helper()
	if dir := os.Getenv("CTX_W065_PLAN_DIR"); dir != "" {
		if err := os.WriteFile(filepath.Join(dir, name+".json"), raw, 0o600); err != nil {
			t.Fatalf("dump plan: %v", err)
		}
	}
	var doc []map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc) != 1 {
		t.Fatalf("explain json: %v (%d documents)", err, len(doc))
	}
	plan, _ := doc[0]["Plan"].(map[string]any)
	out := llmPlanRead{execMs: llmPlanNum(doc[0], "Execution Time")}
	llmPlanWalk(plan, &out)
	out.sharedBlocks = llmPlanNum(plan, "Shared Hit Blocks") + llmPlanNum(plan, "Shared Read Blocks")
	return out
}

// llmPlanCustom plans the statement once with its arguments bound.
func llmPlanCustom(t *testing.T, pool *pgxpool.Pool, name string, s llmPlanStmt) llmPlanRead {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+s.sql, s.args...).Scan(&raw); err != nil {
		t.Fatalf("explain: %v\n%s", err, s.sql)
	}
	return llmPlanParse(t, name, raw)
}

// llmPlanGeneric plans the statement as a prepared statement under
// plan_cache_mode = force_generic_plan: the plan a cached statement of the
// driver can switch to after five executions. Every argument is a
// timestamptz.
func llmPlanGeneric(t *testing.T, pool *pgxpool.Pool, name string, s llmPlanStmt) llmPlanRead {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	lits, types := make([]string, len(s.args)), make([]string, len(s.args))
	for i, a := range s.args {
		ts, ok := a.(time.Time)
		if !ok {
			t.Fatalf("argument %d is %T, not a time", i+1, a)
		}
		lits[i], types[i] = "'"+ts.UTC().Format("2006-01-02 15:04:05.999999")+"+00'", "timestamptz"
	}
	for _, st := range []string{
		"SET plan_cache_mode = force_generic_plan",
		fmt.Sprintf("PREPARE w065_plan(%s) AS %s", strings.Join(types, ", "), s.sql),
	} {
		if _, err := conn.Exec(ctx, st); err != nil {
			t.Fatalf("%s: %v", strings.SplitN(st, "(", 2)[0], err)
		}
	}
	defer func() {
		_, _ = conn.Exec(ctx, "DEALLOCATE w065_plan")
		_, _ = conn.Exec(ctx, "RESET plan_cache_mode")
	}()
	var raw []byte
	if err := conn.QueryRow(ctx, fmt.Sprintf("EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE w065_plan(%s)",
		strings.Join(lits, ", "))).Scan(&raw); err != nil {
		t.Fatalf("explain execute: %v", err)
	}
	return llmPlanParse(t, name, raw)
}

// llmPlanViolations checks one plan against the gate: raw scans bounded on
// both sides and no more raw rows than the edge holds; the rollup read
// through one of the allowed indexes, never sequentially.
func llmPlanViolations(p llmPlanRead, edgeRows int64, allowed ...string) []string {
	var v []string
	if len(p.unbounded) > 0 {
		v = append(v, fmt.Sprintf("raw scan without a two-sided created_at bound: %q", p.unbounded))
	}
	if p.rawRows > edgeRows {
		v = append(v, fmt.Sprintf("touched %d raw rows, the edge hour holds %d", p.rawRows, edgeRows))
	}
	if p.rollupSeq {
		v = append(v, fmt.Sprintf("the rollup is read by a sequential scan: %q", p.rollupAccess))
	}
	if len(p.rollupIdx) == 0 {
		v = append(v, "the rollup is not read")
	}
	for _, idx := range p.rollupIdx {
		ok := false
		for _, a := range allowed {
			ok = ok || idx == a
		}
		if !ok {
			v = append(v, fmt.Sprintf("the rollup is read through %q, not %q: %q", idx, allowed, p.rollupAccess))
		}
	}
	return v
}

func TestStatusLLM24hPlan(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	anchor := llm24hAnchors[0]
	since, edgeEnd := llmusage.Bounds(anchor, llm24hSpan)

	// 40 tenants with one key each; every hour of 31 days, in time order (the
	// heap mixes tenants as live): one row per key on four pipelines and two
	// backends, two consumer rows and two background rows — 44 rows an hour.
	const tenants = 40
	if _, err := pool.Exec(ctx, `
		WITH t AS (
		    INSERT INTO context_tenants (slug, display_name)
		    SELECT 'w065p-' || g, 'w065p-' || g FROM generate_series(1, $1::int) g
		    RETURNING id, slug
		), s AS (
		    INSERT INTO context_tenant_scopes (scope, tenant_id) SELECT slug, id FROM t RETURNING 1
		), p AS (
		    INSERT INTO context_principals (display_name) SELECT slug FROM t RETURNING id, display_name
		)
		INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id, principal_id)
		SELECT 'sk-' || t.slug, t.slug, t.slug, t.id, p.id FROM t JOIN p ON p.display_name = t.slug`, tenants); err != nil {
		t.Fatalf("tenants: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_llm_log (created_at, pipeline, model, host, backend_name, duration_ms, cost_usd, api_key_id, consumer_id)
		SELECT h + make_interval(secs => n * 60 + 7),
		       (ARRAY['dream-eval','query-synthesize','web-chat','query-embed'])[1 + n % 4], 'm', 'spark',
		       (ARRAY['spark-chat','spark-embed'])[1 + n % 2], 100 + n, 0.001,
		       CASE WHEN n < $3::int THEN k.ids[n + 1] END,
		       CASE WHEN n IN ($3::int, $3::int + 1) THEN '0196f0a0-0000-7000-8000-0000000065c1'::uuid END
		  FROM generate_series($1::timestamptz, $2::timestamptz, interval '1 hour') h
		 CROSS JOIN generate_series(0, $3::int + 3) n
		 CROSS JOIN (SELECT array_agg(id ORDER BY label) AS ids FROM context_api_keys WHERE label LIKE 'w065p-%') k
		 ORDER BY 1`,
		anchor.Add(-31*24*time.Hour).Truncate(time.Hour), anchor, tenants); err != nil {
		t.Fatalf("log rows: %v", err)
	}
	if _, err := pool.Exec(ctx, "ANALYZE context_llm_log; ANALYZE context_llm_usage_hourly; ANALYZE context_api_keys; ANALYZE context_tenant_scopes"); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	var edgeRows, dayRows, logRows, rollRows int64
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM context_llm_log WHERE created_at > $1 AND created_at < $2),
		       (SELECT count(*) FROM context_llm_log WHERE created_at > $1),
		       (SELECT count(*) FROM context_llm_log),
		       (SELECT count(*) FROM context_llm_usage_hourly)`, since, edgeEnd).Scan(&edgeRows, &dayRows, &logRows, &rollRows); err != nil {
		t.Fatal(err)
	}
	t.Logf("log %d rows, rollup %d rows; window (%s, …): %d raw rows, edge until %s: %d rows",
		logRows, rollRows, since.Format(time.RFC3339Nano), dayRows, edgeEnd.Format(time.RFC3339), edgeRows)

	rec := &llmPlanRecorder{pool: pool}
	if _, err := llmusage.Groups(ctx, rec, anchor, llm24hSpan); err != nil {
		t.Fatal(err)
	}
	if _, err := llmusage.TenantGroups(ctx, rec, anchor, llm24hSpan); err != nil {
		t.Fatal(err)
	}
	if len(rec.stmts) != 2 {
		t.Fatalf("the readers sent %d statements, want 2", len(rec.stmts))
	}
	readers := []struct {
		name    string
		stmt    llmPlanStmt
		allowed []string
	}{
		{"server", rec.stmts[0], []string{"idx_llm_usage_bucket"}},
		{"tenant", rec.stmts[1], []string{"idx_llm_usage_bucket", "idx_llm_usage_key"}},
	}
	for _, r := range readers {
		for _, mode := range []string{"custom", "generic"} {
			var p llmPlanRead
			if mode == "custom" {
				p = llmPlanCustom(t, pool, r.name+"-"+mode, r.stmt)
			} else {
				p = llmPlanGeneric(t, pool, r.name+"-"+mode, r.stmt)
			}
			t.Logf("%s (%s plan): raw rows touched %d (edge %d, 24 h %d), rollup %q, %.0f shared blocks, %.3f ms",
				r.name, mode, p.rawRows, edgeRows, dayRows, p.rollupAccess, p.sharedBlocks, p.execMs)
			for _, v := range llmPlanViolations(p, edgeRows, r.allowed...) {
				t.Errorf("%s (%s plan): %s", r.name, mode, v)
			}
		}
	}

	// The control: the raw-row queries of before W06-5 fail the same check.
	for _, r := range []struct {
		name string
		sql  string
	}{{"reference-server", refLLM24hSQL}, {"reference-tenant", refTenantLLM24hSQL}} {
		p := llmPlanCustom(t, pool, r.name, llmPlanStmt{sql: r.sql, args: []any{anchor}})
		v := llmPlanViolations(p, edgeRows, "idx_llm_usage_bucket", "idx_llm_usage_key")
		t.Logf("%s (custom plan): raw rows touched %d, %.0f shared blocks, %.3f ms; violations %q", r.name, p.rawRows, p.sharedBlocks, p.execMs, v)
		if len(v) == 0 {
			t.Errorf("%s: the raw-row query passes the check — the check is vacuous", r.name)
		}
	}
}
