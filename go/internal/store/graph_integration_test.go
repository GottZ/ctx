//go:build integration

package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// gVisibleTypes is the resolved builtin visible allowlist (WF T6): every
// fixture block below is default-typed 'knowledge'; the two other visible
// builtins keep the bind list realistic. Shared across the store_test
// integration files (block_grants, tenant isolation, fail-closed).
var gVisibleTypes = []string{"audit-trail", "knowledge", "reference"}

// Fixture UUIDs (design §5.2): S* shared, P* private, W* work; T/Q bridge set;
// U supersedes set; H cap-starvation hub.
const (
	gS1 = "019e0007-0000-7000-9000-000000000011" // shared
	gS2 = "019e0007-0000-7000-9000-000000000012" // shared
	gP1 = "019e0007-0000-7000-9000-000000000021" // private (bridge in S-set)
	gP2 = "019e0007-0000-7000-9000-000000000022" // private
	gW1 = "019e0007-0000-7000-9000-000000000031" // work

	gT1 = "019e0007-0000-7000-9000-000000000041" // shared
	gT2 = "019e0007-0000-7000-9000-000000000042" // shared — reachable ONLY via Q1
	gQ1 = "019e0007-0000-7000-9000-000000000051" // private bridge

	gU1 = "019e0007-0000-7000-9000-000000000061" // shared
	gU2 = "019e0007-0000-7000-9000-000000000062" // shared
	gU3 = "019e0007-0000-7000-9000-000000000063" // shared — only behind supersedes

	gHub  = "019e0007-0000-7000-9000-0000000000a1" // shared starvation hub
	gHub2 = "019e0007-0000-7000-9000-0000000000b1" // shared hit-cap hub

	gMissing = "019e0007-dead-7000-9000-00000000ffff" // never inserted
)

// Tenant read-scope sets per the §5.2 fixture: A reads private+shared,
// B reads work+shared.
var (
	gScopesA = []string{"private", "shared"}
	gScopesB = []string{"work", "shared"}
)

func gInsertBlock(t *testing.T, pool *pgxpool.Pool, id, scope, category string) {
	t.Helper()
	ts := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	_, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks (id, category, title, content, scope, created_at, updated_at)
		 VALUES ($1::uuid, $2, $3, 'graph fixture content', $4, $5, $5)`,
		id, category, "blk-"+id[len(id)-4:], scope, ts,
	)
	if err != nil {
		t.Fatalf("insert block %s: %v", id, err)
	}
}

// gInsertLink seeds one dream link. link.scope is DELIBERATELY 'shared' for
// every link, including those touching private blocks: visibility must gate on
// context_blocks.scope (authoritative), never on context_dream_links.scope —
// a leak through the link scope column would surface in these tests.
func gInsertLink(t *testing.T, pool *pgxpool.Pool, src, dst, rel string, conf, rawConf float64) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO context_dream_links
			(source_block_id, target_block_id, relationship, confidence, raw_confidence, scope, dream_version)
		 VALUES ($1::uuid, $2::uuid, $3, $4, $5, 'shared', 5)`,
		src, dst, rel, conf, rawConf,
	)
	if err != nil {
		t.Fatalf("insert link %s→%s: %v", src, dst, err)
	}
}

func gEgoParams(focus string) store.EgoParams {
	return store.EgoParams{
		Focus:      focus,
		Hops:       2,
		PerNodeCap: 25,
		Limit:      500,
		EdgeLimit:  4000,
	}
}

func gNodeIDs(res *store.EgoResult) map[string]store.GraphNode {
	m := make(map[string]store.GraphNode, len(res.Nodes))
	for _, n := range res.Nodes {
		m[n.ID] = n
	}
	return m
}

// gEdgePairs maps "srcID→dstID" → relationship for assertion readability.
func gEdgePairs(t *testing.T, res *store.EgoResult) map[string]string {
	t.Helper()
	m := make(map[string]string, len(res.Edges))
	for _, e := range res.Edges {
		if e.Src < 0 || e.Src >= len(res.Nodes) || e.Dst < 0 || e.Dst >= len(res.Nodes) {
			t.Fatalf("edge index out of node range: %+v (nodes=%d)", e, len(res.Nodes))
		}
		if e.Rel < 0 || e.Rel >= len(res.Rels) {
			t.Fatalf("edge rel index out of legend range: %+v", e)
		}
		m[res.Nodes[e.Src].ID+"→"+res.Nodes[e.Dst].ID] = res.Rels[e.Rel]
	}
	return m
}

// TestEgoGraph_ScopeFixture covers the §5.2 scope semantics on one container:
// bridge isolation, induced-edge construction safety, visible degree,
// 404-oracle store half, supersedes display-not-traversal, category/time
// filters on neighbors.
func TestEgoGraph_ScopeFixture(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// S-set (verbatim §5.2): S1,S2 shared · P1,P2 private · W1 work.
	gInsertBlock(t, pool, gS1, "shared", "graphtest")
	gInsertBlock(t, pool, gS2, "shared", "graphtest")
	gInsertBlock(t, pool, gP1, "private", "graphtest")
	gInsertBlock(t, pool, gP2, "private", "graphtest2") // distinct category for the filter subtest
	gInsertBlock(t, pool, gW1, "work", "graphtest")
	gInsertLink(t, pool, gS1, gP1, "topical", 0.9, 0.9)
	gInsertLink(t, pool, gP1, gS2, "topical", 0.9, 0.9)
	gInsertLink(t, pool, gS1, gS2, "topical", 0.8, 0.8)
	gInsertLink(t, pool, gS1, gW1, "topical", 0.7, 0.7)
	gInsertLink(t, pool, gS1, gP2, "topical", 0.6, 0.6)

	// T-set: bridge WITHOUT direct edge — T2 is reachable only through the
	// private Q1. The critical leak fixture (§5.2.1).
	gInsertBlock(t, pool, gT1, "shared", "graphtest")
	gInsertBlock(t, pool, gT2, "shared", "graphtest")
	gInsertBlock(t, pool, gQ1, "private", "graphtest")
	gInsertLink(t, pool, gT1, gQ1, "topical", 0.9, 0.9)
	gInsertLink(t, pool, gQ1, gT2, "topical", 0.9, 0.9)

	// U-set: supersedes display vs traversal. U3 sits ONLY behind a
	// supersedes edge.
	gInsertBlock(t, pool, gU1, "shared", "graphtest")
	gInsertBlock(t, pool, gU2, "shared", "graphtest")
	gInsertBlock(t, pool, gU3, "shared", "graphtest")
	gInsertLink(t, pool, gU1, gU2, "topical", 0.8, 0.8)
	gInsertLink(t, pool, gU2, gU1, "supersedes", 0.9, 0.9)
	gInsertLink(t, pool, gU2, gU3, "supersedes", 0.9, 0.9)

	// NEGATIVE PROBE §5.2.1 (bridge leak): as B, the 2-hop ego of T1 must be
	// {T1} alone. With visibility only as a FINAL filter (instead of inside
	// every hop leg) the traversal walks THROUGH the invisible Q1 and delivers
	// the shared T2 — leaking the existence of a private link chain. The
	// rot-beweis removes the leg predicates and post-filters (see report).
	t.Run("BridgeNoLeak", func(t *testing.T) {
		res, err := store.EgoGraph(ctx, pool, gEgoParams(gT1), gScopesB, nil, gVisibleTypes)
		if err != nil {
			t.Fatalf("ego(T1) as B: %v", err)
		}
		nodes := gNodeIDs(res)
		if _, ok := nodes[gQ1]; ok {
			t.Error("LEAK: private bridge Q1 delivered to tenant B")
		}
		if _, ok := nodes[gT2]; ok {
			t.Error("LEAK: T2 is only reachable via the private bridge Q1 — must not be delivered to B")
		}
		if len(res.Nodes) != 1 || res.Nodes[0].ID != gT1 {
			t.Errorf("ego(T1) as B: want exactly [T1], got %d nodes", len(res.Nodes))
		}

		// Sanity (A sees the full chain): focus hop 0, Q1 hop 1, T2 hop 2.
		resA, err := store.EgoGraph(ctx, pool, gEgoParams(gT1), gScopesA, nil, gVisibleTypes)
		if err != nil {
			t.Fatalf("ego(T1) as A: %v", err)
		}
		nodesA := gNodeIDs(resA)
		if len(resA.Nodes) != 3 {
			t.Errorf("ego(T1) as A: want 3 nodes, got %d", len(resA.Nodes))
		}
		if nodesA[gQ1].Hop != 1 || nodesA[gT2].Hop != 2 {
			t.Errorf("hop stamps wrong: Q1=%d T2=%d", nodesA[gQ1].Hop, nodesA[gT2].Hop)
		}
	})

	// §5.2.2: induced edges as B contain no private endpoint (per
	// construction — this test guards against refactors).
	t.Run("InducedEdgesNoForeignEndpoint", func(t *testing.T) {
		res, err := store.EgoGraph(ctx, pool, gEgoParams(gS1), gScopesB, nil, gVisibleTypes)
		if err != nil {
			t.Fatalf("ego(S1) as B: %v", err)
		}
		nodes := gNodeIDs(res)
		for _, bad := range []string{gP1, gP2} {
			if _, ok := nodes[bad]; ok {
				t.Errorf("LEAK: private node %s delivered to B", bad)
			}
		}
		if len(res.Nodes) != 3 { // S1 + S2 + W1
			t.Errorf("ego(S1) as B: want 3 nodes, got %d", len(res.Nodes))
		}
		edges := gEdgePairs(t, res)
		if len(edges) != 2 {
			t.Errorf("want exactly 2 induced edges (S1→S2, S1→W1), got %v", edges)
		}
		if edges[gS1+"→"+gS2] != "topical" || edges[gS1+"→"+gW1] != "topical" {
			t.Errorf("induced edges wrong: %v", edges)
		}
	})

	// §5.2.3 NEGATIVE PROBE (degree leak): the degree badge counts only
	// scope-visible neighbors. Raw count of S1 would be 4 — leaking the
	// existence of private links on a shared block.
	t.Run("DegreeVisibleOnly", func(t *testing.T) {
		resB, err := store.EgoGraph(ctx, pool, gEgoParams(gS1), gScopesB, nil, gVisibleTypes)
		if err != nil {
			t.Fatalf("ego(S1) as B: %v", err)
		}
		if d := gNodeIDs(resB)[gS1].Degree; d != 2 {
			t.Errorf("degree(S1) as B = %d, want 2 (S2+W1; raw count 4 would be a leak)", d)
		}

		resA, err := store.EgoGraph(ctx, pool, gEgoParams(gS1), gScopesA, nil, gVisibleTypes)
		if err != nil {
			t.Fatalf("ego(S1) as A: %v", err)
		}
		if d := gNodeIDs(resA)[gS1].Degree; d != 3 {
			t.Errorf("degree(S1) as A = %d, want 3 (P1,P2,S2; W1 is work-scope)", d)
		}
	})

	// §5.2.4 store half of the existence oracle: invisible and nonexistent
	// focus collapse into the SAME error value.
	t.Run("NotVisibleOracle", func(t *testing.T) {
		_, errInvisible := store.EgoGraph(ctx, pool, gEgoParams(gP1), gScopesB, nil, gVisibleTypes)
		if !errors.Is(errInvisible, store.ErrNotVisible) {
			t.Errorf("ego(P1) as B: want ErrNotVisible, got %v", errInvisible)
		}
		_, errMissing := store.EgoGraph(ctx, pool, gEgoParams(gMissing), gScopesB, nil, gVisibleTypes)
		if !errors.Is(errMissing, store.ErrNotVisible) {
			t.Errorf("ego(missing) as B: want ErrNotVisible, got %v", errMissing)
		}
	})

	// §5.2.6: supersedes is never traversed (U3 absent) but IS displayed as
	// an induced edge between loaded nodes; link_class filters hide it.
	t.Run("SupersedesDisplayNotTraversal", func(t *testing.T) {
		res, err := store.EgoGraph(ctx, pool, gEgoParams(gU1), gScopesB, nil, gVisibleTypes)
		if err != nil {
			t.Fatalf("ego(U1): %v", err)
		}
		nodes := gNodeIDs(res)
		if _, ok := nodes[gU3]; ok {
			t.Error("U3 is only behind a supersedes edge — supersedes must never be traversed")
		}
		if len(res.Nodes) != 2 {
			t.Errorf("want 2 nodes (U1,U2), got %d", len(res.Nodes))
		}
		edges := gEdgePairs(t, res)
		if edges[gU1+"→"+gU2] != "topical" {
			t.Errorf("missing topical U1→U2: %v", edges)
		}
		if edges[gU2+"→"+gU1] != "supersedes" {
			t.Errorf("supersedes U2→U1 must appear as induced display edge: %v", edges)
		}

		// link_class=topical hides the supersedes edge.
		p := gEgoParams(gU1)
		p.LinkClasses = []string{"topical"}
		res, err = store.EgoGraph(ctx, pool, p, gScopesB, nil, gVisibleTypes)
		if err != nil {
			t.Fatalf("ego(U1) topical-only: %v", err)
		}
		edges = gEdgePairs(t, res)
		if len(edges) != 1 || edges[gU1+"→"+gU2] != "topical" {
			t.Errorf("link_class=topical: want only U1→U2 topical, got %v", edges)
		}

		// link_class=supersedes: traversal set empty → focus only, no
		// self-edges possible.
		p.LinkClasses = []string{"supersedes"}
		res, err = store.EgoGraph(ctx, pool, p, gScopesB, nil, gVisibleTypes)
		if err != nil {
			t.Fatalf("ego(U1) supersedes-only: %v", err)
		}
		if len(res.Nodes) != 1 || len(res.Edges) != 0 {
			t.Errorf("supersedes-only: want 1 node / 0 edges, got %d/%d", len(res.Nodes), len(res.Edges))
		}
	})

	// Neighbor-side category filter sits INSIDE the cap legs; the focus is
	// always delivered regardless of its own category.
	t.Run("CategoryFilter", func(t *testing.T) {
		p := gEgoParams(gS1)
		p.Hops = 1
		p.Categories = []string{"graphtest"}
		res, err := store.EgoGraph(ctx, pool, p, gScopesA, nil, gVisibleTypes)
		if err != nil {
			t.Fatalf("ego(S1) category-filtered: %v", err)
		}
		nodes := gNodeIDs(res)
		if _, ok := nodes[gP2]; ok {
			t.Error("P2 (category graphtest2) must be filtered out")
		}
		if len(res.Nodes) != 3 { // S1 + P1 + S2
			t.Errorf("want 3 nodes, got %d", len(res.Nodes))
		}
	})

	// created_before excludes all fixture neighbors (created 2026-03-01) —
	// only the focus remains.
	t.Run("CreatedWindow", func(t *testing.T) {
		p := gEgoParams(gS1)
		cutoff := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
		p.CreatedBefore = &cutoff
		res, err := store.EgoGraph(ctx, pool, p, gScopesA, nil, gVisibleTypes)
		if err != nil {
			t.Fatalf("ego(S1) created-window: %v", err)
		}
		if len(res.Nodes) != 1 || res.Nodes[0].ID != gS1 {
			t.Errorf("want only the focus, got %d nodes", len(res.Nodes))
		}
	})
}

// TestEgoGraph_CapAndBudgets covers §5.2.5/§5.2.7 and the budget mechanics on
// one container: cap starvation (the OpSec-critical slot assignment), hub cap
// with degree badge math, node-budget truncation order, min_confidence gate,
// edge-budget truncation and the degree hit cap (201 = "200+").
func TestEgoGraph_CapAndBudgets(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Starvation hub H (shared), 30 edges: 25 → private blocks with the
	// HIGHEST raw_confidence (invisible to B), 5 → shared blocks with low
	// raw_confidence. §5.2.7 verbatim.
	gInsertBlock(t, pool, gHub, "shared", "graphcap")
	pn := make([]string, 25)
	sn := make([]string, 5)
	for i := 0; i < 25; i++ {
		pn[i] = fmt.Sprintf("019e0007-0000-7000-9000-0000000001%02x", i+1)
		gInsertBlock(t, pool, pn[i], "private", "graphcap")
		// Weighted confidence strictly descending PN1 > PN2 > … (truncation
		// order); raw_confidence 0.95 — the top-25 slots by raw order.
		gInsertLink(t, pool, gHub, pn[i], "topical", 0.90-float64(i)*0.01, 0.95)
	}
	for i := 0; i < 5; i++ {
		sn[i] = fmt.Sprintf("019e0007-0000-7000-9000-0000000002%02x", i+1)
		gInsertBlock(t, pool, sn[i], "shared", "graphcap")
		gInsertLink(t, pool, gHub, sn[i], "topical", 0.50-float64(i)*0.01, 0.50)
	}

	// NEGATIVE PROBE §5.2.7 (cap starvation / counting channel): cap slots are
	// granted ONLY to visible, filter-passing edges. Were the per-node LIMIT
	// applied before the visibility join (fetchNeighbors-style), B's 25 slots
	// would all be eaten by the invisible high-raw-conf private edges → 0
	// neighbors despite 5 visible ones, and per_node_cap variation would
	// become a probe for foreign private link counts.
	t.Run("CapStarvation", func(t *testing.T) {
		p := gEgoParams(gHub)
		p.Hops = 1
		res, err := store.EgoGraph(ctx, pool, p, gScopesB, nil, gVisibleTypes)
		if err != nil {
			t.Fatalf("ego(H) as B: %v", err)
		}
		if got := len(res.Nodes) - 1; got != 5 {
			t.Errorf("ego(H, cap=25) as B: want exactly 5 neighbors (not 0 — starvation; not >5), got %d", got)
		}
		if d := gNodeIDs(res)[gHub].Degree; d != 5 {
			t.Errorf("degree(H) as B = %d, want 5", d)
		}
	})

	// §5.2.5 hub cap: as A all 30 neighbors are visible; cap=25 delivers the
	// top-25 by raw_confidence (the private ones), degree says 30 → client
	// badge math "5 more".
	t.Run("HubCap", func(t *testing.T) {
		p := gEgoParams(gHub)
		p.Hops = 1
		res, err := store.EgoGraph(ctx, pool, p, gScopesA, nil, gVisibleTypes)
		if err != nil {
			t.Fatalf("ego(H) as A: %v", err)
		}
		if got := len(res.Nodes) - 1; got != 25 {
			t.Errorf("ego(H, cap=25) as A: want 25 neighbors, got %d", got)
		}
		if d := gNodeIDs(res)[gHub].Degree; d != 30 {
			t.Errorf("degree(H) as A = %d, want 30", d)
		}
	})

	// Node-budget truncation: limit=10 keeps the focus + top-9 neighbors by
	// weighted confidence DESC (PN1..PN9), flags truncated.
	t.Run("NodeBudgetTruncation", func(t *testing.T) {
		p := gEgoParams(gHub)
		p.Hops = 1
		p.PerNodeCap = 30
		p.Limit = 10
		res, err := store.EgoGraph(ctx, pool, p, gScopesA, nil, gVisibleTypes)
		if err != nil {
			t.Fatalf("ego(H) limit=10: %v", err)
		}
		if len(res.Nodes) != 10 {
			t.Fatalf("want 10 nodes, got %d", len(res.Nodes))
		}
		if !res.Truncated {
			t.Error("node budget cut the hop short — Truncated must be true")
		}
		for i := 1; i < len(res.Nodes); i++ {
			if want := pn[i-1]; res.Nodes[i].ID != want {
				t.Errorf("nodes[%d] = %s, want %s (confidence-DESC truncation order)", i, res.Nodes[i].ID, want)
			}
		}
	})

	// min_confidence gates on the WEIGHTED confidence: 0.895 keeps only PN1
	// (0.90) of the hub's neighbors.
	t.Run("MinConfidenceGate", func(t *testing.T) {
		p := gEgoParams(gHub)
		p.Hops = 1
		p.PerNodeCap = 30
		p.MinConfidence = 0.895
		res, err := store.EgoGraph(ctx, pool, p, gScopesA, nil, gVisibleTypes)
		if err != nil {
			t.Fatalf("ego(H) minconf: %v", err)
		}
		if len(res.Nodes) != 2 || res.Nodes[1].ID != pn[0] {
			t.Errorf("minconf 0.895: want focus+PN1, got %d nodes", len(res.Nodes))
		}
	})

	// Edge budget: the induced fetch keeps the strongest edge_limit edges and
	// flags truncation exactly (one-extra-row detection).
	t.Run("EdgeBudgetTruncation", func(t *testing.T) {
		p := gEgoParams(gHub)
		p.Hops = 1
		p.PerNodeCap = 30
		p.EdgeLimit = 10
		res, err := store.EgoGraph(ctx, pool, p, gScopesA, nil, gVisibleTypes)
		if err != nil {
			t.Fatalf("ego(H) edge_limit=10: %v", err)
		}
		if len(res.Edges) != 10 {
			t.Errorf("want 10 edges, got %d", len(res.Edges))
		}
		if !res.Truncated {
			t.Error("edge budget capped the output — Truncated must be true")
		}
	})

	// Degree hit cap: a hub with 250 visible neighbors reports exactly
	// DegreeHitCap (201) — rendered "200+" client-side.
	t.Run("DegreeHitCap", func(t *testing.T) {
		gInsertBlock(t, pool, gHub2, "shared", "graphcap")
		_, err := pool.Exec(ctx, `
			WITH nb AS (
			    INSERT INTO context_blocks (category, title, content, scope)
			    SELECT 'graphcap', 'hitcap-'||i, 'x', 'shared' FROM generate_series(1, 250) i
			    RETURNING id
			)
			INSERT INTO context_dream_links
				(source_block_id, target_block_id, relationship, confidence, raw_confidence, scope, dream_version)
			SELECT $1::uuid, nb.id, 'topical', 0.5, 0.5, 'shared', 5 FROM nb`,
			gHub2,
		)
		if err != nil {
			t.Fatalf("seed hit-cap hub: %v", err)
		}

		p := gEgoParams(gHub2)
		p.Hops = 1
		p.PerNodeCap = 100
		res, err := store.EgoGraph(ctx, pool, p, gScopesB, nil, gVisibleTypes)
		if err != nil {
			t.Fatalf("ego(H2): %v", err)
		}
		if got := len(res.Nodes) - 1; got != 100 {
			t.Errorf("cap=100: want 100 neighbors, got %d", got)
		}
		if d := gNodeIDs(res)[gHub2].Degree; d != store.DegreeHitCap {
			t.Errorf("degree(H2) = %d, want %d (hit cap, \"200+\")", d, store.DegreeHitCap)
		}
	})
}
