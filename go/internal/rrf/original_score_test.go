package rrf

import (
	"testing"
	"time"
)

// Issue #528 (point 6, review): rrf_score_original is the fusion's score
// whatever stages ran after the fusion. The cyclic gravity runs before the
// linear one and the reranker after both; each of them used to overwrite the
// field with the score the previous stage had already boosted, so a mixed
// gravity or a reranker behind the gravity labelled an answer on a boosted
// scale.
func TestStagesKeepTheFusionScore(t *testing.T) {
	target := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC) // a Tuesday
	fusion := []SearchResult{{ID: "tue", RRFScore: 0.0075}, {ID: "other", RRFScore: 0.0070}}

	// Cyclic first: it records the fusion score and boosts the Tuesday block.
	cyc := ApplyCyclicGravityBoost(fusion, map[string][]TemporalDim{"tue": {{Dimension: "weekday", Value: "2"}}},
		map[string]float64{"weekday": 0.4}, target, 0.3*0.4)
	if cyc[0].ID != "tue" || !(cyc[0].RRFScore > 0.0075) {
		t.Fatalf("precondition: the cyclic stage boosted nothing (%s %v)", cyc[0].ID, cyc[0].RRFScore)
	}
	// Linear second: the original stays the fusion's.
	lin := ApplyGravityBoost(cyc, map[string][]time.Time{"tue": {target}}, GravityParams{
		TargetDate: target, Direction: "both", Cutoff: 60, Power: 1.5, BoostWeight: 0.3 * 0.6,
	})
	for _, r := range lin {
		want := 0.0075
		if r.ID == "other" {
			want = 0.0070
		}
		if r.RRFScoreOriginal == nil || *r.RRFScoreOriginal != want {
			t.Errorf("after cyclic and linear gravity: %s rrf_score_original = %v, want the fusion score %v", r.ID, deref(r.RRFScoreOriginal), want)
		}
	}
	// The reranker last: it keeps what the gravity recorded.
	rer := applyRerankScores(lin, []float64{0.9, 0.1}, 2, 0.5)
	for _, r := range rer {
		want := 0.0075
		if r.ID == "other" {
			want = 0.0070
		}
		if r.RRFScoreOriginal == nil || *r.RRFScoreOriginal != want {
			t.Errorf("after the reranker: %s rrf_score_original = %v, want the fusion score %v", r.ID, deref(r.RRFScoreOriginal), want)
		}
	}
	// Without a stage before it, the reranker records the fusion score
	// rounded to four decimals, as before.
	alone := applyRerankScores([]SearchResult{{ID: "a", RRFScore: 0.007549}}, []float64{0.5}, 1, 0.5)
	if alone[0].RRFScoreOriginal == nil || *alone[0].RRFScoreOriginal != 0.0075 {
		t.Errorf("reranker alone: rrf_score_original = %v, want 0.0075", deref(alone[0].RRFScoreOriginal))
	}
}

// deref prints a score pointer as its value ("<nil>" for none).
func deref(p *float64) any {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// Issue #528, review round 1 (M1): the cluster prior and the graph
// reinforcement keep the fusion score too. Neither recorded it, so a row they
// boosted labelled on its boosted score when no gravity ran before them and on
// its fusion score when one did — the same fusion and the same boost gave two
// labels depending on whether the question carried a date.
func TestGraphAndClusterKeepTheFusionScore(t *testing.T) {
	fusion := []SearchResult{{ID: "a", RRFScore: 0.0075}, {ID: "b", RRFScore: 0.0074}}
	toA := []graphEdge{{seedID: "b", seedRRFScore: 0.0074, seedRealScore: 0.0074, relationship: "topical",
		neighbor: SearchResult{ID: "a"}, hopDecay: 1, neighborDegree: 1}}
	toNew := graphEdge{seedID: "a", seedRRFScore: 0.0075, seedRealScore: 0.0075, relationship: "topical",
		neighbor: SearchResult{ID: "new"}, hopDecay: 1, neighborDegree: 1}

	origOf := func(rs []SearchResult, id string) *float64 {
		for _, r := range rs {
			if r.ID == id {
				return r.RRFScoreOriginal
			}
		}
		t.Fatalf("%s missing from %v", id, rs)
		return nil
	}
	scoreOf := func(rs []SearchResult, id string) float64 {
		for _, r := range rs {
			if r.ID == id {
				return r.RRFScore
			}
		}
		return -1
	}

	// The graph alone: the reinforced row records its fusion score, an
	// untouched row and an injected one carry none.
	g := fuseNeighbors(fusion, append(toA, toNew), defaultGraphCfg())
	if !(scoreOf(g, "a") > 0.0075) {
		t.Fatalf("precondition: the graph reinforced nothing (a = %v)", scoreOf(g, "a"))
	}
	if o := origOf(g, "a"); o == nil || *o != 0.0075 {
		t.Errorf("graph alone: reinforced a rrf_score_original = %v, want the fusion score 0.0075", deref(o))
	}
	if o := origOf(g, "b"); o != nil {
		t.Errorf("graph alone: untouched b rrf_score_original = %v, want none", *o)
	}
	if o := origOf(g, "new"); o != nil {
		t.Errorf("graph alone: injected row rrf_score_original = %v, want none", *o)
	}

	// The same fusion behind the linear gravity (no block dated): the same
	// value as without it.
	target := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)
	lin := ApplyGravityBoost(fusion, map[string][]time.Time{"z": {target}},
		GravityParams{TargetDate: target, Direction: "both", Cutoff: 60, Power: 1.5, BoostWeight: 0.18})
	gl := fuseNeighbors(lin, toA, defaultGraphCfg())
	if o := origOf(gl, "a"); o == nil || *o != 0.0075 {
		t.Errorf("gravity then graph: a rrf_score_original = %v, want 0.0075", deref(o))
	}

	// The cluster prior: a boosted member records its fusion score, also
	// behind a gravity that boosted it first.
	cfg := ClusterConfig{Enabled: true, SeedCount: 10, TopClusters: 2, MinShare: 0.25, BoostWeight: 0.12, SizeDamping: true}
	c := fuseClusters(fusion, map[string]string{"a": "cA"}, map[string]float64{"cA": 1}, cfg, nil)
	if !(scoreOf(c, "a") > 0.0075) {
		t.Fatalf("precondition: the cluster prior boosted nothing (a = %v)", scoreOf(c, "a"))
	}
	if o := origOf(c, "a"); o == nil || *o != 0.0075 {
		t.Errorf("cluster alone: boosted a rrf_score_original = %v, want 0.0075", deref(o))
	}
	if o := origOf(c, "b"); o != nil {
		t.Errorf("cluster alone: untouched b rrf_score_original = %v, want none", *o)
	}
	dated := ApplyGravityBoost(fusion, map[string][]time.Time{"a": {target}},
		GravityParams{TargetDate: target, Direction: "both", Cutoff: 60, Power: 1.5, BoostWeight: 0.18})
	gc := fuseClusters(dated, map[string]string{"a": "cA"}, map[string]float64{"cA": 1}, cfg, nil)
	if o := origOf(gc, "a"); o == nil || *o != 0.0075 {
		t.Errorf("gravity then cluster: a rrf_score_original = %v, want 0.0075", deref(o))
	}
}
