//go:build integration

package store_test

// Issue #266 cost pin: the stats statements read a number of buffers that
// does not grow with the corpus. Same three statements, same read scopes,
// 2 000 vs 20 000 blocks in the scope: the buffer count stays within a small
// slack (btree depth), where a count over context_blocks grows tenfold. Both
// the custom plan and the generic plan are pinned — pgx switches a prepared
// statement to the generic plan from its sixth execution on a connection —
// and each under random_page_cost 1.1 and 4 (testdb.RandomPageCosts): the CI
// image prices pages at 1.1, a deployment at 4, and at 4 the oldest/newest
// entry walked idx_context_created before issue #512.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// costSlackBuffers absorbs one more btree level per index probe between the
// two corpus sizes (two min/max probes per read scope, three read scopes).
const costSlackBuffers = 8

// statementBuffers runs sql under EXPLAIN (ANALYZE, BUFFERS) on one
// connection, as a custom plan or forced generic plan of a prepared statement,
// and returns shared hit + read of the top plan node (inclusive of its
// children and init plans).
func statementBuffers(t *testing.T, pool *pgxpool.Pool, sql string, scopes []string, generic bool) int64 {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	mode := "force_custom_plan"
	if generic {
		mode = "force_generic_plan"
	}
	if _, err := conn.Exec(ctx, "SET plan_cache_mode = "+mode); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(ctx, "RESET plan_cache_mode") }()
	if _, err := conn.Exec(ctx, "PREPARE cost_pin(text[]) AS "+sql); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer func() { _, _ = conn.Exec(ctx, "DEALLOCATE cost_pin") }()
	// One unmeasured execution first: the first read after a bulk write prunes
	// dead row versions (context_guard_state takes one per inserted block via
	// trg_guard_dirty) and sets hint bits — a one-time cost of the write, not
	// of the statement.
	if _, err := conn.Exec(ctx, "EXECUTE cost_pin('{"+strings.Join(scopes, ",")+"}'::text[])"); err != nil {
		t.Fatalf("warm-up: %v", err)
	}
	// EXPLAIN EXECUTE is a utility statement and takes no bind parameters: the
	// read scopes go in as an array literal (test constants, no quoting needed).
	lit := "'{" + strings.Join(scopes, ",") + "}'::text[]"
	var raw []byte
	if err := conn.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE cost_pin("+lit+")").Scan(&raw); err != nil {
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

func seedCostBlocks(t *testing.T, pool *pgxpool.Pool, scope string, from, to int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope, guard_status, is_archived)
		 SELECT 'cost-' || (g % 10), 'cost ' || g, 'cost body ' || g, $1,
		        CASE WHEN g % 4 = 0 THEN 'clean' WHEN g % 9 = 0 THEN NULL ELSE 'active' END,
		        g % 13 = 0
		   FROM generate_series($2::int, $3::int) g`, scope, from, to); err != nil {
		t.Fatalf("seed %s %d..%d: %v", scope, from, to, err)
	}
	if _, err := pool.Exec(context.Background(), `ANALYZE context_blocks`); err != nil {
		t.Fatal(err)
	}
}

func TestBlockCounts_StatsCostIndependentOfCorpus(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	const small, large = 2000, 20000
	for _, s := range []string{"cost-y", "cost-z"} {
		seedCostBlocks(t, pool, s, 1, 50)
	}
	seedCostBlocks(t, pool, "cost-x", 1, small)

	statements := []struct{ name, sql string }{
		{"stats", store.StatsSQLForTest},
		{"guard-stats", store.GuardStatsSQLForTest},
		{"list-categories", store.ListCategoriesSQLForTest},
	}
	windows := [][]string{{"cost-x"}, {"cost-x", "cost-y", "cost-z"}}
	planners := map[string]*pgxpool.Pool{}
	for _, rpc := range testdb.RandomPageCosts {
		planners[rpc] = testdb.WithRandomPageCost(t, pool, rpc)
	}
	type key struct {
		stmt    string
		window  int
		generic bool
		rpc     string
	}
	before := map[key]int64{}
	for _, st := range statements {
		for wi, w := range windows {
			for _, g := range []bool{false, true} {
				for _, rpc := range testdb.RandomPageCosts {
					before[key{st.name, wi, g, rpc}] = statementBuffers(t, planners[rpc], st.sql, w, g)
				}
			}
		}
	}
	seedCostBlocks(t, pool, "cost-x", small+1, large)
	for _, st := range statements {
		for wi, w := range windows {
			for _, g := range []bool{false, true} {
				for _, rpc := range testdb.RandomPageCosts {
					k := key{st.name, wi, g, rpc}
					after := statementBuffers(t, planners[rpc], st.sql, w, g)
					t.Logf("%-15s scopes=%d generic=%-5v random_page_cost=%-3s buffers %d blocks: %d, %d blocks: %d",
						st.name, len(w), g, rpc, small, before[k], large, after)
					if after > before[k]+costSlackBuffers {
						t.Errorf("%s (scopes %v, generic=%v, random_page_cost %s): %d buffers at %d blocks vs %d at %d — the statement grows with the corpus",
							st.name, w, g, rpc, after, large, before[k], small)
					}
				}
			}
		}
	}
}
