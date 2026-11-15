//go:build integration

// Issue #197, cost pin: SupersedesMap now reads pinned links and the two
// linked blocks to tell the query which targets a pinned supersedes settles.
// The statement takes the candidate ids of ONE query (bounded by the internal
// limit, 200 by default) — its cost must not grow with the corpus. Measured
// with the generic plan (what a driver-cached statement runs after five
// executions) and the custom plan, at 10 000 and 100 000 blocks with 1 % of
// them superseded through a pinned link.
//
// The corpus is seeded with set-based SQL: there is no production path that
// writes 100 000 blocks and their links in test time, and the statement under
// test reads only the rows, not how they were written.
//
//	go test -tags=integration ./internal/dream/ -run TestSupersedesMapCostDoesNotGrowWithTheCorpus -count=1 -v
package dream

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// supersedesMapIDs is the size of one query's candidate set.
const supersedesMapIDs = 200

func seedSupersedesCorpus(t *testing.T, pool *pgxpool.Pool, n int) []string {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, scope)
		 SELECT 'cost', 'cost-' || g, 'cost content ' || g, 'private'
		   FROM generate_series(1, $1) g`, n); err != nil {
		t.Fatalf("seed blocks: %v", err)
	}
	// Every 100th block is superseded by its neighbour through a pinned link.
	if _, err := pool.Exec(ctx,
		`WITH b AS (SELECT id, row_number() OVER (ORDER BY title) AS rn FROM context_blocks WHERE category = 'cost')
		 INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope, pinned)
		 SELECT s.id, t.id, 'supersedes', 1, 1, 'private', true
		   FROM b t JOIN b s ON s.rn = t.rn + 1
		  WHERE t.rn % 100 = 0`); err != nil {
		t.Fatalf("seed links: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_dream_links`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	// One query's candidates: a contiguous title range, so it holds the same
	// share of superseded blocks at both sizes.
	rows, err := pool.Query(ctx,
		`SELECT id::text FROM context_blocks WHERE category = 'cost' ORDER BY title LIMIT $1`, supersedesMapIDs)
	if err != nil {
		t.Fatalf("pick ids: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan id: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestSupersedesMapCostDoesNotGrowWithTheCorpus(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	sizes := []int{10_000, 100_000}
	var costs [2][2]testdb.PlanCost
	for i, n := range sizes {
		pool := testdb.SetupTestDB(t)
		ids := seedSupersedesCorpus(t, pool, n)
		_, settled, err := SupersedesMap(context.Background(), pool, ids)
		if err != nil {
			t.Fatalf("supersedes map at %d: %v", n, err)
		}
		if len(settled) == 0 {
			t.Fatalf("premise at %d: the candidate set holds no settled target", n)
		}
		costs[i][0] = testdb.ExplainGeneric(t, pool, supersedesMapSQL, ids)
		costs[i][1] = testdb.ExplainCustom(t, pool, supersedesMapSQL, ids)
		t.Logf("n=%d settled=%d generic=%d custom=%d buffers", n, len(settled), costs[i][0].Buffers, costs[i][1].Buffers)
	}
	for p, name := range []string{"generic", "custom"} {
		small, large := costs[0][p].Buffers, costs[1][p].Buffers
		if limit := small + small/2 + 8; large > limit {
			t.Errorf("%s plan: %d buffers at %d blocks, %d at %d — above %d, the statement grows with the corpus\n%s",
				name, small, sizes[0], large, sizes[1], limit, costs[1][p].Plan)
		}
	}
}
