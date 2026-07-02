//go:build integration

// Integration test for Multi-Tenant wave T41 (Achse 07, design/07 §4.5/§5.7/§8),
// rrf side: the GraphExpand SEED-side grant filter. T40a (8379fa2) carries the
// block-grant OR-arm on the NEIGHBOR side of fetchNeighbors (rrf/graph.go:373) and
// left the seed UNGATED with the explicit marker "Seed $1 stays UNGATED — the
// seed-side grant/scope filter is T41, not T40a" (rrf/graph.go:326). T41 closes
// that seam: a grant-only result (scope NOT in readScopes, visible only via
// grantedBlockIDs) is a LEAF — it is never used as a graph-expand seed.
//
// Two seed sources on the rrf side:
//   - C1 INITIAL seed selection (GraphExpand seedIDs loop): a grant-only result is
//     skipped (continue) and never seeds fetchNeighbors. NOTE: in the live RRF
//     pipeline a grant block becomes an RRF result only with the still-unbuilt
//     T40b, so this is no-op IN PRODUCTION today; it is fully testable here by
//     passing results=[B] directly (the public GraphExpand takes results as an
//     argument).
//   - C2 nextSeedIDs at HopDepth>=2: a hop-1 neighbour that is grant-only (it
//     became visible via the T40a NEIGHBOR-side OR-arm) is skipped from the next
//     seed set. This path is REAL today (T40a already injects grant neighbours).
//
// W9 trap: a foreign-scope UN-granted neighbour D of B never passes the
// fetchNeighbors visibility predicate, so it is absent with OR without the fix — a
// test on it is vacuous. The observable effect uses an IN-SCOPE D reachable only
// through the grant block B: WITHOUT the fix B seeds the hop → D is injected; WITH
// the fix B never seeds → D is not injected via B.
//
// Reuses t16hCfg/t16hBlock/the link-insert shape from graph_isolation_integration_test.go
// (same package rrf_test) — NOT redeclared.
//
//	go test -tags=integration ./internal/rrf/ -run TestGraphExpandT41 -count=1 -v
package rrf_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/testdb"
)

// t41Link seeds one dream link with an explicit link scope (the link scope is the
// promotion lever and must never gate — gating is on context_blocks.scope).
func t41Link(t *testing.T, pool *pgxpool.Pool, src, dst, rel string, conf, rawConf float64, linkScope string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_dream_links
			(source_block_id, target_block_id, relationship, confidence, raw_confidence, scope, dream_version)
		 VALUES ($1::uuid,$2::uuid,$3,$4,$5,$6,5)`,
		src, dst, rel, conf, rawConf, linkScope); err != nil {
		t.Fatalf("insert link %s→%s: %v", src, dst, err)
	}
}

func t41Present(out []rrf.SearchResult, id string) bool {
	for _, r := range out {
		if r.ID == id {
			return true
		}
	}
	return false
}

// TestGraphExpandT41_SeedFilter_Integration pins BOTH seed sources of GraphExpand.
func TestGraphExpandT41_SeedFilter_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const homeScope = "t41r-home" // grantee readScopes
	const foreign = "t41r-foreign"

	// --- C1: INITIAL seed selection ---
	// B = grant-only result (foreign scope), D = in-scope, reachable only via B.
	// results=[B] is constructed directly (no-op in the live pipeline pre-T40b, but
	// the unit of behaviour T41 owns is exactly this filter — see file header).
	t.Run("C1_InitialSeed_GrantOnly_IsNotSeeded", func(t *testing.T) {
		t16hBlock(t, pool, t41uuid("b1"), foreign)   // grant block B (foreign scope)
		t16hBlock(t, pool, t41uuid("d1"), homeScope) // in-scope neighbour D
		t41Link(t, pool, t41uuid("b1"), t41uuid("d1"), "topical", 0.9, 0.9, homeScope)

		cfg := t16hCfg() // directed, hop 1
		// results=[B] only. B is grant-only: its scope (foreign) is NOT in readScopes;
		// it is visible solely via grantedBlockIDs.
		results := []rrf.SearchResult{{ID: t41uuid("b1"), Title: "B", RRFScore: 1.0, Scope: foreign}}
		grants := []string{t41uuid("b1")}

		out, err := rrf.GraphExpand(ctx, pool, results, []string{homeScope}, grants, []string{"knowledge"}, cfg)
		if err != nil {
			t.Fatalf("GraphExpand (C1): %v", err)
		}
		// MIT Fix: B is not seeded → D is never fetched/injected over B.
		if t41Present(out, t41uuid("d1")) {
			t.Errorf("LEAK: in-scope D injected via grant-only SEED B — grant block must not seed the graph (T41 C1)")
		}
	})

	// --- C2: nextSeedIDs at HopDepth>=2 ---
	// S = in-scope seed (real RRF result). B = grant-only hop-1 neighbour (foreign
	// scope, granted → visible via T40a neighbour OR-arm). D = in-scope, reachable
	// only via B at hop 2. WITHOUT the C2 fix B enters nextSeedIDs → hop-2 fetches
	// B->D → D injected. WITH the fix B is skipped from nextSeedIDs → D not injected.
	t.Run("C2_NextSeed_GrantOnlyNeighbor_IsNotReSeeded", func(t *testing.T) {
		t16hBlock(t, pool, t41uuid("s2"), homeScope) // in-scope seed S
		t16hBlock(t, pool, t41uuid("b2"), foreign)   // grant-only hop-1 neighbour B
		t16hBlock(t, pool, t41uuid("d2"), homeScope) // in-scope hop-2, only via B
		t41Link(t, pool, t41uuid("s2"), t41uuid("b2"), "topical", 0.9, 0.9, homeScope)
		t41Link(t, pool, t41uuid("b2"), t41uuid("d2"), "topical", 0.9, 0.9, homeScope)

		cfg := t16hCfg()
		cfg.HopDepth = 2 // exercise nextSeedIDs

		results := []rrf.SearchResult{{ID: t41uuid("s2"), Title: "S", RRFScore: 1.0, Scope: homeScope}}
		grants := []string{t41uuid("b2")}

		out, err := rrf.GraphExpand(ctx, pool, results, []string{homeScope}, grants, []string{"knowledge"}, cfg)
		if err != nil {
			t.Fatalf("GraphExpand (C2): %v", err)
		}
		// Positive control: B (grant-only) IS injected at hop 1 (T40a neighbour arm)
		// — proves the hop ran and the corpus is wired, so the D-absent assertion is
		// not vacuous.
		if !t41Present(out, t41uuid("b2")) {
			t.Fatal("positive control failed: grant neighbour B not injected at hop 1 — T40a arm/corpus broken, the D probe would be vacuous")
		}
		// MIT Fix: B does not re-seed → hop-2 D is not injected via B.
		if t41Present(out, t41uuid("d2")) {
			t.Errorf("LEAK: in-scope D injected via grant-only hop-1 neighbour B as a hop-2 seed — grant block must not re-seed (T41 C2)")
		}
	})
}

// t41uuid maps a short tag to a stable distinct UUID in the T41 rrf test space.
func t41uuid(tag string) string {
	m := map[string]string{
		"b1": "019e4100-0000-7000-9000-0000000000b1",
		"d1": "019e4100-0000-7000-9000-0000000000d1",
		"s2": "019e4100-0000-7000-9000-000000000252",
		"b2": "019e4100-0000-7000-9000-0000000000b2",
		"d2": "019e4100-0000-7000-9000-0000000000d2",
	}
	return m[tag]
}
