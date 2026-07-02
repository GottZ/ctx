//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/overview"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func ovInsBlock(t *testing.T, pool *pgxpool.Pool, id, scope, category, title string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks (id, category, title, content, scope) VALUES ($1::uuid,$2,$3,$4,$5)`,
		id, category, title, "content "+title, scope); err != nil {
		t.Fatalf("insert block %s: %v", id, err)
	}
}

func ovInsLink(t *testing.T, pool *pgxpool.Pool, src, dst string, w float64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
		 VALUES ($1::uuid,$2::uuid,'topical',$3,$3,'private')`,
		src, dst, w); err != nil {
		t.Fatalf("insert link %s->%s: %v", src, dst, err)
	}
}

// TestGraphOverview_ScopeNegativeProbes is the W2 OpSec gate (design §5.2):
// the same clustered corpus seen through different readScopes must expose only
// scope-pure aggregates. Triangle1 {A,B private + C shared}, Triangle2 {D,E,F
// work}, weak cross-scope bridge C-F.
func TestGraphOverview_ScopeNegativeProbes(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const (
		A = "019d0000-0000-7000-9000-000000000001" // private
		B = "019d0000-0000-7000-9000-000000000002" // private
		C = "019d0000-0000-7000-9000-000000000003" // shared
		D = "019d0000-0000-7000-9000-000000000004" // work
		E = "019d0000-0000-7000-9000-000000000005" // work
		F = "019d0000-0000-7000-9000-000000000006" // work
	)
	ovInsBlock(t, pool, A, "private", "learnings", "A")
	ovInsBlock(t, pool, B, "private", "learnings", "B")
	ovInsBlock(t, pool, C, "shared", "decisions", "C")
	ovInsBlock(t, pool, D, "work", "infrastructure", "D")
	ovInsBlock(t, pool, E, "work", "infrastructure", "E")
	ovInsBlock(t, pool, F, "work", "infrastructure", "F")
	ovInsLink(t, pool, A, B, 0.9)
	ovInsLink(t, pool, B, C, 0.9)
	ovInsLink(t, pool, A, C, 0.9)
	ovInsLink(t, pool, D, E, 0.9)
	ovInsLink(t, pool, E, F, 0.9)
	ovInsLink(t, pool, D, F, 0.9)
	ovInsLink(t, pool, C, F, 0.05) // bridge

	if _, err := overview.Rebuild(ctx, pool, 1.0, []string{"knowledge"}, []string{"knowledge"}); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	params := store.OverviewParams{MinClusterSize: 1, NodeLimit: 500, EdgeLimit: 2000}
	get := func(scopes ...string) *store.OverviewResult {
		t.Helper()
		res, err := store.GraphOverview(ctx, pool, params, scopes)
		if err != nil {
			t.Fatalf("GraphOverview(%v): %v", scopes, err)
		}
		return res
	}

	// PROBE 1 — Size-Leak: a [shared]-only tenant sees Triangle1 ONLY through C
	// (size 1), NOT the private A,B (which would be size 3). The private members
	// never enter this tenant's sum.
	if r := get("shared"); len(r.Nodes) != 1 {
		t.Errorf("[shared]: want 1 node, got %d (%+v)", len(r.Nodes), r.Nodes)
	} else if r.Nodes[0].Size != 1 {
		t.Errorf("[shared] size leak: want size 1 (only C), got %d — private members leaked", r.Nodes[0].Size)
	} else if len(r.Edges) != 0 {
		t.Errorf("[shared]: want 0 edges (bridge needs work), got %d", len(r.Edges))
	}

	// PROBE 2 — full Triangle1 for the owning scopes, but still no work cluster
	// and no cross-scope edge ([private,shared] cannot see the work endpoint).
	if r := get("private", "shared"); len(r.Nodes) != 1 {
		t.Errorf("[private,shared]: want 1 node (A,B,C), got %d (%+v)", len(r.Nodes), r.Nodes)
	} else if r.Nodes[0].Size != 3 {
		t.Errorf("[private,shared]: want size 3, got %d", r.Nodes[0].Size)
	} else if len(r.Edges) != 0 {
		t.Errorf("[private,shared] edge leak: want 0 (bridge has a work endpoint), got %d", len(r.Edges))
	}

	// PROBE 3 — Foreign-only cluster: a [work] tenant sees ONLY Triangle2, never
	// the private/shared cluster (vector 4).
	if r := get("work"); len(r.Nodes) != 1 {
		t.Errorf("[work]: want 1 node (D,E,F), got %d (%+v)", len(r.Nodes), r.Nodes)
	} else if r.Nodes[0].Size != 3 {
		t.Errorf("[work]: want size 3, got %d", r.Nodes[0].Size)
	} else if len(r.Edges) != 0 {
		t.Errorf("[work]: want 0 edges, got %d", len(r.Edges))
	}

	// PROBE 4 — Full visibility: both clusters AND the bridge meta-edge (its two
	// endpoint scopes shared+work are both visible).
	r := get("private", "shared", "work")
	if len(r.Nodes) != 2 {
		t.Fatalf("[all]: want 2 nodes, got %d (%+v)", len(r.Nodes), r.Nodes)
	}
	total := r.Nodes[0].Size + r.Nodes[1].Size
	if total != 6 {
		t.Errorf("[all]: want total size 6, got %d", total)
	}
	if len(r.Edges) != 1 {
		t.Fatalf("[all]: want 1 bridge edge, got %d (%+v)", len(r.Edges), r.Edges)
	}
	if r.Edges[0].LinkCount != 1 {
		t.Errorf("[all]: bridge link_count = %d, want 1", r.Edges[0].LinkCount)
	}

	// scope_mix never contains a scope outside the caller's readScopes.
	for _, n := range r.Nodes {
		for _, s := range n.ScopeMix {
			if s != "private" && s != "shared" && s != "work" {
				t.Errorf("scope_mix leaked foreign scope %q", s)
			}
		}
	}
}

// TestGraphOverview_Empty: a tenant whose scope has no blocks gets an empty map
// (no error, no leak), and the never-built case is handled (ComputedAt zero).
func TestGraphOverview_Empty(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	res, err := store.GraphOverview(ctx, pool, store.OverviewParams{MinClusterSize: 1, NodeLimit: 500, EdgeLimit: 2000}, []string{"nonexistent"})
	if err != nil {
		t.Fatalf("empty overview errored: %v", err)
	}
	if len(res.Nodes) != 0 || len(res.Edges) != 0 {
		t.Errorf("expected empty map, got %d nodes / %d edges", len(res.Nodes), len(res.Edges))
	}
}
