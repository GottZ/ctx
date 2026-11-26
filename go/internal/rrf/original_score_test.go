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
