//go:build integration

// The measurement behind issue #238 §1 (not a gate; it skips unless
// CTX_GUARD_FLAGTIME_MEASURE is set): EXPLAIN (ANALYZE, BUFFERS) of the
// guard-review readers — the /api/status rollup (guardGenRollupSQL) and the
// daily report's single-scope read — in the forms named below, on three
// fixtures, at random_page_cost 1.1 and 4, custom and generic plan, median of
// seven runs after one warm-up. Every line that carries a number starts with
// MEASURE|. The forms are compared for their answer first: a form that
// answers differently from the base on a fixture fails the run. Forms: pre218
// (min(updated_at), no metadata read — the reader before #218), base (the
// expression as of 749ee2a0), once, lateral (the issue's LATERAL … OFFSET 0)
// and head (guardGenRollupSQL and the daily report's statement over
// guard.FlaggedSQL).
//
//	CTX_GUARD_FLAGTIME_MEASURE=1 go test -tags=integration -p 1 ./internal/handler/ \
//	    -run TestGuardFlagTimeMeasure -count=1 -v -timeout 60m
//
// CTX_GUARD_FLAGTIME_FIXTURES picks fixtures by name (comma-separated).
//
// The fixture rows are written with one INSERT … SELECT each, carrying the
// metadata document the guard writes (applyDecision's keys) plus caller keys
// to the stated size: a measurement of a reader at 10^5–10^6 rows cannot go
// through the guard batch row by row. The pins in status_guardage_integration_test.go
// write their rows through the production path.
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/testdb"
)

// mfBaseFlaggedAt is guard.FlaggedAtSQL as of 749ee2a0, the base of #238.
const mfBaseFlaggedAt = `CASE WHEN pg_input_is_valid(metadata->>'guard_checked_at', 'timestamptz')
		THEN CASE WHEN isfinite((metadata->>'guard_checked_at')::timestamptz)
				AND (metadata->>'guard_checked_at')::timestamptz >= date_trunc('second', created_at)
				AND (metadata->>'guard_checked_at')::timestamptz <= now()
			THEN (metadata->>'guard_checked_at')::timestamptz
			ELSE updated_at END
		ELSE updated_at END`

const mfFlaggedWhere = `NOT is_archived
	  AND guard_status IN ('needs_review', 'near_duplicate', 'possible_duplicate')`

const mfCounts = `(count(*) FILTER (WHERE guard_status = 'needs_review'))::int,
		(count(*) FILTER (WHERE guard_status = 'near_duplicate'))::int,
		(count(*) FILTER (WHERE guard_status = 'possible_duplicate'))::int`

// mfOuterOnce is the flag time over the column v (the stamp text, read once).
const mfOuterOnce = `CASE WHEN pg_input_is_valid(v, 'timestamptz')
		THEN CASE WHEN v::timestamptz >= date_trunc('second', created_at) AND v::timestamptz <= now()
			THEN v::timestamptz ELSE updated_at END
		ELSE updated_at END`

// mfForm is one form of a reader: its rollup statement and its single-scope
// statement ($1 = scope).
type mfForm struct {
	name, rollup, daily string
}

func mfForms() []mfForm {
	// pre218: the reader before #218, which did not read metadata at all.
	pre := mfForm{name: "pre218",
		rollup: `SELECT scope, GROUPING(scope) AS is_total, ` + mfCounts + `, min(updated_at)
	FROM context_blocks WHERE ` + mfFlaggedWhere + ` GROUP BY ROLLUP (scope)`,
		daily: `SELECT ` + mfCounts + `, min(updated_at) FROM context_blocks WHERE scope = $1 AND ` + mfFlaggedWhere,
	}
	base := mfForm{name: "base",
		rollup: `SELECT scope, GROUPING(scope) AS is_total, ` + mfCounts + `, min(` + mfBaseFlaggedAt + `)
	FROM context_blocks WHERE ` + mfFlaggedWhere + ` GROUP BY ROLLUP (scope)`,
		daily: `SELECT ` + mfCounts + `, min(` + mfBaseFlaggedAt + `) FROM context_blocks WHERE scope = $1 AND ` + mfFlaggedWhere,
	}
	// once: the key read once per row in a fenced derived table (OFFSET 0),
	// the validity test and the three casts on that text in the outer query.
	// head (guard.FlaggedSQL) fences the cast as well: one validity test and
	// one cast per row.
	inner := func(cond string) string {
		return `(SELECT scope, guard_status, created_at, updated_at, metadata->>'guard_checked_at' AS v
		   FROM context_blocks WHERE ` + cond + mfFlaggedWhere + ` OFFSET 0) f`
	}
	once := mfForm{name: "once",
		rollup: `SELECT scope, GROUPING(scope) AS is_total, ` + mfCounts + `, min(` + mfOuterOnce + `)
	FROM ` + inner("") + ` GROUP BY ROLLUP (scope)`,
		daily: `SELECT ` + mfCounts + `, min(` + mfOuterOnce + `) FROM ` + inner("scope = $1 AND "),
	}
	// lateral: the issue's form, a LATERAL sub-select with OFFSET 0.
	lat := func(cond string) string {
		return `context_blocks CROSS JOIN LATERAL (SELECT metadata->>'guard_checked_at' AS v OFFSET 0) s
		  WHERE ` + cond + mfFlaggedWhere
	}
	lateral := mfForm{name: "lateral",
		rollup: `SELECT scope, GROUPING(scope) AS is_total, ` + mfCounts + `, min(` + mfOuterOnce + `)
	FROM ` + lat("") + ` GROUP BY ROLLUP (scope)`,
		daily: `SELECT ` + mfCounts + `, min(` + mfOuterOnce + `) FROM ` + lat("scope = $1 AND "),
	}
	head := mfForm{name: "head", rollup: guardGenRollupSQL, daily: mfHeadDailySQL}
	return []mfForm{pre, base, once, lateral, head}
}

type mfFixture struct {
	name                    string
	total, flagged, toasted int
}

var mfFixtures = []mfFixture{
	{"inline50k", 200_000, 50_000, 0},
	{"toast22k", 200_000, 22_000, 2_000},
	{"inline500k", 1_000_000, 500_000, 0},
}

// mfLoad writes total rows in 20 scopes (mf00 holds half of every class, the
// other 19 the rest), flagged of them in the three flagged states, toasted of
// the flagged ones with ~8 KB of incompressible metadata, the others 'clean'.
// Every stamp lies between the row's creation and now.
func mfLoad(t *testing.T, pool *pgxpool.Pool, f mfFixture) {
	t.Helper()
	ctx := context.Background()
	start := time.Now()
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_blocks (category, title, content, scope, guard_status, metadata, created_at, updated_at)
		SELECT 'reference', 'mf-' || g, 'measure body ' || g,
		       CASE WHEN g % 2 = 0 THEN 'mf00' ELSE 'mf' || lpad((1 + g % 19)::text, 2, '0') END,
		       CASE WHEN g > $2 THEN 'clean'
		            WHEN g % 3 = 0 THEN 'needs_review' WHEN g % 3 = 1 THEN 'near_duplicate' ELSE 'possible_duplicate' END,
		       jsonb_build_object(
		           'guard_status', CASE WHEN g > $2 THEN 'clean' ELSE 'needs_review' END,
		           'guard_checked_at', to_char((now() - (g % 1000) * interval '1 minute') AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
		           'guard_similarity', 0.9 + (g % 100) / 1000.0,
		           'guard_matched_id', gen_random_uuid()::text,
		           'guard_is_cross_scope', false,
		           'guard_threshold_duplicate', 0.98,
		           'guard_threshold_review', 0.92,
		           'source', 'measure', 'project', 'ctx', 'agent_type', 'claude-code',
		           'session_id', md5(g::text) || md5((g + 1)::text))
		       || CASE WHEN g <= $3 THEN jsonb_build_object('blob', (SELECT string_agg(encode(gen_random_bytes(1000), 'base64'), '') FROM generate_series(1, 6) WHERE g IS NOT NULL)) ELSE '{}'::jsonb END,
		       now() - interval '30 days', now() - interval '20 days'
		  FROM generate_series(1, $1) AS g`, f.total, f.flagged, f.toasted); err != nil {
		t.Fatalf("load %s: %v", f.name, err)
	}
	if _, err := pool.Exec(ctx, `VACUUM ANALYZE context_blocks`); err != nil {
		t.Fatalf("vacuum analyze: %v", err)
	}
	var avgMeta, toastPages float64
	if err := pool.QueryRow(ctx, `SELECT avg(pg_column_size(metadata)) FILTER (WHERE guard_status <> 'clean'),
	        (SELECT relpages FROM pg_class WHERE oid = (SELECT reltoastrelid FROM pg_class WHERE oid = 'context_blocks'::regclass))
	   FROM context_blocks`).Scan(&avgMeta, &toastPages); err != nil {
		t.Fatalf("fixture stats: %v", err)
	}
	t.Logf("MEASURE|fixture|%s|total=%d|flagged=%d|toasted=%d|avg_flagged_metadata_bytes=%.0f|toast_pages=%.0f|load_s=%.1f",
		f.name, f.total, f.flagged, f.toasted, avgMeta, toastPages, time.Since(start).Seconds())
}

func mfMedian(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// mfScan names the scan nodes of a JSON plan (node type, and the index it
// reads) and the aggregate strategy.
func mfScan(plan string) string {
	var doc []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(plan), &doc); err != nil || len(doc) != 1 {
		return "?"
	}
	var out []string
	var walk func(n map[string]any)
	walk = func(n map[string]any) {
		nt, _ := n["Node Type"].(string)
		switch {
		case strings.Contains(nt, "Scan") && nt != "Subquery Scan":
			if idx, ok := n["Index Name"].(string); ok {
				nt += "(" + idx + ")"
			}
			out = append(out, nt)
		case nt == "Aggregate":
			st, _ := n["Strategy"].(string)
			out = append(out, "Agg:"+st)
		case nt == "Sort":
			out = append(out, "Sort")
		}
		if kids, ok := n["Plans"].([]any); ok {
			for _, k := range kids {
				if m, ok := k.(map[string]any); ok {
					walk(m)
				}
			}
		}
	}
	walk(doc[0].Plan)
	return strings.Join(out, ">")
}

// mfAnswer runs a form and renders its rows.
func mfAnswer(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	rows, err := pool.Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatalf("answer: %v\n%s", err, sql)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			t.Fatalf("answer values: %v", err)
		}
		lines = append(lines, fmt.Sprint(vals...))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("answer rows: %v\n%s", err, sql)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func TestGuardFlagTimeMeasure(t *testing.T) {
	if os.Getenv("CTX_GUARD_FLAGTIME_MEASURE") == "" {
		t.Skip("measurement; set CTX_GUARD_FLAGTIME_MEASURE=1")
	}
	pick := map[string]bool{}
	for _, n := range strings.Split(os.Getenv("CTX_GUARD_FLAGTIME_FIXTURES"), ",") {
		if n != "" {
			pick[n] = true
		}
	}
	for _, f := range mfFixtures {
		if len(pick) > 0 && !pick[f.name] {
			continue
		}
		t.Run(f.name, func(t *testing.T) {
			pool := testdb.SetupTestDB(t)
			mfLoad(t, pool, f)
			forms := mfForms()
			// One answer: every form returns what the base returns, in one
			// transaction each, so now() differs between them; the fixture's
			// stamps lie well inside the bounds, so it does not matter here.
			want := mfAnswer(t, pool, forms[1].rollup)
			wantDaily := mfAnswer(t, pool, forms[1].daily, "mf00")
			for _, fm := range forms[1:] {
				if got := mfAnswer(t, pool, fm.rollup); got != want {
					t.Errorf("form %s: rollup answer differs from the base", fm.name)
				}
				if got := mfAnswer(t, pool, fm.daily, "mf00"); got != wantDaily {
					t.Errorf("form %s: daily answer differs from the base", fm.name)
				}
			}
			for _, rpc := range testdb.RandomPageCosts {
				p := testdb.WithRandomPageCost(t, pool, rpc)
				for _, fm := range forms {
					for _, reader := range []string{"rollup", "daily"} {
						for _, mode := range []string{"custom", "generic"} {
							if reader == "rollup" && mode == "generic" {
								continue // no parameter: the generic plan is the custom plan
							}
							run := func() testdb.PlanCost {
								if reader == "rollup" {
									return testdb.ExplainCustom(t, p, fm.rollup)
								}
								if mode == "generic" {
									return testdb.ExplainGeneric(t, p, fm.daily, "mf00")
								}
								return testdb.ExplainCustom(t, p, fm.daily, "mf00")
							}
							run() // warm-up
							var ms []float64
							var last testdb.PlanCost
							for i := 0; i < 7; i++ {
								last = run()
								ms = append(ms, last.ExecMs)
							}
							t.Logf("MEASURE|%s|rpc=%s|%s|%s|%s|median_ms=%.2f|min_ms=%.2f|buffers=%d|scan=%s",
								f.name, rpc, reader, mode, fm.name, mfMedian(ms), minF(ms), last.Buffers, mfScan(last.Plan))
						}
					}
				}
			}
		})
	}
}

func minF(xs []float64) float64 {
	m := xs[0]
	for _, x := range xs[1:] {
		if x < m {
			m = x
		}
	}
	return m
}

// mfHeadDailySQL is dream.dailyGuardReviewSQL (another package): the daily
// report's statement over guard.FlaggedSQL.
var mfHeadDailySQL = `SELECT ` + mfCounts + `, min(flagged_at) FROM ` + guard.FlaggedSQL(" AND scope = $1") + ` AS f`
