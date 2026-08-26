package evalscore_test

import (
	"fmt"
	"math"
	"math/rand"
	"testing"

	"github.com/GottZ/ctx/internal/evalscore"
)

// discount is the position discount both metrics share with NDCGBinary:
// 1/log2(rank+2) over a 0-based rank. Every expected value below is built from
// it rather than pasted as a decimal, so a change to the discount shows up as
// a failing metric and not as a silently re-fitted constant.
func discount(rank int) float64 {
	return 1 / math.Log2(float64(rank)+2)
}

// rawAlphaDCG is an INDEPENDENT reimplementation of the un-normalised α-DCG,
// used to brute-force the true ideal ranking on tiny pools. It exists so the
// "ideal ranking scores 1.0" claim is checked against a search over all
// permutations instead of against the very greedy construction under test.
func rawAlphaDCG(ranking []string, aspectsOf map[string][]string, k int, alpha float64) float64 {
	if k > len(ranking) {
		k = len(ranking)
	}
	served := map[string]int{}
	seen := map[string]bool{}
	dcg := 0.0
	for rank := 0; rank < k; rank++ {
		id := ranking[rank]
		if seen[id] {
			continue
		}
		seen[id] = true
		for _, f := range aspectsOf[id] {
			dcg += math.Pow(1-alpha, float64(served[f])) * discount(rank)
		}
		for _, f := range aspectsOf[id] {
			served[f]++
		}
	}
	return dcg
}

// permutations enumerates every ordering of ids. Only called with at most four
// ids (24 orderings).
func permutations(ids []string) [][]string {
	if len(ids) <= 1 {
		return [][]string{append([]string(nil), ids...)}
	}
	var out [][]string
	for i := range ids {
		rest := make([]string, 0, len(ids)-1)
		rest = append(rest, ids[:i]...)
		rest = append(rest, ids[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]string{ids[i]}, p...))
		}
	}
	return out
}

// bestOrder returns the permutation with the highest raw α-DCG, ties going to
// the first one enumerated so the result is deterministic.
func bestOrder(ids []string, aspectsOf map[string][]string, k int, alpha float64) ([]string, float64) {
	var best []string
	bestDCG := math.Inf(-1)
	for _, p := range permutations(ids) {
		if d := rawAlphaDCG(p, aspectsOf, k, alpha); d > bestDCG {
			best, bestDCG = p, d
		}
	}
	return best, bestDCG
}

// displacementFixture is the case design/05 §4.1b is written around: five
// source blocks carrying one facet each, plus one derived block D that carries
// the facets of three of them. aspectsOf is the block→facet view AlphaNDCG
// takes, aspects the facet→block view SRecallAtK takes.
func displacementFixture() (aspectsOf, aspects map[string][]string) {
	aspectsOf = map[string][]string{
		"D":  {"f1", "f2", "f3"},
		"s1": {"f1"},
		"s2": {"f2"},
		"s3": {"f3"},
		"s4": {"f4"},
		"s5": {"f5"},
	}
	aspects = map[string][]string{
		"f1": {"s1", "D"},
		"f2": {"s2", "D"},
		"f3": {"s3", "D"},
		"f4": {"s4"},
		"f5": {"s5"},
	}
	return aspectsOf, aspects
}

// TestSRecallAtK pins subtopic recall including the cases that decide whether
// the number means coverage or document count: one block covering two facets,
// a repeat inside the window, a facet no block covers, and the k clamp.
func TestSRecallAtK(t *testing.T) {
	aspects := map[string][]string{
		"f1": {"s1", "D"},
		"f2": {"s2", "D"},
		"f3": {"s3"},
	}
	cases := []struct {
		name    string
		ranked  []string
		aspects map[string][]string
		k       int
		want    float64
	}{
		{"every facet covered", []string{"s1", "s2", "s3"}, aspects, 3, 1.0},
		{"one block covers two facets", []string{"D"}, aspects, 1, 2.0 / 3},
		{"nothing covered", []string{"x", "y"}, aspects, 2, 0.0},
		{"cutoff hides the third facet", []string{"s1", "s2", "s3"}, aspects, 2, 2.0 / 3},
		{"k longer than the ranking is not an error", []string{"s1", "s2", "s3"}, aspects, 99, 1.0},
		{"a repeat covers what it covers once", []string{"s1", "s1", "s2"}, aspects, 3, 2.0 / 3},
		{"a facet without blocks stays uncovered", []string{"s1", "s2", "s3"}, map[string][]string{
			"f1": {"s1"}, "f2": {"s2"}, "f3": {"s3"}, "f4": nil,
		}, 3, 0.75},
		{"k<=0 yields 0", []string{"s1"}, aspects, 0, 0.0},
		{"empty ranking yields 0", nil, aspects, 3, 0.0},
		{"no facets yields 0, never NaN", []string{"s1"}, map[string][]string{}, 3, 0.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evalscore.SRecallAtK(tc.ranked, tc.aspects, tc.k)
			if math.Abs(got-tc.want) > 1e-12 {
				t.Errorf("SRecallAtK = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAlphaNDCGDegenerateInputs pins the 0-not-NaN contract the slice means in
// the reports depend on, plus the two clamps.
func TestAlphaNDCGDegenerateInputs(t *testing.T) {
	pool := map[string][]string{"s1": {"f1"}, "s2": {"f2"}}
	cases := []struct {
		name      string
		ranked    []string
		aspectsOf map[string][]string
		k         int
		want      float64
	}{
		{"empty ranking", nil, pool, 5, 0},
		{"no judgements", []string{"s1"}, map[string][]string{}, 5, 0},
		{"k<=0", []string{"s1"}, pool, 0, 0},
		{"only unjudged ids", []string{"x", "y"}, pool, 2, 0},
		{"only facet-less documents", []string{"s1"}, map[string][]string{"s1": nil}, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evalscore.AlphaNDCG(tc.ranked, tc.aspectsOf, tc.k, evalscore.AlphaNDCGDefault)
			if got != tc.want {
				t.Errorf("AlphaNDCG = %v, want %v", got, tc.want)
			}
		})
	}

	// k is clamped to the ranking length, like RecallAtK: a cutoff past the end
	// of a short list is a full-list score and not an error.
	short := []string{"s1", "s2"}
	if a, b := evalscore.AlphaNDCG(short, pool, 99, 0.5), evalscore.AlphaNDCG(short, pool, 2, 0.5); a != b {
		t.Errorf("k clamp: @99 = %v, @2 = %v, want equal", a, b)
	}
	// α outside [0,1] is clamped, not propagated into math.Pow.
	if a, b := evalscore.AlphaNDCG(short, pool, 2, -1), evalscore.AlphaNDCG(short, pool, 2, 0); a != b {
		t.Errorf("alpha clamp low: %v vs %v", a, b)
	}
	if a, b := evalscore.AlphaNDCG(short, pool, 2, 7), evalscore.AlphaNDCG(short, pool, 2, 1); a != b {
		t.Errorf("alpha clamp high: %v vs %v", a, b)
	}
	if evalscore.AlphaNDCGDefault != 0.5 {
		t.Errorf("AlphaNDCGDefault = %v, want 0.5", evalscore.AlphaNDCGDefault)
	}
}

// TestAlphaNDCGIdealRankingScoresOne is the normalisation gate: a ranking that
// IS the ideal ordering scores exactly 1.0. The ideal is established by brute
// force over every permutation with the independent rawAlphaDCG above, so the
// check does not simply reproduce the greedy construction under test.
func TestAlphaNDCGIdealRankingScoresOne(t *testing.T) {
	t.Run("single-facet pool: every full ordering is ideal", func(t *testing.T) {
		pool := map[string][]string{"a": {"f1"}, "b": {"f2"}, "c": {"f3"}}
		for _, p := range permutations([]string{"a", "b", "c"}) {
			if got := evalscore.AlphaNDCG(p, pool, 3, 0.5); math.Abs(got-1.0) > 1e-12 {
				t.Errorf("AlphaNDCG(%v) = %v, want 1", p, got)
			}
		}
	})

	t.Run("multi-facet pool: the brute-forced best ordering is ideal", func(t *testing.T) {
		pool := map[string][]string{"x": {"f1", "f2"}, "y": {"f3"}, "z": {"f1"}}
		ids := []string{"x", "y", "z"}
		best, _ := bestOrder(ids, pool, 3, 0.5)
		if got := evalscore.AlphaNDCG(best, pool, 3, 0.5); math.Abs(got-1.0) > 1e-12 {
			t.Errorf("AlphaNDCG(best=%v) = %v, want 1", best, got)
		}
		// Every other ordering must be strictly below it — this is the metric
		// doing its one job: front-loading novelty pays.
		for _, p := range permutations(ids) {
			got := evalscore.AlphaNDCG(p, pool, 3, 0.5)
			if got > 1.0+1e-12 {
				t.Errorf("AlphaNDCG(%v) = %v exceeds the ideal", p, got)
			}
		}
	})
}

// TestAlphaNDCGDisplacement is the design gate of M-W3a (design/05 §4.1b): a
// derived block that sits in the top-5 together with three of its own source
// blocks and pushes a fresh facet out of the window. nDCG cannot see it —
// every hit is relevant — while α-nDCG and S-Recall both move.
func TestAlphaNDCGDisplacement(t *testing.T) {
	pool, aspects := displacementFixture()
	// The base condition has no derived block at all, so it is judged against
	// the pool that exists in it.
	basePool := map[string][]string{}
	for id, f := range pool {
		if id != "D" {
			basePool[id] = f
		}
	}

	base := []string{"s1", "s2", "s3", "s4", "s5"}
	// D displaces s5 out of the top-5 and shares the window with s1..s3, the
	// three sources whose facets it already carries.
	condWasteful := []string{"D", "s1", "s2", "s3", "s4"}
	// Same six documents, same D at rank 1, but the window spends its
	// remaining positions on facets D does not carry.
	condTight := []string{"D", "s4", "s5", "s1", "s2"}

	// (1) nDCG is blind: both rankings hold five relevant ids in the top-5.
	for _, tc := range []struct {
		name   string
		ranked []string
	}{{"base", base}, {"cond wasteful", condWasteful}, {"cond tight", condTight}} {
		if got := evalscore.NDCGRanked(tc.ranked, goldSet(tc.ranked...), 5); math.Abs(got-1.0) > 1e-12 {
			t.Errorf("NDCGRanked(%s) = %v, want 1 — the blindness this wave is about", tc.name, got)
		}
	}

	// (2) α-nDCG sees it. The greedy ideal over the six-document pool spends
	// its five positions on D (three fresh facets), s4, s5 and then two
	// already-served sources at (1-α) each.
	ideal := 3*discount(0) + 1*discount(1) + 1*discount(2) + 0.5*discount(3) + 0.5*discount(4)
	wastefulDCG := 3*discount(0) + 0.5*discount(1) + 0.5*discount(2) + 0.5*discount(3) + 1*discount(4)
	wantWasteful := wastefulDCG / ideal

	gotBase := evalscore.AlphaNDCG(base, basePool, 5, evalscore.AlphaNDCGDefault)
	gotWasteful := evalscore.AlphaNDCG(condWasteful, pool, 5, evalscore.AlphaNDCGDefault)
	gotTight := evalscore.AlphaNDCG(condTight, pool, 5, evalscore.AlphaNDCGDefault)

	if math.Abs(gotBase-1.0) > 1e-12 {
		t.Errorf("base α-nDCG = %v, want 1 (five disjoint facets in order)", gotBase)
	}
	if math.Abs(gotTight-1.0) > 1e-12 {
		t.Errorf("tight α-nDCG = %v, want 1 (it reproduces the ideal)", gotTight)
	}
	if math.Abs(gotWasteful-wantWasteful) > 1e-12 {
		t.Errorf("wasteful α-nDCG = %v, want %v", gotWasteful, wantWasteful)
	}
	if !(gotWasteful < gotBase) {
		t.Errorf("displacement invisible: wasteful α-nDCG %v is not below base %v", gotWasteful, gotBase)
	}
	if !(gotWasteful < gotTight) {
		t.Errorf("redundancy unpunished: wasteful α-nDCG %v is not below tight %v", gotWasteful, gotTight)
	}

	// (3) S-Recall shows the same displacement as lost coverage: the fifth
	// facet has no block left inside the window.
	if got := evalscore.SRecallAtK(base, aspects, 5); math.Abs(got-1.0) > 1e-12 {
		t.Errorf("base S-Recall@5 = %v, want 1", got)
	}
	if got := evalscore.SRecallAtK(condWasteful, aspects, 5); math.Abs(got-0.8) > 1e-12 {
		t.Errorf("wasteful S-Recall@5 = %v, want 0.8", got)
	}
	if got := evalscore.SRecallAtK(condTight, aspects, 5); math.Abs(got-1.0) > 1e-12 {
		t.Errorf("tight S-Recall@5 = %v, want 1", got)
	}
}

// TestAlphaNDCGWithoutRedundancyDiscount is the negative probe belonging to
// the displacement gate. α=0 is exactly an implementation that computes the
// gain WITHOUT the (1-α)^served factor: every facet is worth a full point no
// matter how often it was already served. Under it all three rankings score
// 1.0, the displacement assertions above have nothing left to see, and the
// gate is red — which is the point.
func TestAlphaNDCGWithoutRedundancyDiscount(t *testing.T) {
	pool, _ := displacementFixture()
	basePool := map[string][]string{}
	for id, f := range pool {
		if id != "D" {
			basePool[id] = f
		}
	}
	base := []string{"s1", "s2", "s3", "s4", "s5"}
	condWasteful := []string{"D", "s1", "s2", "s3", "s4"}
	condTight := []string{"D", "s4", "s5", "s1", "s2"}

	gotBase := evalscore.AlphaNDCG(base, basePool, 5, 0)
	gotWasteful := evalscore.AlphaNDCG(condWasteful, pool, 5, 0)
	gotTight := evalscore.AlphaNDCG(condTight, pool, 5, 0)
	for _, tc := range []struct {
		name string
		got  float64
	}{{"base", gotBase}, {"wasteful", gotWasteful}, {"tight", gotTight}} {
		if math.Abs(tc.got-1.0) > 1e-12 {
			t.Errorf("without the redundancy discount %s = %v, want 1", tc.name, tc.got)
		}
	}
	if gotWasteful < gotBase || gotWasteful < gotTight {
		t.Errorf("α=0 still discounts redundancy: wasteful %v, base %v, tight %v", gotWasteful, gotBase, gotTight)
	}
}

// TestAlphaNDCGNeverExceedsGreedyIdeal fuzzes the normalisation over 200
// seeded cases built from SINGLE-facet documents. That class is deliberate:
// there the greedy ideal is provably the true optimum (each document offers
// exactly one gain value, greedy takes the k largest available ones and lays
// them out in descending order against a descending discount — the
// rearrangement inequality), so 0 <= score <= 1 is a structural property and
// not a property of the seed. Rankings carry repeats and unjudged ids, both of
// which can only cost.
func TestAlphaNDCGNeverExceedsGreedyIdeal(t *testing.T) {
	rng := rand.New(rand.NewSource(20260826)) //nolint:gosec // deterministic test fixture, not crypto
	alphas := []float64{0, 0.25, 0.5, 0.75, 1}
	for c := 0; c < 200; c++ {
		pool := map[string][]string{}
		var ids []string
		for f := 0; f < 2+rng.Intn(4); f++ {
			for j := 0; j < 1+rng.Intn(3); j++ {
				id := fmt.Sprintf("d%d_%d", f, j)
				pool[id] = []string{fmt.Sprintf("f%d", f)}
				ids = append(ids, id)
			}
		}
		ranking := append([]string(nil), ids...)
		if rng.Intn(2) == 0 {
			ranking = append(ranking, ranking[rng.Intn(len(ranking))])
		}
		if rng.Intn(2) == 0 {
			ranking = append(ranking, "unjudged")
		}
		rng.Shuffle(len(ranking), func(i, j int) { ranking[i], ranking[j] = ranking[j], ranking[i] })
		k := 1 + rng.Intn(len(ranking)+2)
		alpha := alphas[rng.Intn(len(alphas))]

		got := evalscore.AlphaNDCG(ranking, pool, k, alpha)
		if got < 0 || got > 1.0+1e-12 || math.IsNaN(got) {
			t.Fatalf("case %d: AlphaNDCG(%v, k=%d, alpha=%v) = %v, want [0,1]", c, ranking, k, alpha, got)
		}
	}
}

// TestAlphaNDCGGreedyIdealIsApproximate pins the one property of the greedy
// normaliser that is easy to forget and impossible to fix cheaply: choosing
// the true ideal is NP-hard, greedy is the standard approximation, and it is a
// LOWER bound. On a pool of overlapping multi-facet documents a real ranking
// can therefore land marginally above 1.0. The value is not clamped, so the
// case stays visible instead of being reported as a perfect 1.0.
//
// Pool: a={f1,f2}, b={f1,f3}, c={f2,f4}. Greedy opens with a (gain 2, ties
// broken by ascending id) and is stuck with two 1.5-gain documents afterwards;
// b then c opens with two full 2.0 gains.
func TestAlphaNDCGGreedyIdealIsApproximate(t *testing.T) {
	pool := map[string][]string{"a": {"f1", "f2"}, "b": {"f1", "f3"}, "c": {"f2", "f4"}}
	greedyIdeal := 2*discount(0) + 1.5*discount(1) + 1.5*discount(2)
	realDCG := 2*discount(0) + 2*discount(1) + 1*discount(2)
	want := realDCG / greedyIdeal
	if want <= 1.0 {
		t.Fatalf("fixture no longer demonstrates the greedy gap: want = %v", want)
	}
	if got := evalscore.AlphaNDCG([]string{"b", "c", "a"}, pool, 3, 0.5); math.Abs(got-want) > 1e-12 {
		t.Errorf("AlphaNDCG = %v, want %v (unclamped greedy gap)", got, want)
	}
	// The true optimum found by brute force is exactly that ranking, so the
	// gap is the normaliser's, not the ranking's.
	best, _ := bestOrder([]string{"a", "b", "c"}, pool, 3, 0.5)
	if len(best) != 3 || best[0] != "b" || best[1] != "c" || best[2] != "a" {
		t.Errorf("brute-forced optimum = %v, want [b c a]", best)
	}
}

// TestAlphaNDCGNormaliserIgnoresTheRanking is what makes a base/cond
// comparison sound despite the greedy gap: the denominator depends on the
// judgements, k and α only. Two rankings judged against the same map are
// therefore comparable even where the absolute value is off.
func TestAlphaNDCGNormaliserIgnoresTheRanking(t *testing.T) {
	pool, _ := displacementFixture()
	for _, r := range [][]string{
		{"D", "s1", "s2", "s3", "s4"},
		{"s5", "s4", "s3", "s2", "s1"},
		{"D", "D", "D", "D", "D"},
	} {
		got := evalscore.AlphaNDCG(r, pool, 5, evalscore.AlphaNDCGDefault)
		want := rawAlphaDCG(r, pool, 5, 0.5) / rawAlphaDCG([]string{"D", "s4", "s5", "s1", "s2"}, pool, 5, 0.5)
		if math.Abs(got-want) > 1e-12 {
			t.Errorf("AlphaNDCG(%v) = %v, want %v (same denominator for every ranking)", r, got, want)
		}
	}
}
