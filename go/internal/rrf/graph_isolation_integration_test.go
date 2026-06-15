//go:build integration

// Multi-Tenant wave T16a (02-V3, isolation arm): rrf-side graph-hop scope gate.
//
// design/02 §5.3 (graph-hop over the tenant boundary) is enforced in TWO distinct
// queries: store.hopNeighbors (EgoGraph) and rrf.fetchNeighbors, the retrieval
// graph-expand hop reached through GraphExpand. EgoGraph's gate has a real-DB
// cross-scope probe (store/graph_integration_test.go::TestEgoGraph_ScopeFixture);
// the rrf path had only UNIT tests (nil/closed pool — disabled, empty, fail-open
// in graph_test.go). They never exercise the `cb.scope = ANY($2)` gate
// (graph.go:365) against real rows. This probes it via the exported GraphExpand
// (the actual retrieval hop) against a testcontainer.
//
// One seed, ONE neighbor, ONE link whose OWN scope is the seed's visible scope
// (the promotion lever). The neighbor is delivered iff its BLOCK scope is in
// readScopes, NEVER because the visible LINK scope is — visibility.go:20-22. Run
// twice over the SAME corpus, varying only readScopes: with the block scope
// visible the neighbor is injected (the positive control — proves the hop ran,
// so the absent-case is not vacuous); with only the link scope visible it must be
// absent. The two-run shape isolates the scope gate from fusion/injection scoring
// without needing the unexported helper. Mutation (a) — gating on the link scope
// instead of context_blocks.scope — was proven red on a copy (commit body).
package rrf_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	t16hSeed     = "019e16f0-0000-7000-9000-000000000001" // seed (scope visible)
	t16hNeighbor = "019e16f0-0000-7000-9000-000000000002" // neighbor (scope foreign)
)

// t16hCfg mirrors the rrf graph-expand defaults (defaultGraphCfg is unexported):
// directed, MinConfidence 0.75 so a 0.9 link clears the gate, one hop.
func t16hCfg() rrf.GraphConfig {
	return rrf.GraphConfig{
		Enabled:                true,
		Directed:               true,
		HopDepth:               1,
		SeedCount:              5,
		SeedScoreFloor:         0.5,
		PerSeedCap:             3,
		MaxInjected:            10,
		MinConfidence:          0.75,
		MinConfidenceRecurrent: 0.8,
		BoostWeight:            0.20,
		HubDamping:             true,
		WeightTopical:          0.5,
		WeightFactual:          0.9,
		WeightCausal:           0.9,
		WeightRecurrent:        1.0,
		NewPlacementFrac:       0.6,
	}
}

func t16hBlock(t *testing.T, pool *pgxpool.Pool, id, scope string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks (id, category, title, content, scope)
		 VALUES ($1::uuid,'graphtest',$2,'rrf hop fixture',$3)`,
		id, "blk-"+id[len(id)-4:], scope); err != nil {
		t.Fatalf("insert block %s: %v", id, err)
	}
}

func TestGraphExpand_CrossScopeHopIsolation_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const seedScope, neighborScope = "t16h-seed", "t16h-foreign"
	t16hBlock(t, pool, t16hSeed, seedScope)
	t16hBlock(t, pool, t16hNeighbor, neighborScope)
	// The LINK row carries the SEED's (visible) scope — the promotion lever. The
	// gate must key on the target BLOCK scope (neighborScope), not this link scope.
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_dream_links
			(source_block_id, target_block_id, relationship, confidence, raw_confidence, scope, dream_version)
		 VALUES ($1::uuid,$2::uuid,'topical',0.9,0.9,$3,5)`,
		t16hSeed, t16hNeighbor, seedScope); err != nil {
		t.Fatalf("insert link: %v", err)
	}

	cfg := t16hCfg()
	seed := []rrf.SearchResult{{ID: t16hSeed, Title: "seed", RRFScore: 1.0, Scope: seedScope}}

	present := func(out []rrf.SearchResult, id string) bool {
		for _, r := range out {
			if r.ID == id {
				return true
			}
		}
		return false
	}

	// Positive control: the neighbor's BLOCK scope is visible → the hop hydrates
	// and injects it. Proves the traversal actually ran on this corpus.
	visible, err := rrf.GraphExpand(ctx, pool, seed, []string{seedScope, neighborScope}, cfg)
	if err != nil {
		t.Fatalf("GraphExpand (block scope visible): %v", err)
	}
	if !present(visible, t16hNeighbor) {
		t.Fatal("positive control failed: neighbor not injected when its block scope is visible — the hop did not run, the leak probe below would be vacuous")
	}

	// Leak probe: only the LINK scope is visible, the neighbor's BLOCK scope is
	// not → the neighbor must NOT cross the boundary, despite the visible link.
	leak, err := rrf.GraphExpand(ctx, pool, seed, []string{seedScope}, cfg)
	if err != nil {
		t.Fatalf("GraphExpand (only link scope visible): %v", err)
	}
	if present(leak, t16hNeighbor) {
		t.Error("LEAK: foreign-block neighbor crossed the rrf graph-hop — gate keyed on the visible link scope, not context_blocks.scope")
	}
}
