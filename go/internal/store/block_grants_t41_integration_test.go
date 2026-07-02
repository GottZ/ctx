//go:build integration

// Integration test for Multi-Tenant wave T41 (Achse 07, design/07 §4.5/§5.7/§8):
// the GRAPH-BRIDGE LEAF PROTECTION for block-level grants. T40a (8379fa2) made a
// grant-only block VISIBLE in the graph (it appears in the node set and in induced
// edges to other visible nodes via the per-leg VisibilityPredicate OR-arm). T41
// makes it a LEAF: visible, but NEVER a hop seed. A grant-only block is one whose
// own scope is NOT in readScopes — it reached the node set ONLY via grantedBlockIDs.
//
// EgoGraph has two seed sources, both pinned here:
//   - Hop-0 focus (G4(ii)): a grant-only FOCUS must not traverse — its frontier is
//     initialised empty, so it stays an isolated single visible node.
//   - Hop>=1 frontier (G4(i)): a grant-only block reached as a hop-1 neighbour stays
//     in the node set (visible) but is NOT added to the next frontier — the
//     traversal does not continue THROUGH it.
//
// The W9 trap (why the naive test is vacuous): a foreign-scope, UN-granted neighbour
// of a grant-block never matches the per-leg VisibilityPredicate and is absent WITH
// OR WITHOUT the fix — a test on it is green without the fix and proves nothing. The
// OBSERVABLE effect requires an IN-SCOPE block D reachable ONLY through the grant
// block B: D is legitimately visible, so it is sucked in over the bridge B WITHOUT
// the fix and vanishes WITH it.
//
// Reuses t40Tenant/t40MapScope/t40Block/t40Grant (block_grants_t40a_integration_test.go),
// gInsertBlock/gInsertLink/gEgoParams/gNodeIDs (graph_integration_test.go) — same
// package store_test, NOT redeclared.
//
//	go test -tags=integration ./internal/store/ -run TestBlockGrantsT41 -count=1 -v
package store_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestBlockGrantsT41_GraphBridgeLeaf_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Owner tenant owns scope C (foreign blocks live here); grantee tenant A owns
	// scope A (home_scope = readScopes). D is an A-scope block: legitimately visible.
	const scopeC = "t41-c" // owner / foreign scope (grant block B lives here)
	const scopeA = "t41-a" // grantee home scope (readScopes)
	owner := t40Tenant(t, pool, "t41-owner")
	grantee := t40Tenant(t, pool, "t41-grantee")
	t40MapScope(t, pool, scopeC, owner)
	t40MapScope(t, pool, scopeA, grantee)
	granteeScopes := []string{scopeA}

	// G4(ii) — Hop-0 focus: B = grant-only focus, D = in-scope, reachable only via B.
	// WITHOUT the Hop-0-focus fix the frontier seeds with B → hop 1 traverses B→D →
	// D appears. WITH the fix the frontier is empty → B is an isolated single node.
	t.Run("G4ii_GrantFocus_IsLeaf_DoesNotTraverse", func(t *testing.T) {
		focusB := t40Block(t, pool, scopeC, "t41-focusB-grant", false) // foreign scope
		nodeD := t40Block(t, pool, scopeA, "t41-D-inscope-focus", false)
		// B <-> D (one direction is enough; EgoGraph cap legs traverse both directions).
		gInsertLink(t, pool, focusB, nodeD, "topical", 0.9, 0.9)
		t40Grant(t, pool, focusB, grantee)
		grants := []string{focusB}

		res, err := store.EgoGraph(ctx, pool, gEgoParams(focusB), granteeScopes, grants, gVisibleTypes)
		if err != nil {
			t.Fatalf("EgoGraph(grant focus B): %v", err)
		}
		nodes := gNodeIDs(res)
		if _, ok := nodes[focusB]; !ok {
			t.Fatal("grant focus B must stay VISIBLE as the focus node (T40a)")
		}
		if _, ok := nodes[nodeD]; ok {
			t.Errorf("LEAK: in-scope D reached via grant-only focus B — B must be a LEAF, frontier empty (T41 hop-0)")
		}
		if len(res.Nodes) != 1 {
			t.Errorf("grant focus B must be an isolated single node, got %d nodes: %v", len(res.Nodes), nodeIDsOf(res))
		}
	})

	// G4(i) — Hop>=1 frontier: S = in-scope seed, B = grant-only (reached at hop 1
	// via the grant OR-arm), D = in-scope, reachable ONLY via B. Links S->B, B->D.
	// WITHOUT the frontier fix B enters the next frontier → hop 2 traverses B->D →
	// D appears. WITH the fix B stays a visible hop-1 node but is NOT in the frontier
	// → D never appears. B itself stays present in BOTH cases (it is a visible
	// hop-1 neighbour of S).
	t.Run("G4i_GrantHopNeighbor_IsLeaf_NotInFrontier", func(t *testing.T) {
		seedS := t40Block(t, pool, scopeA, "t41-S-seed", false)      // in-scope focus
		bridgeB := t40Block(t, pool, scopeC, "t41-B-grant-hop", false) // foreign, granted
		nodeD := t40Block(t, pool, scopeA, "t41-D-inscope-hop", false) // in-scope, only via B
		gInsertLink(t, pool, seedS, bridgeB, "topical", 0.9, 0.9)
		gInsertLink(t, pool, bridgeB, nodeD, "topical", 0.9, 0.9)
		t40Grant(t, pool, bridgeB, grantee)
		grants := []string{bridgeB}

		res, err := store.EgoGraph(ctx, pool, gEgoParams(seedS), granteeScopes, grants, gVisibleTypes)
		if err != nil {
			t.Fatalf("EgoGraph(seed S): %v", err)
		}
		nodes := gNodeIDs(res)
		if _, ok := nodes[seedS]; !ok {
			t.Fatal("seed S must be the focus node")
		}
		if _, ok := nodes[bridgeB]; !ok {
			t.Error("grant bridge B must stay VISIBLE as a hop-1 neighbour (T40a) — it is a LEAF, not absent")
		}
		if n, ok := nodes[bridgeB]; ok && n.Hop != 1 {
			t.Errorf("grant bridge B hop = %d, want 1 (visible hop-1 neighbour)", n.Hop)
		}
		if _, ok := nodes[nodeD]; ok {
			t.Errorf("LEAK: in-scope D reached only via grant bridge B — B must not enter the frontier (T41 hop>=1)")
		}
		if len(res.Nodes) != 2 {
			t.Errorf("want exactly 2 nodes (S + leaf B), got %d: %v", len(res.Nodes), nodeIDsOf(res))
		}
	})
}

// nodeIDsOf is a debug helper for the failure messages above.
func nodeIDsOf(res *store.EgoResult) []string {
	out := make([]string, len(res.Nodes))
	for i := range res.Nodes {
		out[i] = res.Nodes[i].ID
	}
	return out
}
