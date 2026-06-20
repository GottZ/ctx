package rrf

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// defaultGraphCfg returns the Wave-1 calibration with the stage enabled, so
// individual tests can tweak single knobs.
func defaultGraphCfg() GraphConfig {
	return GraphConfig{
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

// res is a tiny constructor for a native result with a given id + score.
func res(id string, score float64) SearchResult {
	return SearchResult{ID: id, Title: id, RRFScore: score}
}

// edge constructs a hydrated graph edge (hop 1, degree 1 unless overridden).
func edge(seedID string, seedScore float64, rel, neighborID string, degree int) graphEdge {
	return graphEdge{
		seedID:         seedID,
		seedRRFScore:   seedScore,
		seedRealScore:  seedScore,
		relationship:   rel,
		neighbor:       SearchResult{ID: neighborID, Title: neighborID},
		neighborDegree: degree,
		hopDecay:       1.0,
	}
}

// byID indexes a result slice by id for assertions.
func byID(rs []SearchResult) map[string]SearchResult {
	m := make(map[string]SearchResult, len(rs))
	for _, r := range rs {
		m[r.ID] = r
	}
	return m
}

// TestTypeWeight verifies the type-weight ordering recurrent > causal/factual > topical.
func TestTypeWeight(t *testing.T) {
	cfg := defaultGraphCfg()
	if !(cfg.typeWeight("recurrent") > cfg.typeWeight("causal")) {
		t.Errorf("recurrent (%v) should outweigh causal (%v)", cfg.typeWeight("recurrent"), cfg.typeWeight("causal"))
	}
	if !(cfg.typeWeight("recurrent") > cfg.typeWeight("factual")) {
		t.Errorf("recurrent (%v) should outweigh factual (%v)", cfg.typeWeight("recurrent"), cfg.typeWeight("factual"))
	}
	if !(cfg.typeWeight("causal") > cfg.typeWeight("topical")) {
		t.Errorf("causal (%v) should outweigh topical (%v)", cfg.typeWeight("causal"), cfg.typeWeight("topical"))
	}
	if !(cfg.typeWeight("factual") > cfg.typeWeight("topical")) {
		t.Errorf("factual (%v) should outweigh topical (%v)", cfg.typeWeight("factual"), cfg.typeWeight("topical"))
	}
	if cfg.typeWeight("supersedes") != 0 {
		t.Errorf("supersedes must not be traversed (weight %v, want 0)", cfg.typeWeight("supersedes"))
	}
	if cfg.typeWeight("bogus") != 0 {
		t.Errorf("unknown type weight = %v, want 0", cfg.typeWeight("bogus"))
	}
}

// TestFuseNeighbors_TypeWeightOrdering: at equal seed score / degree, a recurrent
// neighbor must end up ranked above a topical neighbor (higher synthetic score).
func TestFuseNeighbors_TypeWeightOrdering(t *testing.T) {
	cfg := defaultGraphCfg()
	results := []SearchResult{res("S", 1.0)}
	edges := []graphEdge{
		edge("S", 1.0, "recurrent", "Nrec", 1),
		edge("S", 1.0, "topical", "Ntop", 1),
	}
	out := fuseNeighbors(results, edges, cfg)
	m := byID(out)
	if m["Nrec"].RRFScore <= m["Ntop"].RRFScore {
		t.Errorf("recurrent neighbor score %v should exceed topical %v", m["Nrec"].RRFScore, m["Ntop"].RRFScore)
	}
	if !m["Nrec"].ViaGraph || !m["Ntop"].ViaGraph {
		t.Error("graph-introduced neighbors must be marked ViaGraph")
	}
}

// TestFuseNeighbors_NewNeighborCannotOutrankTop1: a NEW graph-only neighbor must
// never be results[0] purely from the graph. Even with maximal accumulation the
// synthetic score = minScore * 0.5 * BoostWeight < the native top hit.
func TestFuseNeighbors_NewNeighborCannotOutrankTop1(t *testing.T) {
	cfg := defaultGraphCfg()
	cfg.HubDamping = false
	results := []SearchResult{res("TOP", 1.0), res("MID", 0.8), res("LOW", 0.5)}
	// Several strong recurrent edges so accumulation caps at 1.0.
	edges := []graphEdge{
		edge("TOP", 1.0, "recurrent", "G", 1),
		edge("MID", 0.8, "recurrent", "G", 1),
		edge("LOW", 0.5, "recurrent", "G", 1),
	}
	out := fuseNeighbors(results, edges, cfg)
	if out[0].ID != "TOP" {
		t.Fatalf("native top hit must remain results[0], got %q (score %v)", out[0].ID, out[0].RRFScore)
	}
	m := byID(out)
	if m["G"].RRFScore >= m["TOP"].RRFScore {
		t.Errorf("graph-only neighbor score %v must be below native top %v", m["G"].RRFScore, m["TOP"].RRFScore)
	}
	// Top-relative ceiling: topScore(1.0) * newPlacementFrac(0.6) * capped accum(1.0)
	// = 0.6 — promotable into the rerank window, but strictly below the native top
	// hit (1.0).
	if got, want := m["G"].RRFScore, 1.0*0.6*1.0; got > want+1e-9 {
		t.Errorf("synthetic score %v exceeds ceiling %v", got, want)
	}
}

// TestFuseNeighbors_DedupReinforcesNotAppends: a neighbor that is ALREADY in
// results is reinforced (score *= 1+...), NOT re-appended, and keeps ViaGraph=false.
func TestFuseNeighbors_DedupReinforcesNotAppends(t *testing.T) {
	cfg := defaultGraphCfg()
	cfg.HubDamping = false
	results := []SearchResult{res("A", 1.0), res("B", 0.6)}
	// Seed A points at B (already present) with a causal edge.
	edges := []graphEdge{edge("A", 1.0, "causal", "B", 1)}
	out := fuseNeighbors(results, edges, cfg)
	if len(out) != len(results) {
		t.Fatalf("present neighbor must not be appended: len %d, want %d", len(out), len(results))
	}
	m := byID(out)
	// injection = 0.9 * 1.0 = 0.9 (capped 1.0). B *= 1 + 0.20*0.9 = 1.18.
	wantB := 0.6 * (1.0 + 0.20*0.9)
	if got := m["B"].RRFScore; got <= 0.6 || abs(got-wantB) > 1e-9 {
		t.Errorf("reinforced B score = %v, want %v", got, wantB)
	}
	if m["B"].ViaGraph {
		t.Error("reinforced native hit must keep ViaGraph=false")
	}
}

// TestFuseNeighbors_MultiSeedAccumCapsAt1: contributions from multiple seeds
// accumulate but the per-neighbor accumulator caps at 1.0 — the reinforced
// factor can never exceed 1 + BoostWeight.
func TestFuseNeighbors_MultiSeedAccumCapsAt1(t *testing.T) {
	cfg := defaultGraphCfg()
	cfg.HubDamping = false
	results := []SearchResult{res("S1", 1.0), res("S2", 1.0), res("P", 0.5)}
	// Two recurrent edges into present P; each injection = 1.0 → sum 2.0 → cap 1.0.
	edges := []graphEdge{
		edge("S1", 1.0, "recurrent", "P", 1),
		edge("S2", 1.0, "recurrent", "P", 1),
	}
	out := fuseNeighbors(results, edges, cfg)
	m := byID(out)
	wantP := 0.5 * (1.0 + 0.20*1.0) // accum capped at 1.0
	if got := m["P"].RRFScore; abs(got-wantP) > 1e-9 {
		t.Errorf("multi-seed reinforced P = %v, want %v (accum must cap at 1.0)", got, wantP)
	}
}

// TestFuseNeighbors_HubDampingLowersHighDegree: at equal type/seed/conf, a
// degree-18 neighbor must rank BELOW a degree-1 neighbor when hub damping is on.
func TestFuseNeighbors_HubDampingLowersHighDegree(t *testing.T) {
	cfg := defaultGraphCfg() // HubDamping=true
	results := []SearchResult{res("S", 1.0)}
	edges := []graphEdge{
		edge("S", 1.0, "topical", "Hub", 18),
		edge("S", 1.0, "topical", "Leaf", 1),
	}
	out := fuseNeighbors(results, edges, cfg)
	m := byID(out)
	if m["Hub"].RRFScore >= m["Leaf"].RRFScore {
		t.Errorf("hub (deg 18) score %v must be below leaf (deg 1) %v under damping", m["Hub"].RRFScore, m["Leaf"].RRFScore)
	}
	// And with damping OFF they should tie.
	cfg.HubDamping = false
	out2 := fuseNeighbors(results, edges, cfg)
	m2 := byID(out2)
	if abs(m2["Hub"].RRFScore-m2["Leaf"].RRFScore) > 1e-12 {
		t.Errorf("without damping hub %v and leaf %v should tie", m2["Hub"].RRFScore, m2["Leaf"].RRFScore)
	}
}

// TestFuseNeighbors_MaxInjectedCapsNewAppends: the global cap limits the number
// of NEW appends to MaxInjected, keeping the highest-injection neighbors.
func TestFuseNeighbors_MaxInjectedCapsNewAppends(t *testing.T) {
	cfg := defaultGraphCfg()
	cfg.HubDamping = false
	cfg.MaxInjected = 2
	results := []SearchResult{res("S", 1.0)}
	// Four new neighbors with descending strength via relationship weight.
	edges := []graphEdge{
		edge("S", 1.0, "recurrent", "N1", 1), // 1.0
		edge("S", 1.0, "causal", "N2", 1),    // 0.9
		edge("S", 1.0, "factual", "N3", 1),   // 0.9
		edge("S", 1.0, "topical", "N4", 1),   // 0.5 (weakest → dropped)
	}
	out := fuseNeighbors(results, edges, cfg)
	newCount := 0
	for _, r := range out {
		if r.ViaGraph {
			newCount++
		}
	}
	if newCount != 2 {
		t.Fatalf("MaxInjected=2 should yield 2 new appends, got %d", newCount)
	}
	m := byID(out)
	if _, ok := m["N1"]; !ok {
		t.Error("strongest neighbor N1 must survive the cap")
	}
	if _, ok := m["N4"]; ok {
		t.Error("weakest neighbor N4 (topical) must be dropped by the cap")
	}
}

// TestFuseNeighbors_PerSeedCapTrim: the per-seed cap is enforced in SQL via
// ROW_NUMBER, but fuseNeighbors must honor whatever edges it receives. This test
// documents that fusion itself does not silently inflate beyond the edges given:
// feeding 2 edges yields exactly the 2 corresponding neighbors.
func TestFuseNeighbors_PerSeedCapTrim(t *testing.T) {
	cfg := defaultGraphCfg()
	cfg.HubDamping = false
	results := []SearchResult{res("S", 1.0)}
	// Simulate PerSeedCap=2 already applied upstream: only 2 edges reach fusion.
	edges := []graphEdge{
		edge("S", 1.0, "recurrent", "N1", 1),
		edge("S", 1.0, "causal", "N2", 1),
	}
	out := fuseNeighbors(results, edges, cfg)
	newCount := 0
	for _, r := range out {
		if r.ViaGraph {
			newCount++
		}
	}
	if newCount != 2 {
		t.Errorf("expected exactly 2 graph neighbors for 2 capped edges, got %d", newCount)
	}
}

// TestFuseNeighbors_ReSortedDescending: output must be sorted by RRFScore desc.
func TestFuseNeighbors_ReSortedDescending(t *testing.T) {
	cfg := defaultGraphCfg()
	results := []SearchResult{res("A", 1.0), res("B", 0.7)}
	edges := []graphEdge{edge("A", 1.0, "recurrent", "C", 1)}
	out := fuseNeighbors(results, edges, cfg)
	for i := 1; i < len(out); i++ {
		if out[i-1].RRFScore < out[i].RRFScore {
			t.Errorf("output not sorted desc at %d: %v < %v", i, out[i-1].RRFScore, out[i].RRFScore)
		}
	}
}

// TestFuseNeighbors_EmptyInputs: empty results or empty edges return the input
// unchanged (no panic, no append).
func TestFuseNeighbors_EmptyInputs(t *testing.T) {
	cfg := defaultGraphCfg()
	if got := fuseNeighbors(nil, []graphEdge{edge("S", 1, "topical", "N", 1)}, cfg); got != nil {
		t.Errorf("nil results should return nil, got %v", got)
	}
	results := []SearchResult{res("A", 1.0)}
	out := fuseNeighbors(results, nil, cfg)
	if len(out) != 1 || out[0].ID != "A" {
		t.Errorf("empty edges should return results unchanged, got %v", out)
	}
}

// TestFuseNeighbors_DoesNotMutateInput: the caller's slice must be untouched
// (fusion works on a copy).
func TestFuseNeighbors_DoesNotMutateInput(t *testing.T) {
	cfg := defaultGraphCfg()
	cfg.HubDamping = false
	results := []SearchResult{res("A", 1.0), res("B", 0.6)}
	before := results[1].RRFScore
	_ = fuseNeighbors(results, []graphEdge{edge("A", 1.0, "causal", "B", 1)}, cfg)
	if results[1].RRFScore != before {
		t.Errorf("input slice was mutated: B score %v != original %v", results[1].RRFScore, before)
	}
}

// GraphExpand fail-open + guard paths (no live DB needed).

// TestGraphExpand_DisabledReturnsInput: with Enabled=false the input is returned
// unchanged and no error (byte-identical pipeline).
func TestGraphExpand_DisabledReturnsInput(t *testing.T) {
	cfg := defaultGraphCfg()
	cfg.Enabled = false
	results := []SearchResult{res("A", 1.0)}
	out, err := GraphExpand(context.Background(), nil, results, []string{"private"}, nil, cfg)
	if err != nil {
		t.Fatalf("disabled GraphExpand should not error: %v", err)
	}
	if len(out) != 1 || out[0].ID != "A" {
		t.Errorf("disabled GraphExpand should return input unchanged, got %v", out)
	}
}

// TestGraphExpand_EmptyResultsReturnsInput: empty results short-circuit before
// any DB access (pool=nil is safe) and return the input unchanged.
func TestGraphExpand_EmptyResultsReturnsInput(t *testing.T) {
	cfg := defaultGraphCfg()
	out, err := GraphExpand(context.Background(), nil, []SearchResult{}, []string{"private"}, nil, cfg)
	if err != nil {
		t.Fatalf("empty-results GraphExpand should not error: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("empty-results GraphExpand should return empty, got %v", out)
	}
	// And a non-nil empty slice round-trips as the same slice (not nil).
	in := make([]SearchResult, 0)
	out2, _ := GraphExpand(context.Background(), nil, in, []string{"private"}, nil, cfg)
	if out2 == nil {
		t.Error("empty (non-nil) input should return non-nil slice")
	}
}

// TestGraphExpand_EmptyScopesReturnsInput: no readScopes → no visibility frame →
// return input unchanged, no error.
func TestGraphExpand_EmptyScopesReturnsInput(t *testing.T) {
	cfg := defaultGraphCfg()
	results := []SearchResult{res("A", 1.0)}
	out, err := GraphExpand(context.Background(), nil, results, nil, nil, cfg)
	if err != nil {
		t.Fatalf("empty-scopes GraphExpand should not error: %v", err)
	}
	if len(out) != 1 || out[0].ID != "A" {
		t.Errorf("empty-scopes GraphExpand should return input unchanged, got %v", out)
	}
}

// TestGraphExpand_FetchErrorFailsOpen: a pool that cannot connect makes the edge
// fetch error. GraphExpand must FAIL-OPEN: return the ORIGINAL slice unchanged
// (same length, same order, not nil) alongside a non-nil error.
func TestGraphExpand_FetchErrorFailsOpen(t *testing.T) {
	// pgxpool.New is lazy — it parses the DSN but does not dial until first use.
	// Pointing at a closed port guarantees the first Query() errors.
	pool, err := pgxpool.New(context.Background(), "postgres://nouser:nopass@127.0.0.1:1/nodb?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("constructing lazy pool should not fail: %v", err)
	}
	defer pool.Close()

	cfg := defaultGraphCfg()
	// Seeds carry an in-scope ("private") scope so they survive the T41 grant-only
	// seed filter and the expansion actually reaches fetchNeighbors (a real RRF hit
	// always carries its block's scope; the scope-less res() helper would be skipped
	// as grant-only-visible and short-circuit before the fetch).
	results := []SearchResult{
		{ID: "TOP", Title: "TOP", RRFScore: 1.0, Scope: "private"},
		{ID: "MID", Title: "MID", RRFScore: 0.8, Scope: "private"},
	}
	out, gerr := GraphExpand(context.Background(), pool, results, []string{"private"}, nil, cfg)
	if gerr == nil {
		t.Fatal("expected a fetch error from an unreachable pool")
	}
	if len(out) != len(results) {
		t.Fatalf("fail-open must return original slice (len %d), got %d", len(results), len(out))
	}
	if out[0].ID != "TOP" || out[1].ID != "MID" {
		t.Errorf("fail-open must return original order unchanged, got %v", out)
	}
}

// abs is a tiny float helper for tolerance comparisons.
func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
