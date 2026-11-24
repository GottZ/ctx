//go:build integration

// Issue #197, cost pin: archiving a block or moving it to another scope first
// locks the block and the targets of its supersedes links
// (lockSupersedesSource). The statement must cost what a primary-key lookup of
// a handful of ids costs, at any corpus size. A first form, `id = $1 OR id IN
// (subquery)`, was planned as a filter with a hashed subplan over a full
// primary-key scan: 569 → 5653 buffers at 10 000 → 100 000 blocks. Measured
// with the generic plan (what a driver-cached statement runs after five
// executions) and the custom plan. The guard resolve batch locks its rows and
// their targets with the same shape over an id array (issue #284) and is
// pinned the same way.
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
	rpcs := testdb.RandomPageCosts
	// [random_page_cost][size][generic, custom]
	costs := make([][2][2]testdb.PlanCost, len(rpcs))
	batch := make([][2][2]testdb.PlanCost, len(rpcs))
	for i, n := range sizes {
		seeded := testdb.SetupTestDB(t)
		ctx := context.Background()
		// Set-based seed: no production path writes this many blocks in test
		// time, and the statement reads only the rows.
		if _, err := seeded.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, scope)
			 SELECT 'cost', 'cost-' || g, 'cost content ' || g, 'private' FROM generate_series(1, $1) g`, n); err != nil {
			t.Fatalf("seed blocks: %v", err)
		}
		// The block under test supersedes three others.
		var src string
		if err := seeded.QueryRow(ctx,
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
			if _, err := seeded.Exec(ctx, `ANALYZE `+tbl); err != nil {
				t.Fatalf("analyze: %v", err)
			}
		}
		for r, rpc := range rpcs {
			pool := testdb.WithRandomPageCost(t, seeded, rpc)
			// The lock pass is two statements: the ordered lock, then the
			// re-read of the targets under the held lock. Both are pinned,
			// summed.
			args := []any{src, "private"}
			costs[r][i][0] = testdb.ExplainGeneric(t, pool, store.LockSupersedesSourceSQLForTest, args...)
			costs[r][i][1] = testdb.ExplainCustom(t, pool, store.LockSupersedesSourceSQLForTest, args...)
			reGeneric := testdb.ExplainGeneric(t, pool, store.SupersedesTargetsOfSQLForTest, []string{src}, []string{"private"})
			reCustom := testdb.ExplainCustom(t, pool, store.SupersedesTargetsOfSQLForTest, []string{src}, []string{"private"})
			costs[r][i][0].Buffers += reGeneric.Buffers
			costs[r][i][1].Buffers += reCustom.Buffers
			// The guard resolve batch's lock statement (issue #284): the same
			// shape over an id array, for an archive with the targets.
			batchArgs := []any{[]string{src}, []string{"private"}, true}
			batch[r][i][0] = testdb.ExplainGeneric(t, pool, store.GuardBatchLockSQLForTest, batchArgs...)
			batch[r][i][1] = testdb.ExplainCustom(t, pool, store.GuardBatchLockSQLForTest, batchArgs...)
			t.Logf("random_page_cost=%s n=%d lock pass: generic=%d (re-read %d) custom=%d (re-read %d); guard batch lock: generic=%d custom=%d buffers",
				rpc, n, costs[r][i][0].Buffers, reGeneric.Buffers, costs[r][i][1].Buffers, reCustom.Buffers,
				batch[r][i][0].Buffers, batch[r][i][1].Buffers)
		}
	}
	for r, rpc := range rpcs {
		for p, name := range []string{"generic", "custom"} {
			for _, pin := range []struct {
				what string
				c    [2][2]testdb.PlanCost
			}{{"the lock pass", costs[r]}, {"the guard batch lock", batch[r]}} {
				small, large := pin.c[0][p].Buffers, pin.c[1][p].Buffers
				if limit := small + small/2 + 8; large > limit {
					t.Errorf("random_page_cost=%s %s plan: %s %d buffers at %d blocks, %d at %d — above %d, it grows with the corpus\n%s",
						rpc, name, pin.what, small, sizes[0], large, sizes[1], limit, pin.c[1][p].Plan)
				}
			}
		}
	}
}
