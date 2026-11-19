//go:build integration

package store_test

// Issue #271: the per-scope block count of the server-admin scope-overview
// was the last stats number that counted context_blocks on every call
// (`count(*) … GROUP BY scope`, a pass over every block of every tenant).
// Since then it sums the block counters of migration 170, like stats,
// guard-stats and list-categories:
//
//   - TestScopeOverview_BlockCountCostIndependentOfCorpus pins that the
//     statement reads a number of buffers that does not grow with the
//     corpus (2 000 vs 20 000 blocks);
//   - TestScopeOverview_BlockCountsMatchACount pins that the numbers are the
//     ones the count gave: archived rows counted, a scope whose blocks are
//     all gone absent, before and after a fold.
//
//	go test -tags=integration ./internal/store/ -run 'TestScopeOverview_' -count=1 -v

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// plainStatementBuffers runs a statement without parameters under EXPLAIN
// (ANALYZE, BUFFERS) after one unmeasured execution (hint bits, pruning of
// the bulk write) and returns shared hit + read of the whole execution. A
// statement without parameters has one plan, so there is no custom/generic
// pair to pin.
func plainStatementBuffers(t *testing.T, pool *pgxpool.Pool, sql string) int64 {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	var raw []byte
	if err := conn.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+sql).Scan(&raw); err != nil {
		t.Fatalf("explain: %v", err)
	}
	var out []struct {
		Plan struct {
			Hit  int64 `json:"Shared Hit Blocks"`
			Read int64 `json:"Shared Read Blocks"`
		} `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out) != 1 {
		t.Fatalf("explain json: %v (%s)", err, raw)
	}
	return out[0].Plan.Hit + out[0].Plan.Read
}

func TestScopeOverview_BlockCountCostIndependentOfCorpus(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	const small, large = 2000, 20000
	for _, s := range []string{"cost-y", "cost-z"} {
		seedCostBlocks(t, pool, s, 1, 50)
	}
	seedCostBlocks(t, pool, "cost-x", 1, small)
	before := plainStatementBuffers(t, pool, store.ScopeOverviewBlockCountsSQLForTest)
	seedCostBlocks(t, pool, "cost-x", small+1, large)
	after := plainStatementBuffers(t, pool, store.ScopeOverviewBlockCountsSQLForTest)
	t.Logf("scope-overview block counts: %d buffers at %d blocks, %d at %d", before, small, after, large)
	if after > before+costSlackBuffers {
		t.Errorf("scope-overview block counts: %d buffers at %d blocks vs %d at %d — the statement grows with the corpus",
			after, large, before, small)
	}
}

func TestScopeOverview_BlockCountsMatchACount(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	seedCostBlocks(t, pool, "ov-a", 1, 400) // every 13th archived
	seedCostBlocks(t, pool, "ov-b", 1, 30)
	seedCostBlocks(t, pool, "ov-gone", 1, 25)
	if _, err := pool.Exec(ctx, `DELETE FROM context_blocks WHERE scope = 'ov-gone'`); err != nil {
		t.Fatal(err)
	}

	check := func(stage string) {
		t.Helper()
		want := map[string]int{}
		rows, err := pool.Query(ctx, `SELECT scope, count(*)::int FROM context_blocks GROUP BY scope`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			var n int
			if err := rows.Scan(&s, &n); err != nil {
				t.Fatal(err)
			}
			want[s] = n
		}
		rows.Close()
		if want["ov-a"] != 400 || want["ov-b"] != 30 {
			t.Fatalf("%s: seed not as expected: %v", stage, want)
		}
		got, err := store.ScopeOverviews(ctx, pool)
		if err != nil {
			t.Fatalf("%s: ScopeOverviews: %v", stage, err)
		}
		seen := map[string]bool{}
		for _, o := range got {
			seen[o.Scope] = true
			if o.BlockCount != want[o.Scope] {
				t.Errorf("%s: scope %q block_count = %d, a count gives %d", stage, o.Scope, o.BlockCount, want[o.Scope])
			}
			if o.Scope == "ov-gone" {
				t.Errorf("%s: scope ov-gone has no blocks, keys or tenant and is listed: %+v", stage, o)
			}
		}
		for s := range want {
			if !seen[s] {
				t.Errorf("%s: scope %q with %d blocks is missing", stage, s, want[s])
			}
		}
	}
	check("deltas open")
	if _, err := store.FoldBlockCountDeltas(ctx, pool); err != nil {
		t.Fatal(err)
	}
	check("folded")
}
