// Wave C9 (Cluster-Topic-Map, design/03 §4.9/§5.4) — the PURE half: placement,
// cap and the ordering invariant. No database.
package rrf

import (
	"math"
	"testing"

	"github.com/GottZ/ctx/internal/graphcache"
)

func cand(id, cluster string, quality float64) clusterCandidate {
	return clusterCandidate{block: SearchResult{ID: id}, clusterID: cluster, quality: quality}
}

// PLACEMENT WITHOUT A NEW KNOB. An injected block carries ONLY cluster evidence,
// so it enters at the fraction of the top score that the stage's own declared
// authority represents: topScore * BoostWeight * share. A stage allowed to move
// a native result by at most (1+BoostWeight) may introduce one at at most
// BoostWeight of the top — the same number governs both, so an operator raising
// boost_weight gets a coherent increase in reach without a second dial.
//
// ROT-PROBE: place at `topScore * share` (drop the BoostWeight factor) ⇒ the
// injected block lands above native hits and the ceiling assert fails.
func TestInjectPlacementStaysUnderTheStageCeiling(t *testing.T) {
	in := c3Results(5) // scores 1.00 … 0.96
	cfg := c3Cfg()
	cfg.InjectMax = 2

	out := injectClusterMembers(in, []clusterCandidate{cand("new-1", "cA", 1)},
		map[string]float64{"cA": 1.0}, cfg, graphcache.NewBudgetReport(graphcache.SourceSQL))

	if len(out) != len(in)+1 {
		t.Fatalf("len = %d, want %d", len(out), len(in)+1)
	}
	var injected *SearchResult
	for i := range out {
		if out[i].ViaCluster {
			injected = &out[i]
		}
	}
	if injected == nil {
		t.Fatal("nothing was injected")
	}
	want := in[0].RRFScore * cfg.BoostWeight
	if math.Abs(injected.RRFScore-want) > 1e-12 {
		t.Errorf("injected score = %g, want %g (topScore * boost_weight * share)", injected.RRFScore, want)
	}
	// Cluster evidence alone must never outrank direct retrieval evidence.
	for i := range out {
		if !out[i].ViaCluster && out[i].RRFScore <= injected.RRFScore {
			t.Errorf("native %s (%g) fell below an injected block (%g)", out[i].ID, out[i].RRFScore, injected.RRFScore)
		}
	}
	// The output is re-sorted, which is load-bearing for the same reason it is in
	// fuseClusters: selectSeeds downstream breaks at the first result under its
	// floor, so an unsorted tail would cut the seed set short.
	for i := 1; i < len(out); i++ {
		if out[i-1].RRFScore < out[i].RRFScore {
			t.Fatalf("output is not sorted desc at %d", i)
		}
	}
}

// THE CAP KEEPS THE HIGHEST SHARE FIRST, then quality, then id. Share before
// quality is the substantive choice: quality_score ranks blocks WITHIN a
// cluster, share ranks how much this query is about that cluster at all, and a
// quality-first cut would let one strong cluster starve a more relevant one.
//
// ROT-PROBE: sort by quality first ⇒ the two cB blocks win and cA, the cluster
// the query actually landed in, is cut.
func TestInjectCapPrefersShareThenQuality(t *testing.T) {
	in := c3Results(5)
	cfg := c3Cfg()
	cfg.InjectMax = 2
	rep := graphcache.NewBudgetReport(graphcache.SourceSQL)

	out := injectClusterMembers(in, []clusterCandidate{
		cand("b-hi", "cB", 9),
		cand("b-lo", "cB", 8),
		cand("a-lo", "cA", 1),
		cand("a-hi", "cA", 2),
	}, map[string]float64{"cA": 0.9, "cB": 0.2}, cfg, rep)

	var got []string
	for i := range out {
		if out[i].ViaCluster {
			got = append(got, out[i].ID)
		}
	}
	if len(got) != 2 {
		t.Fatalf("injected %v, want 2", got)
	}
	seen := map[string]bool{got[0]: true, got[1]: true}
	if !seen["a-hi"] || !seen["a-lo"] {
		t.Errorf("injected %v, want both cA candidates — share ranks before quality", got)
	}
	if rep.Count(graphcache.TravClusterInjectCapped) != 1 {
		t.Error("the cut was silent — cluster_inject_capped must be recorded exactly once")
	}
}

// inject_max 0 is the shipped default and must be a pure no-op: the SAME slice
// header comes back, so there is not even an allocation to differ.
func TestInjectDisabledIsIdentity(t *testing.T) {
	in := c3Results(5)
	cfg := c3Cfg()
	cfg.InjectMax = 0

	out := injectClusterMembers(in, []clusterCandidate{cand("new-1", "cA", 1)},
		map[string]float64{"cA": 1.0}, cfg, graphcache.NewBudgetReport(graphcache.SourceSQL))
	if len(out) != len(in) {
		t.Fatalf("len = %d, want %d — inject_max 0 must add nothing", len(out), len(in))
	}
	for i := range in {
		if out[i].ID != in[i].ID || out[i].RRFScore != in[i].RRFScore || out[i].ViaCluster {
			t.Fatalf("drift at %d: %+v", i, out[i])
		}
	}
}

// Degenerate scores (everything at or below zero) make the relative placement
// undefined. Adding a block anyway would put it at 0 among zeros, i.e. in an
// arbitrary position — the graph stage refuses the same case for the same
// reason.
func TestInjectRefusesDegenerateScores(t *testing.T) {
	in := []SearchResult{{ID: "a", RRFScore: 0}, {ID: "b", RRFScore: 0}}
	cfg := c3Cfg()
	cfg.InjectMax = 3

	out := injectClusterMembers(in, []clusterCandidate{cand("new-1", "cA", 1)},
		map[string]float64{"cA": 1.0}, cfg, graphcache.NewBudgetReport(graphcache.SourceSQL))
	if len(out) != len(in) {
		t.Fatalf("len = %d, want %d — undefined placement must add nothing", len(out), len(in))
	}
}
