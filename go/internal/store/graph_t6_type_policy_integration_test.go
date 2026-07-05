//go:build integration

// Integration gates for wave T6 (design/01-type-registry.md §7-T6) against a
// real PG18 testcontainer: the ego-graph legs and the overview clustering
// consume the REGISTRY type allowlist instead of the retired
// `type_name <> 'system-meta'` literal.
//
// RED states proven before T6 was written (scratch probe on HEAD ea32d37,
// t6red_scratch_integration_test.go — deleted before this commit, output in
// the wave return): a rogue-typed neighbour WAS visible in the ego graph, a
// rogue focus DID hydrate, and both the rogue and an overview.include=false
// block WERE Louvain members (literal semantics, fail-open).
//
// Run with:
//
//	GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  go test -tags=integration ./internal/store/ -run TestT6TypePolicy -count=1 -v
package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/overview"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	t6Focus  = "019f2206-0000-7000-9000-00000000c001" // knowledge focus
	t6Rogue  = "019f2206-0000-7000-9000-00000000c002" // unregistered type
	t6NoOver = "019f2206-0000-7000-9000-00000000c003" // overview.include=false type
)

func TestT6TypePolicy_GraphAndOverview(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	ts := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

	insert := func(id, typeName string) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (id, category, title, content, scope, type_name, created_at, updated_at)
			 VALUES ($1::uuid, 'learnings', $2, 't6 fixture', 'private', $3, $4, $4)`,
			id, "t6-"+typeName, typeName, ts); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	link := func(src, dst string) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_dream_links
				(source_block_id, target_block_id, relationship, confidence, raw_confidence, scope, dream_version)
			 VALUES ($1::uuid, $2::uuid, 'topical', 0.9, 0.9, 'private', 5)`,
			src, dst); err != nil {
			t.Fatalf("link %s→%s: %v", src, dst, err)
		}
	}

	// Custom registered type: retrieval-visible (full-pass) but NOT an
	// overview node (overview.include=false — the Achse-02 issue seed shape).
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_block_types (name, scope, builtin, is_default, config)
		 VALUES ('wf-noover', '_global', false, false,
		         '{"v":1,"retrieval":{"policy":"full-pass"},"overview":{"include":false}}'::jsonb)`); err != nil {
		t.Fatalf("insert wf-noover type: %v", err)
	}

	// The policy source is the DB registry (T5 doctrine), not a test literal:
	// Reload merges the wf-noover row over the builtins.
	reg := blocktype.NewRegistry()
	if err := reg.Reload(ctx, pool); err != nil {
		t.Fatalf("registry reload: %v", err)
	}
	set := reg.Snapshot()
	visible, overviewTypes := set.VisibleTypes(), set.OverviewTypes()

	insert(t6Focus, "knowledge")
	insert(t6Rogue, "rogue") // SQL INSERT past Go — unregistered, fail-closed invisible
	insert(t6NoOver, "wf-noover")
	link(t6Focus, t6Rogue)
	link(t6Focus, t6NoOver)

	// PROBE 1 (ego graph, T5-negative on the graph endpoint): the rogue
	// neighbour is invisible, the registered custom type is visible (positive
	// control proving the allowlist carries DB-registered types).
	t.Run("EgoGraph_RogueInvisible_CustomVisible", func(t *testing.T) {
		res, err := store.EgoGraph(ctx, pool,
			store.EgoParams{Focus: t6Focus, Hops: 1, PerNodeCap: 25, Limit: 100, EdgeLimit: 100},
			[]string{"private"}, nil, visible)
		if err != nil {
			t.Fatalf("EgoGraph: %v", err)
		}
		var sawNoOver bool
		for _, n := range res.Nodes {
			if n.ID == t6Rogue {
				t.Errorf("rogue-typed neighbour visible in ego graph (allowlist not applied)")
			}
			if n.ID == t6NoOver {
				sawNoOver = true
			}
		}
		if !sawNoOver {
			t.Errorf("registered full-pass type wf-noover missing from ego graph (allowlist too narrow)")
		}
		// Visible degree of the focus counts ONLY the allowlisted neighbour.
		if got := res.Nodes[0].Degree; got != 1 {
			t.Errorf("focus visible degree = %d, want 1 (rogue must not count)", got)
		}
	})

	// PROBE 2: a rogue FOCUS must not hydrate — same ErrNotVisible as a
	// nonexistent block (no existence oracle for policy-invisible types).
	t.Run("EgoGraph_RogueFocusNotVisible", func(t *testing.T) {
		if _, err := store.EgoGraph(ctx, pool,
			store.EgoParams{Focus: t6Rogue, Hops: 1, PerNodeCap: 25, Limit: 100, EdgeLimit: 100},
			[]string{"private"}, nil, visible); !errors.Is(err, store.ErrNotVisible) {
			t.Errorf("rogue focus: err = %v, want store.ErrNotVisible", err)
		}
	})

	// PROBE 3 (fail-closed wiring guard): an empty allowlist is a LOUD Go
	// error, never a silently empty graph (rrf.Search pattern).
	t.Run("EgoGraph_EmptyAllowlistFailsClosed", func(t *testing.T) {
		_, err := store.EgoGraph(ctx, pool,
			store.EgoParams{Focus: t6Focus, Hops: 1, PerNodeCap: 25, Limit: 100, EdgeLimit: 100},
			[]string{"private"}, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "visible-types") {
			t.Errorf("empty allowlist: err = %v, want loud visible-types error", err)
		}
	})

	// PROBE 4 (overview): after a rebuild under the registry lists, neither
	// the rogue block nor the overview.include=false block is a Louvain
	// member; the knowledge focus is (positive control).
	t.Run("Overview_TypeSieve", func(t *testing.T) {
		if _, err := overview.Rebuild(ctx, pool, overview.Options{Resolution: 1.0, VisibleTypes: visible, OverviewTypes: overviewTypes}); err != nil {
			t.Fatalf("Rebuild: %v", err)
		}
		memberCount := func(id string) int {
			var n int
			if err := pool.QueryRow(ctx,
				`SELECT count(*) FROM graph_cluster_member WHERE block_id = $1::uuid`, id).Scan(&n); err != nil {
				t.Fatalf("member count %s: %v", id, err)
			}
			return n
		}
		if n := memberCount(t6Rogue); n != 0 {
			t.Errorf("rogue-typed block is a Louvain member (count=%d, want 0)", n)
		}
		if n := memberCount(t6NoOver); n != 0 {
			t.Errorf("overview.include=false block is a Louvain member (count=%d, want 0)", n)
		}
		if n := memberCount(t6Focus); n != 1 {
			t.Errorf("knowledge focus not a Louvain member (count=%d, want 1)", n)
		}
	})

	// PROBE 5 (overview fail-closed): empty lists are a wiring bug and error
	// loudly instead of clustering an empty (or unfiltered) corpus.
	t.Run("Overview_EmptyAllowlistFailsClosed", func(t *testing.T) {
		if _, err := overview.Rebuild(ctx, pool, overview.Options{Resolution: 1.0, OverviewTypes: overviewTypes}); err == nil {
			t.Error("Rebuild with empty visible list: want loud error, got nil")
		}
		if _, err := overview.Rebuild(ctx, pool, overview.Options{Resolution: 1.0, VisibleTypes: visible}); err == nil {
			t.Error("Rebuild with empty overview list: want loud error, got nil")
		}
	})
}
