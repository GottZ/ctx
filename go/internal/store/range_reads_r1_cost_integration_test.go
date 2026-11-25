//go:build integration

package store_test

// Review round 1 of issue #536 (PR #558): the issue search by update while
// idx_blocks_scope_updated does not stand, and the created list with a
// status filter, measured against the statements of the base (5a28c475) as
// the plan cache really runs them. Before #536 the store sent every one of
// them in the pool's default mode, pgx's statement cache: a named statement
// per connection, planned with its arguments on its first five executions
// and then on the plan the cache chooses (plan_cache_mode auto) — the
// generic plan wherever its estimate undercuts the custom plans' average.
// That choice depends on what the connection ran before, so the base is read
// here after five executions with the same arguments and after five with
// each other word or status (the histories), the sixth counting.
//
//	go test -tags=integration -p 1 ./internal/store/ -run 'TestRangeReadsR1' -count=1 -v
//
// The cost tests need the fixture at the size of the review (60 000 issues
// beside 40 000 other blocks, 21 s to build); they run with CTX_I536_R1_COST=1:
// the two gates, and TestRangeReadsR1Table with every case of the review
// round on both projects and both sorts and the forced forms of the created
// list, as rows "R1ROW {json}" in the log. The tests that always run use a
// fixture a tenth of the size (r1Small).

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// The fixture: a large and a small project and a scope of newer blocks of
// another type. The words: r1Common in every issue of both projects; r1Zebra
// in every 1 000th issue of the large project, every 100th of the small one,
// and every block of r1-other; r1Stripes in r1-other only; r1Seldom in the
// zebra issues and nowhere else. The large project's statuses are graded
// (r1Statuses) so the created list's crossover can be read off; its labels:
// a on every issue, rare on 10 (all done), b on 600 (1 %, all open).
const (
	r1Proj  = "r1-proj"  // the large project, older than r1-other
	r1Small = "r1-small" // the small project, older than everything
	r1Other = "r1-other" // newer blocks of type knowledge

	r1Common  = "common"
	r1Zebra   = "zebra"
	r1Stripes = "stripes"
	r1Seldom  = "seldom"
)

// r1Words are the four words of the search cases.
var r1Words = []string{r1Common, r1Zebra, r1Stripes, r1Seldom}

// r1Statuses are the large project's statuses (every 3 000th issue done,
// every 600th triage, every 200th review, every 60th doing, every 20th
// blocked, the rest open: at 60 000 issues 20, 100, 300, 1 000, 3 000 and
// 55 580).
var r1Statuses = []string{"done", "triage", "review", "doing", "blocked", "open"}

// r1Size is a size of the fixture.
type r1Size struct{ proj, small, other int }

var (
	// r1Review is the size of the review: the prüfer's fixture.
	r1Review = r1Size{proj: 60_000, small: 300, other: 40_000}
	// r1Tenth is the size of the tests that always run.
	r1Tenth = r1Size{proj: 6_000, small: 300, other: 4_000}
)

// r1CostRuns reports whether the cost tests run (CTX_I536_R1_COST).
func r1CostRuns(t *testing.T) {
	t.Helper()
	if os.Getenv("CTX_I536_R1_COST") == "" {
		t.Skip("CTX_I536_R1_COST not set: the fixture of the review takes 21 s to build")
	}
}

// r1Seed fills the store in bulk (INSERT … SELECT; the production triggers
// run, the per-row path would take minutes for 100 000 rows): the large
// project's issues, 100 of one created_at each (one second), updated one
// second apart; the small project's; the other scope's blocks, newest of the
// corpus.
func r1Seed(size r1Size) func(ctx context.Context, pool *pgxpool.Pool) error {
	return func(ctx context.Context, pool *pgxpool.Pool) error {
		for _, st := range []string{
			`INSERT INTO context_blocks (category, title, content, metadata, scope, type_name, workflow_status, created_at, updated_at)
		 SELECT 'issue', 'r1 issue ' || g,
		        'issue body ' || g || ' ` + r1Common + `' || CASE WHEN g % 1000 = 0 THEN ' ` + r1Zebra + ` ` + r1Seldom + `' ELSE '' END,
		        CASE WHEN g % 6000 = 0 THEN '{"labels":["rare","a"]}' WHEN g % 100 = 7 THEN '{"labels":["b","a"]}' ELSE '{"labels":["a"]}' END::jsonb,
		        '` + r1Proj + `', 'issue',
		        CASE WHEN g % 3000 = 0 THEN 'done' WHEN g % 600 = 1 THEN 'triage' WHEN g % 200 = 2 THEN 'review'
		             WHEN g % 60 = 3 THEN 'doing' WHEN g % 20 = 4 THEN 'blocked' ELSE 'open' END,
		        timestamptz '2026-01-01 00:00:00+00' + (g / 100) * interval '1 second',
		        timestamptz '2026-03-01 00:00:00+00' + g * interval '1 second'
		   FROM generate_series(1, ` + strconv.Itoa(size.proj) + `) g`,
			`INSERT INTO context_blocks (category, title, content, metadata, scope, type_name, workflow_status, created_at, updated_at)
		 SELECT 'issue', 'r1 small ' || g,
		        'small body ' || g || ' ` + r1Common + `' || CASE WHEN g % 100 = 0 THEN ' ` + r1Zebra + ` ` + r1Seldom + `' ELSE '' END,
		        '{"labels":["a"]}'::jsonb, '` + r1Small + `', 'issue', CASE WHEN g % 100 = 0 THEN 'done' ELSE 'open' END,
		        timestamptz '2025-12-01 00:00:00+00' + g * interval '1 second',
		        timestamptz '2025-12-01 00:00:00+00' + g * interval '1 second'
		   FROM generate_series(1, ` + strconv.Itoa(size.small) + `) g`,
			`INSERT INTO context_blocks (category, title, content, scope, type_name, created_at, updated_at)
		 SELECT 'learnings', 'r1 other ' || g, 'other body ' || g || ' ` + r1Zebra + ` ` + r1Stripes + `', '` + r1Other + `', 'knowledge',
		        timestamptz '2026-06-01 00:00:00+00' + (g / 100) * interval '1 second',
		        timestamptz '2026-06-01 00:00:00+00' + g * interval '1 second'
		   FROM generate_series(1, ` + strconv.Itoa(size.other) + `) g`,
			`VACUUM (ANALYZE) context_blocks`,
		} {
			if _, err := pool.Exec(ctx, st); err != nil {
				return fmt.Errorf("%w\n%s", err, st)
			}
		}
		return nil
	}
}

// r1Store returns a database of the fixture of size (its own copy); without195 drops
// the two indexes of migration 195, as a database of more than 500 000 rows
// holds them until an operator builds them, and forgets the process's reading
// of them.
func r1Store(t *testing.T, size r1Size, without195 bool) *pgxpool.Pool {
	t.Helper()
	start := time.Now()
	pool := testdb.SetupSeededTestDB(t, fmt.Sprintf("i536-r1-v2-%d-%d-%d", size.proj, size.small, size.other), r1Seed(size))
	t.Logf("fixture database ready after %v", time.Since(start).Round(time.Millisecond))
	if without195 {
		if _, err := pool.Exec(context.Background(), `DROP INDEX idx_blocks_scope_updated, idx_blocks_scope_category_updated`); err != nil {
			t.Fatal(err)
		}
	}
	store.ResetListIndexReadingsForTest()
	return pool
}

// The statements of the base and of the head before this round, byte for
// byte, as the reference of what they read. The search by update and the
// created list kept their text through the head (only the query mode and the
// transaction changed); the search by creation stated its scope as a range in
// the head.
const (
	r1BaseSearchUpdated = rrOldSearchUpdated
	r1BaseSearchCreated = rrOldSearchCreated
	r1HeadSearchUpdated = rrOldSearchUpdated
	r1HeadSearchCreated = `
	SELECT b.id::text, b.scope, b.type_name, b.title, b.workflow_status, b.updated_at, b.created_at
	FROM context_blocks b
	WHERE b.scope >= $1::text AND b.scope <= $1::text
	  AND b.type_name = $2
	  AND b.type_name NOT IN ('checkpoint','system-meta')
	  AND NOT b.is_archived
	  AND ($3::text = '' OR b.workflow_status = $3)
	  AND (b.ts_de @@ plainto_tsquery('german', $4) OR b.ts_en @@ plainto_tsquery('english', $4))
	  AND (b.created_at, b.id) < ($5::timestamptz, $6::uuid)
	  AND ($7::text[] IS NULL OR b.metadata->'labels' ?& $7::text[])
	ORDER BY b.scope, b.created_at DESC, b.id DESC
	LIMIT $8`
	r1BaseCreatedList = `
	SELECT b.id::text, b.scope, b.type_name, b.title, b.workflow_status, b.updated_at, b.created_at
	FROM context_blocks b
	WHERE b.scope = $1::text
	  AND b.type_name = $2
	  AND b.workflow_status IS NOT NULL
	  AND NOT b.is_archived
	  AND ($3::text = '' OR b.workflow_status = $3)
	  AND (b.created_at, b.id) < ($4::timestamptz, $5::uuid)
	  AND ($6::text[] IS NULL OR b.metadata->'labels' ?& $6::text[])
	ORDER BY b.created_at DESC, b.id DESC
	LIMIT $7`
)

// r1SortsOff are the settings of the head's created list (readNoSort).
var r1SortsOff = map[string]string{"enable_sort": "off", "enable_incremental_sort": "off"}

// r1Plan is what one measured execution read and took.
type r1Plan struct {
	Buffers int64   `json:"buf"`
	PlanMS  float64 `json:"plan_ms"`
	ExecMS  float64 `json:"exec_ms"`
	Generic bool    `json:"generic,omitempty"`
	Shape   string  `json:"shape,omitempty"`
}

func (a r1Plan) plus(b r1Plan) r1Plan {
	return r1Plan{Buffers: a.Buffers + b.Buffers, PlanMS: a.PlanMS + b.PlanMS, ExecMS: a.ExecMS + b.ExecMS,
		Generic: a.Generic || b.Generic, Shape: a.Shape + " + " + b.Shape}
}

// r1Shape renders a plan's nodes, an index scan with its index.
func r1Shape(n map[string]any) string {
	s, _ := n["Node Type"].(string)
	if ix, ok := n["Index Name"].(string); ok {
		s += "(" + ix + ")"
	}
	if kids, ok := n["Plans"].([]any); ok && len(kids) > 0 {
		parts := make([]string, 0, len(kids))
		for _, k := range kids {
			if m, ok := k.(map[string]any); ok {
				parts = append(parts, r1Shape(m))
			}
		}
		s += "[" + strings.Join(parts, ",") + "]"
	}
	return s
}

// r1Measure prepares sql on a connection of db with random_page_cost rpc and
// the settings local and measures one execution with args. mode "custom" and
// "generic" force plan_cache_mode and run one unmeasured execution first;
// "auto" leaves plan_cache_mode at auto and runs five executions with hist
// first: the sixth runs the plan pgx's statement cache runs from the sixth
// execution of a statement on one connection.
func r1Measure(p *rrPlanners, db *pgxpool.Pool, rpc string, local map[string]string, mode, sql string, hist, args []any) r1Plan {
	t := p.t
	t.Helper()
	ctx := context.Background()
	conn, err := p.pool(db, rpc, local).Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	cacheMode := map[string]string{"custom": "force_custom_plan", "generic": "force_generic_plan", "auto": "auto"}[mode]
	if cacheMode == "" {
		t.Fatalf("mode %q", mode)
	}
	for _, s := range []string{"SET plan_cache_mode = " + cacheMode, "PREPARE r1_measure AS " + sql} {
		if _, err := conn.Exec(ctx, s, pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatalf("%s: %v\n%s", strings.SplitN(s, " ", 2)[0], err, sql)
		}
	}
	defer func() {
		_, _ = conn.Exec(ctx, "DEALLOCATE r1_measure", pgx.QueryExecModeSimpleProtocol)
		_, _ = conn.Exec(ctx, "RESET plan_cache_mode", pgx.QueryExecModeSimpleProtocol)
	}()
	exec := func(a []any) string {
		lits := make([]string, len(a))
		for i, x := range a {
			lits[i] = rrLiteral(t, x)
		}
		return "EXECUTE r1_measure(" + strings.Join(lits, ", ") + ")"
	}
	before := 1
	if mode == "auto" {
		before = 5
	} else {
		hist = args
	}
	for i := 0; i < before; i++ {
		if _, err := conn.Exec(ctx, exec(hist), pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatalf("execute: %v\n%s", err, sql)
		}
	}
	var raw []byte
	if err := conn.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+exec(args), pgx.QueryExecModeSimpleProtocol).Scan(&raw); err != nil {
		t.Fatalf("explain: %v\n%s", err, sql)
	}
	var doc []map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc) != 1 {
		t.Fatalf("explain json: %v", err)
	}
	plan, _ := doc[0]["Plan"].(map[string]any)
	hit, _ := plan["Shared Hit Blocks"].(float64)
	read, _ := plan["Shared Read Blocks"].(float64)
	planMS, _ := doc[0]["Planning Time"].(float64)
	execMS, _ := doc[0]["Execution Time"].(float64)
	var generic int64
	if err := conn.QueryRow(ctx, `SELECT generic_plans FROM pg_prepared_statements WHERE name = 'r1_measure'`, pgx.QueryExecModeSimpleProtocol).Scan(&generic); err != nil {
		t.Fatal(err)
	}
	return r1Plan{Buffers: int64(hit + read), PlanMS: planMS, ExecMS: execMS, Generic: generic > 0, Shape: r1Shape(plan)}
}

// r1Sent measures the statements a store call sent as it sent them: one sent
// with a query mode that plans it on every execution (CustomPlanMode) on its
// custom plan, one sent in the pool's default mode under the plan cache's own
// choice after each history (hist rewrites the arguments for one history).
// It returns one total per history.
func r1Sent(p *rrPlanners, db *pgxpool.Pool, rpc string, sent []rrStmt, hists int, hist func(i int, args []any) []any) []r1Plan {
	out := make([]r1Plan, hists)
	for _, st := range sent {
		mode := st.mode
		if mode == pgx.QueryExecModeExec && st.local["plan_cache_mode"] == "force_generic_plan" {
			// An unnamed statement under force_generic_plan (FullGraph's merge
			// seed): the generic plan on every execution, which the measure
			// reads as a prepared statement's under the same setting.
			mode = pgx.QueryExecModeCacheStatement
		}
		switch mode {
		case pgx.QueryExecModeCacheDescribe, pgx.QueryExecModeDescribeExec, pgx.QueryExecModeExec:
			c := r1Measure(p, db, rpc, st.local, "custom", st.sql, nil, st.args)
			for i := range out {
				out[i] = out[i].plus(c)
			}
		case 0, pgx.QueryExecModeCacheStatement:
			for i := range out {
				out[i] = out[i].plus(r1Measure(p, db, rpc, st.local, "auto", st.sql, hist(i, st.args), st.args))
			}
		default:
			p.t.Fatalf("a statement sent in query mode %v", st.mode)
		}
	}
	for i := range out {
		out[i].Shape = strings.TrimPrefix(out[i].Shape, " + ")
	}
	return out
}

// r1All returns every traced statement match admits, each with the SET LOCAL
// settings of its transaction and its query mode (rrFind's reading).
func r1All(t *testing.T, traced []reviewStmt, match func(sql string) bool) []rrStmt {
	t.Helper()
	var out []rrStmt
	var local map[string]string
	for _, st := range traced {
		low := strings.ToLower(st.sql)
		switch {
		case strings.HasPrefix(low, "begin") || low == "commit" || low == "rollback":
			local = nil
			continue
		case strings.HasPrefix(st.sql, "SET LOCAL "):
			m := setLocals(t, st.sql)
			if local == nil {
				local = map[string]string{}
			}
			for k, v := range m {
				local[k] = v
			}
			continue
		case !match(st.sql):
			continue
		}
		s := rrStmt{sql: st.sql, local: local}
		for _, a := range st.args {
			if m, ok := a.(pgx.QueryExecMode); ok {
				s.mode = m
				continue
			}
			s.args = append(s.args, a)
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		t.Fatal("no statement was traced")
	}
	return out
}

// r1Bench is the measuring kit of a test: the planners' pools, and one
// traced pool per database (a pool per case would hold the server's
// connections until the test ends).
type r1Bench struct {
	p      *rrPlanners
	traced map[*pgxpool.Pool]*r1TracedPool
}

type r1TracedPool struct {
	pool *pgxpool.Pool
	tr   *reviewTracer
}

func newR1Bench(t *testing.T) *r1Bench {
	return &r1Bench{p: &rrPlanners{t: t}, traced: map[*pgxpool.Pool]*r1TracedPool{}}
}

func (b *r1Bench) tracedPool(db *pgxpool.Pool) (*pgxpool.Pool, *reviewTracer) {
	if b.traced[db] == nil {
		pool, tr := rrTraced(b.p.t, db)
		b.traced[db] = &r1TracedPool{pool, tr}
	}
	return b.traced[db].pool, b.traced[db].tr
}

// r1Slack is the noise a re-read of one plan carries (hint bits, a page of the
// visibility map): a tenth, at least 8 buffers.
func r1Slack(b int64) int64 { return max(8, b/10) }

// r1Inf and the first page's keyset.
var r1Inf = pgtype.Timestamptz{InfinityModifier: pgtype.Infinity, Valid: true}

const r1MaxUUID = "ffffffff-ffff-ffff-ffff-ffffffffffff"

// r1Row is one measured case, logged as "R1ROW {json}".
type r1Row struct {
	Image   string            `json:"image"`
	Point   string            `json:"point"`
	Index   string            `json:"index195,omitempty"`
	RPC     string            `json:"rpc"`
	Case    string            `json:"case"`
	Base    map[string]r1Plan `json:"base"`
	Head    map[string]r1Plan `json:"head"`
	New     map[string]r1Plan `json:"new"`
	NewMode string            `json:"new_mode"`
	Forced  map[string]r1Plan `json:"forced,omitempty"`
	Verdict string            `json:"verdict"`
}

func r1Image() string {
	if v := os.Getenv("CTX_TEST_PG_IMAGE"); v != "" {
		return v
	}
	return "ci"
}

func (r r1Row) log(t *testing.T) {
	t.Helper()
	b, _ := json.Marshal(r)
	t.Logf("R1ROW %s", b)
}

func r1ModeName(sent []rrStmt) string {
	var parts []string
	for _, st := range sent {
		m := "default"
		switch st.mode {
		case pgx.QueryExecModeCacheDescribe:
			m = "cache-describe"
		case pgx.QueryExecModeDescribeExec:
			m = "describe-exec"
		case pgx.QueryExecModeExec:
			m = "exec"
		}
		if len(st.local) > 0 {
			keys := make([]string, 0, len(st.local))
			for k, v := range st.local {
				keys = append(keys, k+"="+v)
			}
			m += " " + strings.Join(keys, ",")
		}
		parts = append(parts, m)
	}
	return strings.Join(parts, " + ")
}

// r1SearchCase measures one search case: the base's statement under the plan
// cache's choice after each word's history (hists) and, with full, on its
// custom and its generic plan; with full the head's on its custom plan (the
// head sent it with QueryExecModeCacheDescribe); and what SearchIssues sends
// now, as sent. It reports whether the search now reads more than the base
// after any history.
func r1SearchCase(t *testing.T, b *r1Bench, db *pgxpool.Pool, rpc, index, scope, word, sort string, hists []string, full bool) (r1Row, bool) {
	t.Helper()
	p := b.p
	ctx := context.Background()
	args := []any{scope, store.IssueTypeName, "", word, r1Inf, r1MaxUUID, nil, 50}
	withWord := func(a []any, w string) []any { h := append([]any(nil), a...); h[3] = w; return h }
	baseSQL, headSQL := r1BaseSearchUpdated, r1HeadSearchUpdated
	if sort == store.SortCreated {
		baseSQL, headSQL = r1BaseSearchCreated, r1HeadSearchCreated
	}
	row := r1Row{Image: r1Image(), Point: "M2", Index: index, RPC: rpc, Case: fmt.Sprintf("%s %s word=%s", scope, sort, word),
		Base: map[string]r1Plan{}, Head: map[string]r1Plan{}, New: map[string]r1Plan{}}
	if full {
		row.Base["custom"] = r1Measure(p, db, rpc, nil, "custom", baseSQL, nil, args)
		row.Base["generic"] = r1Measure(p, db, rpc, nil, "generic", baseSQL, nil, args)
		row.Head["custom"] = r1Measure(p, db, rpc, nil, "custom", headSQL, nil, args)
	}
	for _, h := range hists {
		row.Base["auto/"+h] = r1Measure(p, db, rpc, nil, "auto", baseSQL, withWord(args, h), args)
	}

	traced, tr := b.tracedPool(db)
	tr.take()
	if _, _, err := store.SearchIssues(ctx, traced, store.IssueReadQuery{Scope: scope, Q: word, Sort: sort, Limit: 50}); err != nil {
		t.Fatalf("%s: %v", row.Case, err)
	}
	sent := r1All(t, tr.take(), func(sql string) bool { return strings.Contains(sql, "plainto_tsquery") })
	row.NewMode = r1ModeName(sent)
	news := r1Sent(p, db, rpc, sent, len(hists), func(i int, a []any) []any { return withWord(a, hists[i]) })
	worse := false
	var why []string
	for i, h := range hists {
		row.New["auto/"+h] = news[i]
		base := row.Base["auto/"+h]
		if news[i].Buffers > base.Buffers+r1Slack(base.Buffers) {
			worse = true
			why = append(why, fmt.Sprintf("after %s: %d > base %d", h, news[i].Buffers, base.Buffers))
		}
	}
	row.Verdict = "ok"
	if worse {
		row.Verdict = "WORSE " + strings.Join(why, "; ")
	}
	return row, worse
}

// r1CreatedCase measures one created-list case: the base's statement and the
// head's (sorts off, the pool's default mode) under the plan cache's choice
// after each status's history (hists) and, with full, on their custom and
// generic plans; and what ListIssuesByCreated sends now, as sent. It reports
// whether the list now reads more than factor times the better of the base
// and the head after any history.
func r1CreatedCase(t *testing.T, b *r1Bench, db *pgxpool.Pool, rpc, status string, labels []string, limit int, hists []string, factor int64, full bool) (r1Row, bool) {
	t.Helper()
	p := b.p
	ctx := context.Background()
	args := []any{r1Proj, store.IssueTypeName, status, r1Inf, r1MaxUUID, labels, limit}
	withStatus := func(a []any, s string) []any { h := append([]any(nil), a...); h[2] = s; return h }
	row := r1Row{Image: r1Image(), Point: "L1", RPC: rpc, Case: fmt.Sprintf("status=%q labels=%v limit=%d", status, labels, limit),
		Base: map[string]r1Plan{}, Head: map[string]r1Plan{}, New: map[string]r1Plan{}}
	if full {
		row.Base["custom"] = r1Measure(p, db, rpc, nil, "custom", r1BaseCreatedList, nil, args)
		row.Base["generic"] = r1Measure(p, db, rpc, nil, "generic", r1BaseCreatedList, nil, args)
		row.Head["custom"] = r1Measure(p, db, rpc, r1SortsOff, "custom", r1BaseCreatedList, nil, args)
		row.Head["generic"] = r1Measure(p, db, rpc, r1SortsOff, "generic", r1BaseCreatedList, nil, args)
	}
	for _, h := range hists {
		row.Base["auto/"+h] = r1Measure(p, db, rpc, nil, "auto", r1BaseCreatedList, withStatus(args, h), args)
		row.Head["auto/"+h] = r1Measure(p, db, rpc, r1SortsOff, "auto", r1BaseCreatedList, withStatus(args, h), args)
	}

	traced, tr := b.tracedPool(db)
	tr.take()
	if _, _, err := store.ListIssuesByCreated(ctx, traced, store.IssueReadQuery{Scope: r1Proj, Status: status, Labels: labels, Limit: limit}); err != nil {
		t.Fatalf("%s: %v", row.Case, err)
	}
	sent := r1All(t, tr.take(), func(sql string) bool { return strings.Contains(sql, "context_blocks") })
	row.NewMode = r1ModeName(sent)
	news := r1Sent(p, db, rpc, sent, len(hists), func(i int, a []any) []any {
		// The page's status is its third argument, the count's as well.
		return withStatus(a, hists[i])
	})
	worse := false
	var why []string
	for i, h := range hists {
		row.New["auto/"+h] = news[i]
		better := min(row.Base["auto/"+h].Buffers, row.Head["auto/"+h].Buffers)
		if news[i].Buffers > factor*better+r1Slack(better)+r1CountPages(status, labels, limit) {
			worse = true
			why = append(why, fmt.Sprintf("after %q: %d > %d × %d", h, news[i].Buffers, factor, better))
		}
	}
	row.Verdict = "ok"
	if worse {
		row.Verdict = "WORSE " + strings.Join(why, "; ")
	}
	return row, worse
}

// r1OtherWord is the history of the other kind of word: the common word
// for a rare or an absent one, a word rare in the project for the common one.
func r1OtherWord(w string) string {
	if w == r1Common {
		return r1Zebra
	}
	return r1Common
}

// TestRangeReadsR1IssueSearchByUpdate (review round 1 of #536, M2): the
// search by update reads no more than the base did as the plan cache ran it,
// after a history of the same word and of the other kind of word, with and
// without idx_blocks_scope_updated, in the large project. Without the index
// every custom plan of a word that is rare in the project and common
// elsewhere read the project's issues whole and sorted them (3 597 buffers at
// 60 000 issues), where the base's cached statement switched to the generic
// plan after five executions — a BitmapAnd of the word and the scope, 70–179.
func TestRangeReadsR1IssueSearchByUpdate(t *testing.T) {
	r1CostRuns(t)
	b := newR1Bench(t)
	for _, without := range []bool{false, true} {
		index := "with"
		if without {
			index = "without"
		}
		db := r1Store(t, r1Review, without)
		for _, rpc := range testdb.RandomPageCosts {
			for _, w := range r1Words {
				row, worse := r1SearchCase(t, b, db, rpc, index, r1Proj, w, store.SortUpdated, []string{w, r1OtherWord(w)}, false)
				row.log(t)
				// With the index the statement is planned on every execution,
				// without it the pool's statement cache keeps it, as before.
				if want := map[bool]string{false: "cache-describe", true: "default"}[without]; row.NewMode != want {
					t.Errorf("search by update, %s idx_blocks_scope_updated: sent in mode %q, want %q", index, row.NewMode, want)
				}
				if worse {
					t.Errorf("search by update, %s idx_blocks_scope_updated, random_page_cost %s, word %s: %s", index, rpc, w, row.Verdict)
				}
			}
		}
	}
}

// r1CountPages is what the created list's capped count of a status may read
// beside the page: the status's index entries up to the count (about 130 a
// leaf page of idx_blocks_workflow_board, rounded up to 100) and a descent.
// The count is the price of the choice between the walk and the bitmap; for
// a status most issues hold it is most of what the page costs (7 → 17
// buffers at a page of 50, 30 with a label filter).
func r1CountPages(status string, labels []string, limit int) int64 {
	if status == "" {
		return 0
	}
	few := int64(64 * math.Sqrt(float64(limit)))
	if len(labels) > 0 {
		few *= 3
	}
	return few/100 + 4
}

// r1OtherStatus is the history of another status: no status for a status,
// the rare status for none.
func r1OtherStatus(s string) string {
	if s == "" {
		return "done"
	}
	return ""
}

// TestRangeReadsR1CreatedListByStatus (review round 1 of #536, L1): the
// created list reads no more than twice the better of the base as the plan
// cache ran it and the head (sorts off), after a history of the same status
// and of another, for a rare, a middle and a common status and without a
// status, without a label filter and with one that 1 % of the issues carry
// (none of the middle status's), at a page of 50, the count beside the page
// allowed (r1CountPages). The head walked idx_blocks_workflow_created for
// every status: for 20 issues of a status in 60 000 it read the project's
// whole range (4 304 buffers), where the base's custom plan read the
// status's range of idx_blocks_workflow_board and sorted it (23). Two cases
// hold a declared bound instead (createdListFewIssues): a label every issue
// carries on the middle status at a page of 50 (the bitmap, 4.7 times the
// walk) and the 1 % label on it at a page of 5 (the walk, 4.2 times the
// bitmap).
func TestRangeReadsR1CreatedListByStatus(t *testing.T) {
	r1CostRuns(t)
	b := newR1Bench(t)
	db := r1Store(t, r1Review, false)
	type lage struct {
		status string
		labels []string
		limit  int
		factor int64
	}
	var lagen []lage
	for _, status := range []string{"done", "doing", "open", ""} {
		for _, labels := range [][]string{nil, {"b"}} {
			lagen = append(lagen, lage{status, labels, 50, 2})
		}
	}
	lagen = append(lagen, lage{"open", []string{"a"}, 50, 2}, lage{"doing", []string{"a"}, 50, 5}, lage{"doing", []string{"b"}, 5, 5})
	for _, rpc := range testdb.RandomPageCosts {
		for _, l := range lagen {
			row, worse := r1CreatedCase(t, b, db, rpc, l.status, l.labels, l.limit, []string{l.status, r1OtherStatus(l.status)}, l.factor, false)
			row.log(t)
			if worse {
				t.Errorf("created list, random_page_cost %s, %s: %s", rpc, row.Case, row.Verdict)
			}
		}
	}
}

// TestRangeReadsR1Table (CTX_I536_R1_COST=1): the whole table of the review
// round — the search on both projects, both sorts, all four words, with and
// without the index, after the history of each word; the created list for
// every graded status with no label and the labels rare, b and a, at limits
// 50, 5 and 100, after the history of each status, plus the two forms the
// list chooses between, forced: the walk with sorts off and the status's
// bitmap with index scans off. No assertion beyond the two tests above; the
// rows are the record.
func TestRangeReadsR1Table(t *testing.T) {
	r1CostRuns(t)
	b := newR1Bench(t)
	p := b.p
	for _, without := range []bool{false, true} {
		index := "with"
		if without {
			index = "without"
		}
		db := r1Store(t, r1Review, without)
		for _, rpc := range testdb.RandomPageCosts {
			for _, scope := range []string{r1Proj, r1Small} {
				for _, sort := range []string{store.SortUpdated, store.SortCreated} {
					for _, w := range r1Words {
						row, _ := r1SearchCase(t, b, db, rpc, index, scope, w, sort, r1Words, true)
						row.log(t)
					}
				}
			}
		}
	}
	db := r1Store(t, r1Review, false)
	statuses := append([]string{""}, r1Statuses...)
	bitmap := map[string]string{"enable_indexscan": "off"}
	for _, rpc := range testdb.RandomPageCosts {
		for _, limit := range []int{50, 5, 100} {
			for _, status := range statuses {
				for _, labels := range [][]string{nil, {"rare"}, {"b"}, {"a"}} {
					if limit != 50 && len(labels) > 0 && labels[0] != "b" {
						continue
					}
					row, _ := r1CreatedCase(t, b, db, rpc, status, labels, limit, statuses, 2, true)
					args := []any{r1Proj, store.IssueTypeName, status, r1Inf, r1MaxUUID, labels, limit}
					row.Forced = map[string]r1Plan{
						"walk/custom":    r1Measure(p, db, rpc, r1SortsOff, "custom", r1BaseCreatedList, nil, args),
						"bitmap/custom":  r1Measure(p, db, rpc, bitmap, "custom", r1BaseCreatedList, nil, args),
						"bitmap/generic": r1Measure(p, db, rpc, bitmap, "generic", r1BaseCreatedList, nil, args),
					}
					row.log(t)
				}
			}
		}
	}
}
