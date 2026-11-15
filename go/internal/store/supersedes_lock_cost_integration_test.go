//go:build integration

// Issue #197, cost pin: archiving a block or moving it to another scope first
// locks the block and the targets of its supersedes links
// (lockSupersedesSource). The statement must cost what a primary-key lookup of
// a handful of ids costs, at any corpus size. A first form, `id = $1 OR id IN
// (subquery)`, was planned as a filter with a hashed subplan over a full
// primary-key scan: 569 → 5653 buffers at 10 000 → 100 000 blocks. Measured
// with the generic plan (what a driver-cached statement runs after five
// executions) and the custom plan.
//
//	go test -tags=integration ./internal/store/ -run TestLockSupersedesSourceCostDoesNotGrowWithTheCorpus -count=1 -v
package store_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestLockSupersedesSourceCostDoesNotGrowWithTheCorpus(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	sizes := []int{10_000, 100_000}
	var costs [2][2]testdb.PlanCost
	for i, n := range sizes {
		pool := testdb.SetupTestDB(t)
		ctx := context.Background()
		// Set-based seed: no production path writes this many blocks in test
		// time, and the statement reads only the rows.
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, scope)
			 SELECT 'cost', 'cost-' || g, 'cost content ' || g, 'private' FROM generate_series(1, $1) g`, n); err != nil {
			t.Fatalf("seed blocks: %v", err)
		}
		// The block under test supersedes three others.
		var src string
		if err := pool.QueryRow(ctx,
			`WITH b AS (SELECT id, row_number() OVER (ORDER BY title) AS rn FROM context_blocks WHERE category = 'cost'),
			      l AS (INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope, pinned)
			            SELECT s.id, t.id, 'supersedes', 1, 1, 'private', true
			              FROM b s JOIN b t ON t.rn IN (s.rn + 1, s.rn + 2, s.rn + 3)
			             WHERE s.rn = 500
			            RETURNING source_block_id)
			 SELECT DISTINCT source_block_id::text FROM l`).Scan(&src); err != nil {
			t.Fatalf("seed links: %v", err)
		}
		for _, tbl := range []string{"context_blocks", "context_dream_links"} {
			if _, err := pool.Exec(ctx, `ANALYZE `+tbl); err != nil {
				t.Fatalf("analyze: %v", err)
			}
		}
		// The lock pass is two statements: the ordered lock, then the re-read
		// of the targets under the held lock. Both are pinned, summed.
		args := []any{src, []string{"private"}}
		costs[i][0] = testdb.ExplainGeneric(t, pool, store.LockSupersedesSourceSQLForTest, args...)
		costs[i][1] = testdb.ExplainCustom(t, pool, store.LockSupersedesSourceSQLForTest, args...)
		reGeneric := testdb.ExplainGeneric(t, pool, store.SupersedesTargetsOfSQLForTest, src)
		reCustom := testdb.ExplainCustom(t, pool, store.SupersedesTargetsOfSQLForTest, src)
		costs[i][0].Buffers += reGeneric.Buffers
		costs[i][1].Buffers += reCustom.Buffers
		t.Logf("n=%d generic=%d (re-read %d) custom=%d (re-read %d) buffers", n,
			costs[i][0].Buffers, reGeneric.Buffers, costs[i][1].Buffers, reCustom.Buffers)
	}
	for p, name := range []string{"generic", "custom"} {
		small, large := costs[0][p].Buffers, costs[1][p].Buffers
		if limit := small + small/2 + 8; large > limit {
			t.Errorf("%s plan: %d buffers at %d blocks, %d at %d — above %d, the lock pass grows with the corpus\n%s",
				name, small, sizes[0], large, sizes[1], limit, costs[1][p].Plan)
		}
	}
}
