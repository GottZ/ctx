//go:build integration

// Cost of one browse page of SearchBlocksPage (#206) at two corpus sizes and
// two depths, under the generic plan a long-lived pool runs. A MEASUREMENT,
// not a gate: it logs buffers and time and fails only if a page is not the
// probed statement. The finding it exists for — browse sorts on updated_at
// without an index — belongs to #237.
//
//	CTX_SEARCH_PAGE_COST=1 go test -tags=integration -p 1 ./internal/store/ -run TestSearchPageCost -count=1 -v
package store_test

import (
	"context"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

var nodeTypeRe = regexp.MustCompile(`"Node Type": "([^"]+)"`)

func TestSearchPageCost(t *testing.T) {
	if testing.Short() || os.Getenv("CTX_SEARCH_PAGE_COST") == "" {
		t.Skip("measurement; set CTX_SEARCH_PAGE_COST=1")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seeded := 0
	for _, n := range []int{10_000, 100_000} {
		// n rows of the listed category plus n of another, one scope.
		add := n - seeded
		for _, cat := range []string{"cost-listed", "cost-other"} {
			if _, err := pool.Exec(ctx,
				`INSERT INTO context_blocks (category, title, content, scope, created_at, updated_at)
				 SELECT $1::text, $1::text || ' ' || ($2::int + g), 'body ' || g, 'private',
				        now() - make_interval(secs => ($2::int + g)), now() - make_interval(secs => ($2::int + g))
				   FROM generate_series(1, $3::int) g`, cat, seeded, add); err != nil {
				t.Fatalf("seed %s: %v", cat, err)
			}
		}
		seeded = n
		if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
			t.Fatalf("analyze: %v", err)
		}

		// A deep cursor: the position of the row 100 before the end.
		var deepAt time.Time
		var deepID string
		if err := pool.QueryRow(ctx,
			`SELECT updated_at, id::text FROM context_blocks WHERE category = 'cost-listed'
			 ORDER BY updated_at DESC, id DESC OFFSET $1 LIMIT 1`, n-100).Scan(&deepAt, &deepID); err != nil {
			t.Fatalf("deep cursor: %v", err)
		}
		for _, c := range []struct {
			name  string
			after *store.SearchCursor
		}{
			{"page 1", nil},
			{"deep page", &store.SearchCursor{UpdatedAt: deepAt, ID: deepID}},
		} {
			sql, args := store.SearchBlocksSQLForTest(store.SearchBlocksParams{
				ReadScopes: []string{"private"}, Category: "cost-listed", Limit: store.SearchMaxLimit,
				Compact: true, After: c.after, ProbeNext: true,
			})
			if !strings.Contains(sql, "LIMIT $"+strconv.Itoa(len(args))) || args[len(args)-1] != store.SearchMaxLimit+1 {
				t.Fatalf("%s: not the probed statement (last arg %v)", c.name, args[len(args)-1])
			}
			cost := testdb.ExplainGeneric(t, pool, sql, args...)
			nodes := nodeTypeRe.FindAllStringSubmatch(cost.Plan, -1)
			var kinds []string
			for _, m := range nodes {
				kinds = append(kinds, m[1])
			}
			t.Logf("n=%d %s: buffers %d, %.1f ms, plan %v", n, c.name, cost.Buffers, cost.ExecMs, kinds)
		}
	}
}
