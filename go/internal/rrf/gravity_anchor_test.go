package rrf

import (
	"math"
	"testing"
	"time"
)

// Both gravity boosts are anchored absolutely — against the largest value the
// formula itself can produce, not against the strongest candidate that happens
// to be in the current window (Issue #40, finding 3). The set-relative form was
// only ever exercised with an exact hit in the set; these tests pin the
// degenerate case, where the best near-miss used to be lifted onto the full
// boost, plus the tie-order determinism the reranker owes every measurement
// (Issue #40, side finding N2).

// boostFactorOf returns the factor a rerank applied to one block.
func boostFactorOf(t *testing.T, boosted []SearchResult, id string) float64 {
	t.Helper()
	for _, r := range boosted {
		if r.ID != id {
			continue
		}
		if r.RRFScoreOriginal == nil {
			t.Fatalf("%q carries no RRFScoreOriginal, so no boost was applied at all", id)
		}
		return r.RRFScore / *r.RRFScoreOriginal
	}
	t.Fatalf("%q is missing from the reranked results", id)
	return 0
}

// TestApplyCyclicGravityBoost_NoExactPhaseMatchStaysWeak is the degeneration
// case: query "Dienstag", and the window holds a Wednesday block plus a block
// with no dimension at all. Wednesday is one weekday off — phase distance 1/7,
// which sigma 0.07 decays to ~0.128. Being the best block in the window must
// not turn that into the boost of an exact hit.
func TestApplyCyclicGravityBoost_NoExactPhaseMatchStaysWeak(t *testing.T) {
	tuesday := mustDate("2026-03-31") // ISO weekday 2
	const boostWeight = 0.30
	dimWeights := map[string]float64{"weekday": 1.0}

	results := []SearchResult{
		{ID: "no-dim", RRFScore: 0.100},
		{ID: "wed-block", RRFScore: 0.090},
	}
	blockDims := map[string][]TemporalDim{
		"wed-block": {{Dimension: "weekday", Value: "3"}},
	}

	boosted := ApplyCyclicGravityBoost(results, blockDims, dimWeights, tuesday, boostWeight)

	// Expectation derived from the formula, not copied from the doc table.
	decay := GaussianDecay(1.0/7.0, DimensionSigma["weekday"])
	want := 1.0 + boostWeight*decay // ~1.038, not 1.300
	if got := boostFactorOf(t, boosted, "wed-block"); math.Abs(got-want) > 1e-3 {
		t.Fatalf("wednesday factor is %.4f, want %.4f (decay %.4f against the sum-of-weights anchor)",
			got, want, decay)
	}
	if got := boostFactorOf(t, boosted, "no-dim"); math.Abs(got-1.0) > 1e-12 {
		t.Fatalf("block without dimensions got factor %.4f, want 1.0 (neutral)", got)
	}
}

// TestApplyCyclicGravityBoost_AnchorIsCyclicWeightSum pins the anchor itself:
// with mixed weights the cyclic path is anchored against the cyclic share
// alone, so an exact hit on every scorable dimension reaches the full boost —
// the linear share is the linear path's business.
func TestApplyCyclicGravityBoost_AnchorIsCyclicWeightSum(t *testing.T) {
	tuesday := mustDate("2026-03-31")
	const boostWeight = 0.30
	dimWeights := map[string]float64{"linear": 0.6, "weekday": 0.4}

	results := []SearchResult{{ID: "tue-block", RRFScore: 0.100}}
	blockDims := map[string][]TemporalDim{
		"tue-block": {{Dimension: "weekday", Value: "2"}},
	}

	boosted := ApplyCyclicGravityBoost(results, blockDims, dimWeights, tuesday, boostWeight)
	want := 1.0 + boostWeight
	if got := boostFactorOf(t, boosted, "tue-block"); math.Abs(got-want) > 1e-12 {
		t.Fatalf("exact weekday hit got factor %.6f, want %.6f", got, want)
	}
}

// TestApplyCyclicGravityBoost_NothingScorableIsNoOp covers the anchor's own
// degenerate case: weights that name nothing ComputeCyclicGravity can score
// leave the results untouched instead of dividing by zero.
func TestApplyCyclicGravityBoost_NothingScorableIsNoOp(t *testing.T) {
	tuesday := mustDate("2026-03-31")
	results := []SearchResult{{ID: "a", RRFScore: 0.100}, {ID: "b", RRFScore: 0.090}}
	blockDims := map[string][]TemporalDim{
		"a": {{Dimension: "weekday", Value: "2"}},
	}

	cases := []struct {
		name       string
		dimWeights map[string]float64
	}{
		{"linear_only", map[string]float64{"linear": 1.0}},
		{"unknown_dimension_only", map[string]float64{"year": 1.0}},
		{"non_positive_weights_only", map[string]float64{"weekday": 0}},
		{"nil_weights", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			boosted := ApplyCyclicGravityBoost(results, blockDims, tc.dimWeights, tuesday, 0.30)
			if len(boosted) != len(results) {
				t.Fatalf("length %d, want %d", len(boosted), len(results))
			}
			for i := range boosted {
				if boosted[i].ID != results[i].ID {
					t.Fatalf("rank %d is %q, want %q", i, boosted[i].ID, results[i].ID)
				}
				if boosted[i].RRFScore != results[i].RRFScore {
					t.Fatalf("%q score %.6f, want %.6f", boosted[i].ID, boosted[i].RRFScore, results[i].RRFScore)
				}
				if boosted[i].RRFScoreOriginal != nil {
					t.Fatalf("%q carries RRFScoreOriginal, so the results were rewritten", boosted[i].ID)
				}
			}
		})
	}
}

// TestCyclicGravityAnchor_ScorableWeightsOnly pins which weights the anchor
// counts. It must mirror what ComputeCyclicGravity can actually score,
// otherwise the ratio leaves [0,1] in one direction or the other.
func TestCyclicGravityAnchor_ScorableWeightsOnly(t *testing.T) {
	cases := []struct {
		name       string
		dimWeights map[string]float64
		want       float64
	}{
		{"single_cyclic_dimension", map[string]float64{"weekday": 1.0}, 1.0},
		{"linear_share_excluded", map[string]float64{"linear": 0.6, "weekday": 0.4}, 0.4},
		{"two_cyclic_dimensions", map[string]float64{"weekday": 0.4, "month": 0.2}, 0.6},
		{"unknown_dimension_excluded", map[string]float64{"weekday": 0.4, "year": 0.5}, 0.4},
		{"non_positive_weight_excluded", map[string]float64{"weekday": 0.4, "month": -0.1}, 0.4},
		{"nothing_scorable", map[string]float64{"linear": 1.0}, 0},
		{"nil_map", nil, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cyclicGravityAnchor(tc.dimWeights); math.Abs(got-tc.want) > 1e-12 {
				t.Fatalf("anchor %.6f, want %.6f", got, tc.want)
			}
		})
	}
}

// TestApplyGravityBoost_SaturatesAtAbsoluteAnchor pins the linear path: one
// date at the clamp distance already reaches the full boost, a second such date
// cannot push past it, and a distant date stays near neutral. Under the
// set-relative form the two-date block set the scale and the single-date block
// was demoted to half the boost for it.
func TestApplyGravityBoost_SaturatesAtAbsoluteAnchor(t *testing.T) {
	target := mustDate("2026-03-29")
	const boostWeight = 0.30

	results := []SearchResult{
		{ID: "one-date", RRFScore: 0.100},
		{ID: "two-dates", RRFScore: 0.100},
		{ID: "far", RRFScore: 0.100},
	}
	blockDates := map[string][]time.Time{
		// Both inside the 0.5-day clamp and ahead of the target, so each date
		// contributes exactly the per-date maximum.
		"one-date":  {target.Add(12 * time.Hour)},
		"two-dates": {target.Add(6 * time.Hour), target.Add(12 * time.Hour)},
		"far":       {target.AddDate(0, 0, -45)},
	}
	params := GravityParams{
		TargetDate:  target,
		Direction:   "both",
		Cutoff:      60,
		Power:       1.5,
		BoostWeight: boostWeight,
	}

	boosted := ApplyGravityBoost(results, blockDates, params)

	want := 1.0 + boostWeight
	one := boostFactorOf(t, boosted, "one-date")
	if math.Abs(one-want) > 1e-12 {
		t.Fatalf("single saturating date got factor %.6f, want %.6f", one, want)
	}
	two := boostFactorOf(t, boosted, "two-dates")
	if math.Abs(two-want) > 1e-12 {
		t.Fatalf("two saturating dates got factor %.6f, want %.6f (saturating, not doubling)", two, want)
	}
	far := boostFactorOf(t, boosted, "far")
	if far < 1.0 || far > 1.01 {
		t.Fatalf("date 45 days away got factor %.6f, want near-neutral in [1.0, 1.01]", far)
	}
}

// TestMaxDateGravity_DerivedFromPower pins the per-date anchor against the
// formula it is derived from: whatever ComputeGravity returns for a single date
// sitting on the target is exactly the anchor for that power.
func TestMaxDateGravity_DerivedFromPower(t *testing.T) {
	target := mustDate("2026-03-29")

	for _, power := range []float64{0, 1.0, 1.5, 2.0} {
		got := ComputeGravity([]time.Time{target}, GravityParams{TargetDate: target, Power: power})
		if want := maxDateGravity(power); math.Abs(got-want) > 1e-12 {
			t.Fatalf("power %.1f: strongest single date scores %.6f, anchor says %.6f", power, got, want)
		}
	}

	// The production power spelled out, so a changed default is visible here.
	if want := math.Pow(2, 1.5*1.2); math.Abs(maxDateGravity(1.5)-want) > 1e-12 {
		t.Fatalf("anchor at power 1.5 is %.6f, want %.6f", maxDateGravity(1.5), want)
	}
}

// tiedFixture returns results whose scores repeat in a fixed cycle: four tie
// groups of four, interleaved. Blocks that share an RRF score are ordinary —
// the score is rank-based, so two blocks hit by the same single arm at the same
// rank carry the same number. The set is deliberately past the point where the
// sort package still runs a plain insertion sort, which would preserve the
// input order even without asking for stability.
func tiedFixture() []SearchResult {
	const groups = 4
	out := make([]SearchResult, 16)
	for i := range out {
		out[i] = SearchResult{
			ID:       "b" + string(rune('a'+i)),
			RRFScore: 0.01 * float64(groups-i%groups),
		}
	}
	return out
}

// wantTiedOrder is the stable ranking of tiedFixture: score descending, input
// order inside each tie group.
func wantTiedOrder() []string {
	in := tiedFixture()
	var out []string
	for _, score := range []float64{0.04, 0.03, 0.02, 0.01} {
		for _, r := range in {
			if math.Abs(r.RRFScore-score) < 1e-12 {
				out = append(out, r.ID)
			}
		}
	}
	return out
}

// TestGravityBoosts_TiedScoresKeepInputOrder is the determinism gate. Blocks
// that tie after boosting must come out in the order they went in — anything
// else makes the same query over the same corpus a different result set from
// one call to the next, and with it every measurement built on top of it
// (Issue #40, side finding N2). Repeated to show the ranking does not drift
// between calls either.
func TestGravityBoosts_TiedScoresKeepInputOrder(t *testing.T) {
	target := mustDate("2026-03-29")
	want := wantTiedOrder()

	assertOrder := func(t *testing.T, what string, iteration int, boosted []SearchResult) {
		t.Helper()
		if len(boosted) != len(want) {
			t.Fatalf("%s: length %d, want %d", what, len(boosted), len(want))
		}
		for i := range boosted {
			if boosted[i].ID != want[i] {
				t.Fatalf("%s (iteration %d): rank %d is %q, want %q — tied scores were reordered (got %s)",
					what, iteration, i, boosted[i].ID, want[i], idsOf(boosted))
			}
		}
	}

	for iteration := range 50 {
		// Linear path: no dates at all, so every factor is 1.0 and the input
		// ties survive into the sort untouched.
		linear := ApplyGravityBoost(tiedFixture(), map[string][]time.Time{}, GravityParams{
			TargetDate:  target,
			Direction:   "both",
			Cutoff:      60,
			Power:       1.5,
			BoostWeight: 0.30,
		})
		assertOrder(t, "linear", iteration, linear)

		// Cyclic path: same, with no block dimensions to separate them.
		cyclic := ApplyCyclicGravityBoost(tiedFixture(),
			map[string][]TemporalDim{}, map[string]float64{"weekday": 1.0}, target, 0.30)
		assertOrder(t, "cyclic", iteration, cyclic)
	}
}
