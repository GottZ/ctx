//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// Fixture UUIDs (fa*) — own namespace, one container per test (testdb).
const (
	faS1 = "019e0009-0000-7000-9000-000000000011" // shared, newest
	faS2 = "019e0009-0000-7000-9000-000000000012" // shared
	faP1 = "019e0009-0000-7000-9000-000000000021" // private
	faW1 = "019e0009-0000-7000-9000-000000000031" // work — invisible to scope set A
)

// faInsertBlock seeds one block with a DISTINCT created_at (ordering /
// truncation determinism depends on it; hour = the given offset).
func faInsertBlock(t *testing.T, pool *pgxpool.Pool, id, scope string, hourOffset int) {
	t.Helper()
	ts := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(hourOffset) * time.Hour)
	_, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks (id, category, title, content, scope, created_at, updated_at)
		 VALUES ($1::uuid, 'graphtest', $2, 'full-graph fixture', $3, $4, $4)`,
		id, "blk-"+id[len(id)-4:], scope, ts,
	)
	if err != nil {
		t.Fatalf("insert block %s: %v", id, err)
	}
}

func faParams(limit int) store.EgoParams {
	return store.EgoParams{Limit: limit, EdgeLimit: 4000}
}

// TestFullGraph pins the load-all seed: visibility (scope + type triple on the
// FLAT node set), induced edges of both classes between visible nodes only,
// deterministic newest-first truncation, and the fail-closed entry checks.
func TestFullGraph(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	faInsertBlock(t, pool, faS1, "shared", 3) // newest
	faInsertBlock(t, pool, faS2, "shared", 2)
	faInsertBlock(t, pool, faP1, "private", 1)
	faInsertBlock(t, pool, faW1, "work", 0) // oldest, invisible to A
	gInsertLink(t, pool, faS1, faS2, "topical", 0.9, 0.9)
	gInsertLink(t, pool, faS1, faP1, "topical", 0.8, 0.8)
	gInsertLink(t, pool, faS1, faW1, "topical", 0.7, 0.7) // hidden endpoint → must not deliver
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_structural_links (source_block_id, target_block_id, link_class, scope, origin)
		 VALUES ($1::uuid,$2::uuid,'references','shared','system')`, faS2, faP1); err != nil {
		t.Fatalf("insert structural link: %v", err)
	}

	t.Run("VisibilityAndEdges", func(t *testing.T) {
		res, err := store.FullGraph(ctx, pool, faParams(500), gScopesA, nil, gVisibleTypes)
		if err != nil {
			t.Fatalf("FullGraph: %v", err)
		}
		if res.Focus != "" {
			t.Errorf("Focus = %q, want empty (no focus exists)", res.Focus)
		}
		nodes := gNodeIDs(res)
		if len(nodes) != 3 {
			t.Fatalf("nodes = %d (%v), want 3 (W1 invisible)", len(nodes), nodes)
		}
		if _, leaked := nodes[faW1]; leaked {
			t.Error("work-scope block leaked into scope set A")
		}
		// Newest-first determinism: S1 (hour 3) before S2 before P1.
		if res.Nodes[0].ID != faS1 || res.Nodes[2].ID != faP1 {
			t.Errorf("order wrong: %s, %s, %s", res.Nodes[0].ID, res.Nodes[1].ID, res.Nodes[2].ID)
		}
		pairs := gEdgePairs(t, res)
		if len(pairs) != 2 {
			t.Errorf("dream edges = %v, want exactly the 2 visible-endpoint edges", pairs)
		}
		if _, leaked := pairs[faS1+"→"+faW1]; leaked {
			t.Error("edge to invisible endpoint delivered")
		}
		if len(res.StructEdges) != 1 || len(res.StructRels) != 1 || res.StructRels[0] != "references" {
			t.Errorf("struct edges/legend wrong: %v / %v", res.StructEdges, res.StructRels)
		}
		if res.Truncated {
			t.Error("Truncated set without any budget cut")
		}
		// Degree covers BOTH classes: P1 has one dream + one structural row = 2.
		if nodes[faP1].Degree != 2 {
			t.Errorf("P1 degree = %d, want 2 (dream+structural)", nodes[faP1].Degree)
		}
	})

	t.Run("TruncationNewestFirst", func(t *testing.T) {
		res, err := store.FullGraph(ctx, pool, faParams(2), gScopesA, nil, gVisibleTypes)
		if err != nil {
			t.Fatalf("FullGraph: %v", err)
		}
		if len(res.Nodes) != 2 || !res.Truncated {
			t.Fatalf("nodes = %d truncated = %v, want 2/true", len(res.Nodes), res.Truncated)
		}
		if res.Nodes[0].ID != faS1 || res.Nodes[1].ID != faS2 {
			t.Errorf("truncation must keep the newest: %s, %s", res.Nodes[0].ID, res.Nodes[1].ID)
		}
	})

	t.Run("FailClosedEntry", func(t *testing.T) {
		if _, err := store.FullGraph(ctx, pool, faParams(500), nil, nil, gVisibleTypes); err == nil {
			t.Error("empty read scopes accepted (T07 fail-closed violated)")
		}
		if _, err := store.FullGraph(ctx, pool, faParams(500), gScopesA, nil, nil); err == nil {
			t.Error("empty visible-types allowlist accepted (T6 fail-closed violated)")
		}
	})
}
