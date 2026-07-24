package rrf

import (
	"context"
	"reflect"
	"testing"

	"github.com/GottZ/ctx/internal/graphcache"
)

// scopedRes is res with an in-scope scope, so the T41 grant-only leaf filter
// (a per-result continue) does not shadow the floor break under test.
func scopedRes(id string, score float64) SearchResult {
	r := res(id, score)
	r.Scope = "private"
	return r
}

// TestFuseNeighbors_InjectCapDeclared is the W05.4 expand-declaration gate: the
// MaxInjected truncation was SILENT (no flag, no telemetry, design §2 "stille
// Kappungen"). It must now show up as inject_capped in the traversal report —
// and ONLY there.
//
// RED PROBE (recorded 2026-07-25): with the `rep.Add(graphcache.TravInjectCapped)`
// line removed from fuseNeighborsReport this test fails with
//
//	MaxInjected truncation is still SILENT: report has no inject_capped trip (counts map[])
func TestFuseNeighbors_InjectCapDeclared(t *testing.T) {
	cfg := defaultGraphCfg()
	cfg.MaxInjected = 1
	cfg.HubDamping = false

	results := []SearchResult{res("S", 1.0)}
	edges := []graphEdge{
		edge("S", 1.0, "causal", "N1", 1),
		edge("S", 1.0, "factual", "N2", 1),
		edge("S", 1.0, "recurrent", "N3", 1),
	}

	rep := graphcache.NewBudgetReport(graphcache.SourceSQL)
	out := fuseNeighborsReport(results, edges, cfg, rep)

	if got := len(out); got != 2 { // seed + exactly one injected neighbour
		t.Fatalf("setup: %d results, want 2 (the cap must have bitten)", got)
	}
	if rep.Count(graphcache.TravInjectCapped) != 1 {
		t.Errorf("MaxInjected truncation is still SILENT: report has no inject_capped trip (counts %v)", rep.Counts)
	}
	if rep.Count(graphcache.TravNodeLimitReached) != 0 {
		t.Errorf("inject cap is a SERVER budget, not an API-contract limit: %v", rep.Limits)
	}
}

// TestFuseNeighbors_InjectCapUntripped: no cap, no trip. Guards against a report
// that is "always full" and therefore meaningless.
func TestFuseNeighbors_InjectCapUntripped(t *testing.T) {
	cfg := defaultGraphCfg()
	cfg.MaxInjected = 10

	rep := graphcache.NewBudgetReport(graphcache.SourceSQL)
	fuseNeighborsReport([]SearchResult{res("S", 1.0)},
		[]graphEdge{edge("S", 1.0, "causal", "N1", 1)}, cfg, rep)

	if rep.Tripped() {
		t.Errorf("no cap was hit, but the report claims trips: %v", rep.Counts)
	}
}

// TestFuseNeighbors_ReportDoesNotChangeOutput is the regression assert of the
// gate: the fused output with a report sink must be byte-identical (deep-equal)
// to the output without one. W05.4 declares — it does not steer.
func TestFuseNeighbors_ReportDoesNotChangeOutput(t *testing.T) {
	cfg := defaultGraphCfg()
	cfg.MaxInjected = 2

	results := []SearchResult{res("S", 1.0), res("N2", 0.4)}
	edges := []graphEdge{
		edge("S", 1.0, "causal", "N1", 1),
		edge("S", 1.0, "factual", "N2", 2),
		edge("S", 1.0, "recurrent", "N3", 3),
		edge("S", 1.0, "topical", "N4", 1),
	}

	// The legacy 3-arg entry point is what every pre-W05.4 unit test calls.
	silent := fuseNeighbors(results, edges, cfg)
	reported := fuseNeighborsReport(results, edges, cfg, graphcache.NewBudgetReport(graphcache.SourceSQL))

	if !reflect.DeepEqual(silent, reported) {
		t.Errorf("declaring the cap changed the fusion output:\n silent:   %+v\n reported: %+v", silent, reported)
	}
}

// TestSelectSeeds_FloorBreakDeclared: the SeedScoreFloor break was the second
// silent capping. It becomes seed_floor_capped — telemetry only.
//
// RED PROBE (recorded 2026-07-25): with the `rep.Add(graphcache.TravSeedFloorCapped)`
// line removed from selectSeeds this test fails with
//
//	SeedScoreFloor break is still SILENT: counts map[]
func TestSelectSeeds_FloorBreakDeclared(t *testing.T) {
	cfg := defaultGraphCfg()
	cfg.SeedScoreFloor = 0.5
	cfg.SeedCount = 5

	// Descending scores; 0.1 is below 0.5*1.0 and triggers the break. The scope
	// must be in readScopes or T41 would drop every result as grant-only, which
	// is a DIFFERENT (per-result continue) path than the floor break.
	results := []SearchResult{scopedRes("A", 1.0), scopedRes("B", 0.9), scopedRes("C", 0.1)}
	scopes := []string{"private"}

	rep := graphcache.NewBudgetReport(graphcache.SourceSQL)
	seeds, _, _ := selectSeeds(results, scopes, cfg, rep)

	if len(seeds) != 2 {
		t.Fatalf("setup: %d seeds, want 2 (the floor must have cut C)", len(seeds))
	}
	if rep.Count(graphcache.TravSeedFloorCapped) != 1 {
		t.Errorf("SeedScoreFloor break is still SILENT: counts %v", rep.Counts)
	}

	// No break when everything clears the floor.
	rep2 := graphcache.NewBudgetReport(graphcache.SourceSQL)
	if seeds2, _, _ := selectSeeds(results[:2], scopes, cfg, rep2); len(seeds2) != 2 || rep2.Tripped() {
		t.Errorf("floor untouched, but report trips: seeds=%d counts=%v", len(seeds2), rep2.Counts)
	}
}

// TestGraphExpandWithReport_Passthrough pins the wrapper contract: the reporting
// entry point returns the same slice and error as GraphExpand for the disabled
// stage, and always hands back a non-nil SQL-arm report (Source is what the
// ego envelope shows; the query path never wires it to any envelope).
func TestGraphExpandWithReport_Passthrough(t *testing.T) {
	cfg := defaultGraphCfg()
	cfg.Enabled = false
	in := []SearchResult{res("A", 1.0)}

	out, rep, err := GraphExpandWithReport(context.Background(), nil, in, []string{"private"}, nil, []string{"knowledge"}, cfg)
	if err != nil {
		t.Fatalf("disabled stage returned an error: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Errorf("disabled stage changed the results: %+v", out)
	}
	if rep == nil {
		t.Fatal("report must never be nil")
	}
	if rep.Source != graphcache.SourceSQL {
		t.Errorf("report source = %q, want %q (no expand cache arm before W05.7)", rep.Source, graphcache.SourceSQL)
	}
	if rep.Tripped() {
		t.Errorf("disabled stage tripped budgets: %v", rep.Counts)
	}
}
