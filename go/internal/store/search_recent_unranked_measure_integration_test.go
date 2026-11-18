//go:build integration

// Measurement of issue #342 (not a gate): the compact search and recent
// statements before and after the page subquery, on stores of the reference
// instance's shape — 57 % never-ranked checkpoint blocks of 35 000 characters,
// interleaved in time with ranked blocks of 500 to 6 000 — and on the same
// ranked blocks alone. Generic plan (the plan a long-lived pool runs), one warm
// run and five measured runs per statement; it logs the median and the
// buffers. The gate is TestSearchRecentNeverRankedRowsCost.
//
//	CTX_UNRANKED_MEASURE=10000,200000 go test -tags=integration -p 1 ./internal/store/ -run TestSearchRecentNeverRankedRowsMeasure -count=1 -v -timeout 3h
package store_test

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// The compact statements as they were before #342: the preview expressions
// in the scan, below the sort. Scope $1, grants and limit as the builders bind
// them; the full-text one with its query at $2.
const (
	before342Recent = `SELECT id, category, tags, title, scope, type_name, lifecycle_state, type_source, LEFT(content, 200), char_length(content), created_at, updated_at, COALESCE(untrusted_origin, '')
	      FROM context_blocks
	      WHERE NOT is_archived AND ( scope = ANY($1::text[]) OR id = ANY($2::uuid[]) ) ORDER BY updated_at DESC LIMIT $3`
	before342Browse = `SELECT id, category, tags, title, scope, sensitivity, type_name, lifecycle_state, type_source, LEFT(content, 200) AS content_preview, length(content) AS content_length, created_at, updated_at, COALESCE(untrusted_origin, '') FROM context_blocks WHERE NOT is_archived AND (scope = ANY($1::text[]) OR id = ANY($2::uuid[])) ORDER BY updated_at DESC, id DESC LIMIT $3`
	before342FullText = `SELECT id, category, tags, title, scope, sensitivity, type_name, lifecycle_state, type_source, LEFT(content, 200) AS content_preview, length(content) AS content_length, created_at, updated_at, COALESCE(untrusted_origin, '') FROM context_blocks WHERE NOT is_archived AND (ts_de @@ plainto_tsquery('german', $2) OR ts_en @@ plainto_tsquery('english', $2)) AND (scope = ANY($1::text[]) OR id = ANY($3::uuid[])) ORDER BY GREATEST(ts_rank_cd(ts_de, plainto_tsquery('german', $2)), ts_rank_cd(ts_en, plainto_tsquery('english', $2))) DESC LIMIT $4`
)

func measureMedian(t *testing.T, pool *pgxpool.Pool, name, sql string, args ...any) {
	t.Helper()
	testdb.ExplainGeneric(t, pool, sql, args...) // warm
	ms := make([]float64, 0, 5)
	var last testdb.PlanCost
	for r := 0; r < 5; r++ {
		last = testdb.ExplainGeneric(t, pool, sql, args...)
		ms = append(ms, last.ExecMs)
	}
	sort.Float64s(ms)
	t.Logf("%-46s %10.2f ms (min %.2f, max %.2f) %9d buffers", name, ms[2], ms[0], ms[4], last.Buffers)
}

func TestSearchRecentNeverRankedRowsMeasure(t *testing.T) {
	sizes := os.Getenv("CTX_UNRANKED_MEASURE")
	if testing.Short() || sizes == "" {
		t.Skip("measurement; set CTX_UNRANKED_MEASURE to the store sizes, e.g. 10000,200000")
	}
	scopes, grants := []string{"private"}, []string{}
	// Terms by how many blocks hold them: every block, about a fifth of the
	// wide ones, a pair in well under one per cent, and none.
	terms := []struct{ name, q string }{
		{"broad", unrankedWords[30]},
		{"medium", unrankedWords[2000]},
		{"selective", unrankedWords[5000] + " " + unrankedWords[7000]},
		{"absent", "nowherefoundterm"},
	}
	for _, f := range strings.Split(sizes, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 100 {
			t.Fatalf("size %q", f)
		}
		unranked := n * 57 / 100
		shape := unrankedShape{ranked: n - unranked, unranked: unranked, unrankedWidth: 35000, interleaved: true}
		alone := shape
		alone.omitUnranked = true
		for _, st := range []struct {
			label string
			shape unrankedShape
		}{{"without", alone}, {"with", shape}} {
			pool := testdb.SetupSeededTestDB(t, fmt.Sprintf("i342-measure-%d-%s", n, st.label), unrankedSeed(st.shape))
			label := fmt.Sprintf("n=%d %s never-ranked rows:", n, st.label)
			t.Logf("%s heap %d pages", label, unrankedHeapPages(t, pool))
			sql, args := store.RecentBlocksSQLForTest(scopes, "", 10, nil, nil, grants)
			measureMedian(t, pool, label+" recent before", before342Recent, scopes, grants, 10)
			measureMedian(t, pool, label+" recent after", sql, args...)
			sql, args = store.SearchBlocksSQLForTest(store.SearchBlocksParams{ReadScopes: scopes, Limit: 10, Compact: true, ProbeNext: true})
			measureMedian(t, pool, label+" browse before", before342Browse, scopes, grants, 11)
			measureMedian(t, pool, label+" browse after", sql, args...)
			for _, term := range terms {
				sql, args = store.SearchBlocksSQLForTest(store.SearchBlocksParams{Query: term.q, ReadScopes: scopes, Limit: 10, Compact: true, ProbeNext: true})
				measureMedian(t, pool, label+" full text "+term.name+" before", before342FullText, scopes, term.q, grants, 10)
				measureMedian(t, pool, label+" full text "+term.name+" after", sql, args...)
			}
		}
	}
}
