//go:build integration

// Graph-structural wave W04-3 (04-overview-retrieval §4.2b, §7): boost-isolation
// negative gate — the edge-consumer doctrine's mirror for the retrieval path.
//
// docs/architecture.md §"Structural links" (since GA3): context_structural_links
// is a strictly separate data class; the query-path graph boost (GraphExpand /
// fetchNeighbors) reads ONLY context_dream_links. A structural `references` edge
// is a time-neighborhood fact ("listed in the same daily report"), not a semantic
// endorsement — injecting along it would smuggle a second, edge-disguised recency
// channel into retrieval (§4.2b). This test pins that doctrine with TWO asserts:
//
//	(i) Isolation: a block reachable from a seed ONLY via a structural
//	    `references` edge is never injected (ViaGraph provenance over the
//	    result set).
//	(ii) Cap displacement: a dream neighbor with raw_confidence BELOW the
//	    structural class's definitional 1.0 is STILL injected under
//	    PerSeedCap=1. If structural rows ever entered the fetchNeighbors CTE,
//	    ROW_NUMBER() OVER (PARTITION BY seed_id ORDER BY raw_confidence DESC)
//	    (graph.go:410) would rank the 1.0 rows first and displace real dream
//	    edges out of the per-seed cap — an observable degradation even when
//	    the second defense line (typeWeight default 0, graph.go:133-134, and
//	    fuseNeighbors' `if w <= 0 { continue }`, graph.go:531-533, BEFORE the
//	    ViaGraph stamp at graph.go:610) keeps the structural block itself out.
//
// Defense-in-depth makes a naive red probe unprovable (Bau-Chronik ctx 019f480e):
// a temporary structural CTE leg that keeps `'references' AS relationship` stays
// GREEN — typeWeight('references') is 0, so fuseNeighbors drops the edge before
// injection (second line). The probe therefore aliases the relationship onto a
// WEIGHTED dream class (`'factual' AS relationship, 1.0 AS raw_confidence`),
// neutralizing the second line so the FIRST line (dream-only table choice in
// edge_dir) is the one under test. With that leg both asserts go red: B appears
// ViaGraph=true (i) and the 1.0 row displaces C from PerSeedCap=1 (ii).
// Probe reverted — production code byte-identical.
package rrf_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	w043SeedA       = "019f6017-0000-7000-9000-00000000000a" // A: seed (ranks for the query)
	w043StructOnlyB = "019f6017-0000-7000-9000-00000000000b" // B: structural-only neighbor
	w043DreamC      = "019f6017-0000-7000-9000-00000000000c" // C: dream neighbor, low raw_confidence
)

// w043Cfg mirrors the rrf graph-expand defaults (defaultGraphCfg is unexported;
// same shape as the T16a fixture) with ONE deliberate deviation: PerSeedCap=1.
// That cap is the displacement lever for assert (ii) — the seed has exactly one
// legitimate dream edge (C, raw_confidence 0.8), so C fills the cap iff no
// higher-confidence row (structural = definitional 1.0) enters the ranked CTE.
func w043Cfg() rrf.GraphConfig {
	return rrf.GraphConfig{
		Enabled:                true,
		Directed:               true,
		HopDepth:               1,
		SeedCount:              5,
		SeedScoreFloor:         0.5,
		PerSeedCap:             1, // displacement lever — see assert (ii)
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

func w043Block(t *testing.T, pool *pgxpool.Pool, id, title string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks (id, category, title, content, scope)
		 VALUES ($1::uuid,'graphtest',$2,'w043 boost isolation fixture','w043')`,
		id, title); err != nil {
		t.Fatalf("insert block %s: %v", id, err)
	}
}

// TestGraphExpand_StructuralLinkIsolation_Integration is the W04-3 negative
// gate. Fixture stays tiny and deterministic (target system: 1M+ blocks /
// 3M dream edges — the gate proves table choice, not scale): one seed A,
// one structural-only neighbor B (context_structural_links, class
// `references`, NO dream edge), one dream neighbor C (factual, 0.8).
func TestGraphExpand_StructuralLinkIsolation_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	w043Block(t, pool, w043SeedA, "w043 seed A")
	w043Block(t, pool, w043StructOnlyB, "w043 structural-only B")
	w043Block(t, pool, w043DreamC, "w043 dream neighbor C")

	// B hangs off A ONLY via a structural `references` fact edge (M076/M103
	// shape: no confidence column — 1.0 by definition). Direct SQL, matching
	// how forge-sync/report writers persist rows.
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_structural_links
			(source_block_id, target_block_id, link_class, scope, origin)
		 VALUES ($1::uuid,$2::uuid,'references','w043','system')`,
		w043SeedA, w043StructOnlyB); err != nil {
		t.Fatalf("insert structural link A->B: %v", err)
	}

	// C hangs off A via a real dream edge whose raw_confidence (0.8) clears
	// MinConfidence (0.75) but sits BELOW the structural class's definitional
	// 1.0 — the displacement oracle for assert (ii).
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_dream_links
			(source_block_id, target_block_id, relationship, confidence, raw_confidence, scope, dream_version)
		 VALUES ($1::uuid,$2::uuid,'factual',0.8,0.8,'w043',5)`,
		w043SeedA, w043DreamC); err != nil {
		t.Fatalf("insert dream link A->C: %v", err)
	}

	seed := []rrf.SearchResult{{ID: w043SeedA, Title: "w043 seed A", RRFScore: 1.0, Scope: "w043"}}
	out, err := rrf.GraphExpand(ctx, pool, seed, []string{"w043"}, nil, []string{"knowledge"}, w043Cfg())
	if err != nil {
		t.Fatalf("GraphExpand: %v", err)
	}

	var foundB, foundC *rrf.SearchResult
	for i := range out {
		switch out[i].ID {
		case w043StructOnlyB:
			foundB = &out[i]
		case w043DreamC:
			foundC = &out[i]
		}
	}

	// Assert (ii) first — it doubles as the positive control: C injected via
	// its dream edge proves the hop ran on this corpus, so assert (i) below is
	// not vacuously green. If structural rows entered the ranked CTE their 1.0
	// raw_confidence would displace C from PerSeedCap=1 (graph.go:410) — an
	// observable degradation WITHOUT B ever appearing.
	// Non-fatal (t.Errorf) so a breach reports BOTH asserts in one run — the
	// red probe must show (i) and (ii) failing together.
	if foundC == nil {
		t.Errorf("cap displacement: dream neighbor C (raw_confidence 0.8) was not injected under PerSeedCap=1 — either the hop did not run (fixture broken) or a higher-confidence non-dream row consumed the per-seed cap (graph.go:410 ROW_NUMBER ... ORDER BY raw_confidence DESC)")
	} else if !foundC.ViaGraph {
		t.Errorf("dream neighbor C injected without ViaGraph provenance: %+v", *foundC)
	}

	// Assert (i): B is reachable from A ONLY via context_structural_links —
	// it must never cross into the boost result set. ViaGraph provenance over
	// the whole result set: B absent entirely (it was not a native result, so
	// any appearance would be a graph injection).
	if foundB != nil {
		t.Errorf("structural isolation breached: block B (structural `references` only, no dream edge) was injected into the result set (ViaGraph=%v, rel=%q) — fetchNeighbors read context_structural_links (doctrine: dream-only, architecture.md §Structural links)",
			foundB.ViaGraph, foundB.GraphRelationship)
	}
}
