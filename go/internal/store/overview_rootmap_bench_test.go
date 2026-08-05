//go:build integration

// W-B gate 8 (design/02 §6.3): the read cost of the root map, MEASURED instead
// of guessed. The earlier design revision carried "~50–200 ms" for overviewNodes
// at ~10^5 rows with no EXPLAIN and no bench behind it — this file replaces that
// number with one that has provenance.
//
// The assumption is not trivial: idx_gcn_scope is a SINGLE-column btree on scope
// (057_graph_overview.sql), so in a single-tenant install `scope = ANY('{private}')`
// matches practically every row and the planner picks Seq Scan + HashAggregate +
// Sort for `ORDER BY sum(size) DESC`. The measurement therefore covers the
// many-scope shape too, which is what answers whether a (scope, cluster_id)
// index is needed — an additive migration on a small table, no lock problem.
//
// Opt-in like the ego bench next door (CTX_BENCH_DSN there): seeding 10^6 rows
// costs minutes, so it stays out of the normal integration run. The
// context_blocks seed is kept at 50k on purpose — writing blocks is dominated
// by the per-write trg_digest_dirty UPDATE on a singleton row (the
// serialization point W-G removes), which would otherwise measure the seed
// instead of the read under test.
//
//	CTX_ROOTMAP_BENCH=1 go test -tags=integration ./internal/store/ \
//	    -run TestOverviewRootMapReadCost -v -timeout 60m
package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// seedClusterNodes fills graph_cluster_node with `rows` synthetic partial rows
// spread over `scopes` scopes — the shape §6.4 extrapolates over (10^5 rows ≈ 1M
// corpus blocks, 10^6 rows ≈ 10M at the same type mix).
func seedClusterNodes(t *testing.T, pool *pgxpool.Pool, rows, scopes, avgSize int) {
	t.Helper()
	start := time.Now()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO graph_cluster_node (cluster_id, scope, size, category_counts, repr_block_id, repr_title, repr_quality)
		SELECT (('019d3000-0000-7000-9000-' || lpad(to_hex(g), 12, '0'))::uuid),
		       'scope-' || (g % $2),
		       1 + (g % (2 * $3)),
		       jsonb_build_object('learnings', 1 + g % 7, 'decisions', 1 + g % 5, 'projects', 1 + g % 3),
		       (('019d4000-0000-7000-9000-' || lpad(to_hex(g), 12, '0'))::uuid),
		       'repr title ' || g, (g % 100)::real / 100
		  FROM generate_series(1, $1) g`, rows, scopes, avgSize); err != nil {
		t.Fatalf("seed %d cluster rows: %v", rows, err)
	}
	if _, err := pool.Exec(context.Background(), `ANALYZE graph_cluster_node`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	t.Logf("seeded %d cluster rows over %d scopes in %s", rows, scopes, time.Since(start).Round(time.Millisecond))
}

// timeIt reports the median of n runs — a median, not a mean, so one cold first
// run cannot carry the number that ends up in the design table.
func timeIt(t *testing.T, n int, f func() error) time.Duration {
	t.Helper()
	samples := make([]time.Duration, 0, n)
	for range n {
		start := time.Now()
		if err := f(); err != nil {
			t.Fatalf("measured call failed: %v", err)
		}
		samples = append(samples, time.Since(start))
	}
	for i := 1; i < len(samples); i++ { // insertion sort, n is single digit
		for j := i; j > 0 && samples[j] < samples[j-1]; j-- {
			samples[j], samples[j-1] = samples[j-1], samples[j]
		}
	}
	return samples[len(samples)/2]
}

func TestOverviewRootMapReadCost(t *testing.T) {
	if os.Getenv("CTX_ROOTMAP_BENCH") == "" {
		t.Skip("CTX_ROOTMAP_BENCH unset — opt-in measurement (seeds up to 10^6 rows)")
	}
	cases := []struct {
		rows, scopes, blocks int
	}{
		{100_000, 1, 50_000},   // ~1M corpus blocks, single tenant
		{100_000, 50, 50_000},  // same size, many partitions
		{1_000_000, 1, 50_000}, // ~10M corpus blocks
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("rows=%d/scopes=%d", c.rows, c.scopes), func(t *testing.T) {
			pool := testdb.SetupTestDB(t)
			seedClusterNodes(t, pool, c.rows, c.scopes, 20)
			if _, err := pool.Exec(context.Background(), `
				INSERT INTO context_blocks (category, title, content, scope)
				SELECT 'learnings', 'blk ' || g, 'body', 'scope-' || (g % $2)
				  FROM generate_series(1, $1) g`, c.blocks, c.scopes); err != nil {
				t.Fatalf("seed blocks: %v", err)
			}

			read := make([]string, 0, c.scopes)
			for i := range c.scopes {
				read = append(read, fmt.Sprintf("scope-%d", i))
			}
			ctx := context.Background()
			// NodeLimit 95 is the typical root-map line budget (§4.3).
			params := store.OverviewParams{MinClusterSize: 1, NodeLimit: 95, EdgeLimit: 0}

			t.Logf("GraphOverview   (nodes+categories, EdgeLimit=0): %s",
				timeIt(t, 5, func() error { _, err := store.GraphOverview(ctx, pool, params, read); return err }))
			t.Logf("OverviewTotals:  %s",
				timeIt(t, 5, func() error { _, err := store.OverviewTotals(ctx, pool, read, 2); return err }))
			t.Logf("OverviewMeta:    %s",
				timeIt(t, 5, func() error { _, err := store.OverviewMeta(ctx, pool, read); return err }))
			t.Logf("ActiveBlockCount (%d blocks): %s", c.blocks,
				timeIt(t, 5, func() error {
					_, _, err := store.ActiveBlockCount(ctx, pool, read, 30*time.Second)
					return err
				}))

			var plan string
			if err := pool.QueryRow(ctx, `
				EXPLAIN (FORMAT TEXT) SELECT cluster_id, sum(size)::int
				  FROM graph_cluster_node WHERE scope = ANY($1::text[])
				 GROUP BY cluster_id ORDER BY sum(size) DESC, cluster_id LIMIT 96`, read).Scan(&plan); err != nil {
				t.Fatalf("explain: %v", err)
			}
			t.Logf("overviewNodes plan (top line): %s", plan)
		})
	}
}
