package rrf

import (
	"math"
	"testing"
	"time"
)

// Helper: date from string.
func mustDate(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic("bad date: " + s)
	}
	return t
}

// --- ComputeGravity Tests (10) ---.

func TestComputeGravity_ExactDateMatch(t *testing.T) {
	// source_date == target → dist clamped to 0.5, gives max score
	target := mustDate("2026-03-29")
	dates := []time.Time{target}
	params := GravityParams{
		TargetDate: target,
		Direction:  "both",
		Cutoff:     60,
		Power:      1.5,
	}
	score := ComputeGravity(dates, params)
	// 1 / 0.5^1.8 (future: power*1.2 because distDays==0 >=0)
	if score <= 0 {
		t.Fatalf("expected positive score, got %f", score)
	}
	// With dist=0.5 and effPower=1.8, score = 1/0.5^1.8 ≈ 3.48
	if score < 2.0 {
		t.Fatalf("expected score > 2.0 for exact match, got %f", score)
	}
}

func TestComputeGravity_30DaysAway(t *testing.T) {
	target := mustDate("2026-03-29")
	dates := []time.Time{mustDate("2026-02-27")} // 30 days before
	params := GravityParams{
		TargetDate: target,
		Direction:  "both",
		Cutoff:     60,
		Power:      1.5,
	}
	score := ComputeGravity(dates, params)
	if score <= 0 {
		t.Fatalf("expected positive score, got %f", score)
	}
	// Should be much smaller than exact match
	exactScore := ComputeGravity([]time.Time{target}, params)
	if score >= exactScore {
		t.Fatalf("30-day score (%f) should be less than exact match (%f)", score, exactScore)
	}
}

func TestComputeGravity_BeyondCutoff(t *testing.T) {
	target := mustDate("2026-03-29")
	dates := []time.Time{mustDate("2025-12-20")} // ~100 days before
	params := GravityParams{
		TargetDate: target,
		Direction:  "both",
		Cutoff:     60,
		Power:      1.5,
	}
	score := ComputeGravity(dates, params)
	if score != 0 {
		t.Fatalf("expected 0 for beyond cutoff, got %f", score)
	}
}

func TestComputeGravity_NoDates(t *testing.T) {
	params := GravityParams{
		TargetDate: mustDate("2026-03-29"),
		Direction:  "both",
		Cutoff:     60,
		Power:      1.5,
	}
	score := ComputeGravity(nil, params)
	if score != 0 {
		t.Fatalf("expected 0 for no dates, got %f", score)
	}
}

func TestComputeGravity_DirectionPast_FutureDatesIgnored(t *testing.T) {
	target := mustDate("2026-03-29")
	// One date 5 days in the future — should be ignored with direction=past
	dates := []time.Time{mustDate("2026-04-03")}
	params := GravityParams{
		TargetDate: target,
		Direction:  "past",
		Cutoff:     60,
		Power:      1.5,
	}
	score := ComputeGravity(dates, params)
	if score != 0 {
		t.Fatalf("expected 0 for future date with direction=past, got %f", score)
	}
}

func TestComputeGravity_DirectionFuture_PastDatesIgnored(t *testing.T) {
	target := mustDate("2026-03-29")
	// One date 5 days in the past — should be ignored with direction=future
	dates := []time.Time{mustDate("2026-03-24")}
	params := GravityParams{
		TargetDate: target,
		Direction:  "future",
		Cutoff:     60,
		Power:      1.5,
	}
	score := ComputeGravity(dates, params)
	if score != 0 {
		t.Fatalf("expected 0 for past date with direction=future, got %f", score)
	}
}

func TestComputeGravity_DirectionBoth_AllDatesConsidered(t *testing.T) {
	target := mustDate("2026-03-29")
	dates := []time.Time{
		mustDate("2026-03-24"), // 5 days past
		mustDate("2026-04-03"), // 5 days future
	}
	params := GravityParams{
		TargetDate: target,
		Direction:  "both",
		Cutoff:     60,
		Power:      1.5,
	}
	score := ComputeGravity(dates, params)
	if score <= 0 {
		t.Fatalf("expected positive score for both dates, got %f", score)
	}
	// Both should contribute
	pastOnly := ComputeGravity([]time.Time{mustDate("2026-03-24")}, params)
	futureOnly := ComputeGravity([]time.Time{mustDate("2026-04-03")}, params)
	if math.Abs(score-(pastOnly+futureOnly)) > 0.0001 {
		t.Fatalf("expected sum of individual scores (%f + %f = %f), got %f",
			pastOnly, futureOnly, pastOnly+futureOnly, score)
	}
}

func TestComputeGravity_FutureDecaysFaster(t *testing.T) {
	target := mustDate("2026-03-29")
	// Same absolute distance: 10 days
	pastDate := mustDate("2026-03-19")   // 10 days before
	futureDate := mustDate("2026-04-08") // 10 days after
	params := GravityParams{
		TargetDate: target,
		Direction:  "both",
		Cutoff:     60,
		Power:      1.5,
	}
	pastScore := ComputeGravity([]time.Time{pastDate}, params)
	futureScore := ComputeGravity([]time.Time{futureDate}, params)
	// Future uses power * 1.2, so future score should be lower
	if futureScore >= pastScore {
		t.Fatalf("future score (%f) should be < past score (%f) due to 20%% faster decay",
			futureScore, pastScore)
	}
}

func TestComputeGravity_MultipleDates(t *testing.T) {
	target := mustDate("2026-03-29")
	dates := []time.Time{
		mustDate("2026-03-28"), // 1 day before
		mustDate("2026-03-27"), // 2 days before
		mustDate("2026-03-26"), // 3 days before
	}
	params := GravityParams{
		TargetDate: target,
		Direction:  "both",
		Cutoff:     60,
		Power:      1.5,
	}
	score := ComputeGravity(dates, params)
	// Sum of 3 individual contributions
	sum := 0.0
	for _, d := range dates {
		sum += ComputeGravity([]time.Time{d}, params)
	}
	if math.Abs(score-sum) > 0.0001 {
		t.Fatalf("expected sum %f, got %f", sum, score)
	}
}

func TestComputeGravity_MinDistance(t *testing.T) {
	target := mustDate("2026-03-29")
	// Same date → distance 0, clamped to 0.5
	dates := []time.Time{target}
	params := GravityParams{
		TargetDate: target,
		Direction:  "both",
		Cutoff:     60,
		Power:      1.5,
	}
	score := ComputeGravity(dates, params)
	// effPower = 1.5 * 1.2 = 1.8 (distDays=0, which is >=0)
	expected := 1.0 / math.Pow(0.5, 1.8)
	if math.Abs(score-expected) > 0.001 {
		t.Fatalf("expected %f (1/0.5^1.8), got %f", expected, score)
	}
}

// --- ApplyGravityBoost Tests (7) ---.

func makeResults(ids ...string) []SearchResult {
	results := make([]SearchResult, len(ids))
	for i, id := range ids {
		results[i] = SearchResult{
			ID:       id,
			RRFScore: float64(len(ids)-i) * 0.01, // descending: 0.03, 0.02, 0.01
		}
	}
	return results
}

func TestApplyGravityBoost_EmptyResults(t *testing.T) {
	result := ApplyGravityBoost(nil, nil, GravityParams{BoostWeight: 0.30})
	if len(result) != 0 {
		t.Fatalf("expected empty, got %d results", len(result))
	}
}

func TestApplyGravityBoost_NoTemporalBlocks(t *testing.T) {
	results := makeResults("a", "b", "c")
	blockDates := map[string][]time.Time{} // no dates for any block
	params := GravityParams{
		TargetDate:  mustDate("2026-03-29"),
		Direction:   "both",
		Cutoff:      60,
		Power:       1.5,
		BoostWeight: 0.30,
	}
	boosted := ApplyGravityBoost(results, blockDates, params)
	if len(boosted) != 3 {
		t.Fatalf("expected 3 results, got %d", len(boosted))
	}
	// All gravity=0, so factor=1.0 for all, order preserved
	for i, r := range boosted {
		if r.ID != results[i].ID {
			t.Fatalf("order changed at index %d: expected %s, got %s", i, results[i].ID, r.ID)
		}
	}
}

func TestApplyGravityBoost_SingleTemporalBlock(t *testing.T) {
	// "c" has lowest RRF but closest date → should move up
	results := makeResults("a", "b", "c") // scores: 0.03, 0.02, 0.01
	blockDates := map[string][]time.Time{
		"c": {mustDate("2026-03-29")}, // exact match
	}
	params := GravityParams{
		TargetDate:  mustDate("2026-03-29"),
		Direction:   "both",
		Cutoff:      60,
		Power:       1.5,
		BoostWeight: 0.30,
	}
	boosted := ApplyGravityBoost(results, blockDates, params)
	// "c" gets factor 1.30 → 0.01*1.30 = 0.013
	// "a" gets factor 1.0 → 0.03
	// "b" gets factor 1.0 → 0.02
	// Order should still be a, b, c because 0.03 > 0.02 > 0.013
	// But let's verify the boost happened
	var cResult *SearchResult
	for i := range boosted {
		if boosted[i].ID == "c" {
			cResult = &boosted[i]
			break
		}
	}
	if cResult == nil {
		t.Fatal("result 'c' not found in boosted results")
	}
	if cResult.RRFScoreOriginal == nil {
		t.Fatal("RRFScoreOriginal should be set for 'c'")
	}
	if *cResult.RRFScoreOriginal != 0.01 {
		t.Fatalf("expected original score 0.01, got %f", *cResult.RRFScoreOriginal)
	}
	// Boosted score should be > original
	if cResult.RRFScore <= *cResult.RRFScoreOriginal {
		t.Fatalf("boosted score (%f) should be > original (%f)", cResult.RRFScore, *cResult.RRFScoreOriginal)
	}
}

func TestApplyGravityBoost_NormalizesToMaxGravity(t *testing.T) {
	results := makeResults("a", "b")
	blockDates := map[string][]time.Time{
		"a": {mustDate("2026-03-29")}, // exact match (max gravity)
		"b": {mustDate("2026-03-20")}, // 9 days away (less gravity)
	}
	params := GravityParams{
		TargetDate:  mustDate("2026-03-29"),
		Direction:   "both",
		Cutoff:      60,
		Power:       1.5,
		BoostWeight: 0.30,
	}
	boosted := ApplyGravityBoost(results, blockDates, params)
	// "a" should get the full boost (gravity/maxGrav = 1.0) → factor = 1.30
	var aResult SearchResult
	for _, r := range boosted {
		if r.ID == "a" {
			aResult = r
			break
		}
	}
	expectedA := 0.02 * 1.30 // original score * max boost
	if math.Abs(aResult.RRFScore-expectedA) > 0.001 {
		t.Fatalf("expected 'a' score ~%f (full boost), got %f", expectedA, aResult.RRFScore)
	}
}

func TestApplyGravityBoost_BoostWeight030(t *testing.T) {
	results := makeResults("a", "b")
	blockDates := map[string][]time.Time{
		"a": {mustDate("2026-03-29")},
		"b": {mustDate("2026-03-29")},
	}
	params := GravityParams{
		TargetDate:  mustDate("2026-03-29"),
		Direction:   "both",
		Cutoff:      60,
		Power:       1.5,
		BoostWeight: 0.30,
	}
	boosted := ApplyGravityBoost(results, blockDates, params)
	for _, r := range boosted {
		if r.RRFScoreOriginal == nil {
			t.Fatalf("RRFScoreOriginal not set for %s", r.ID)
		}
		factor := r.RRFScore / *r.RRFScoreOriginal
		if factor < 1.0 || factor > 1.30+0.001 {
			t.Fatalf("boost factor for %s = %f, expected in [1.0, 1.30]", r.ID, factor)
		}
	}
}

func TestApplyGravityBoost_OrderPreservedWhenNoTemporal(t *testing.T) {
	results := makeResults("x", "y", "z")
	blockDates := map[string][]time.Time{} // empty
	params := GravityParams{
		TargetDate:  mustDate("2026-03-29"),
		Direction:   "both",
		Cutoff:      60,
		Power:       1.5,
		BoostWeight: 0.30,
	}
	boosted := ApplyGravityBoost(results, blockDates, params)
	for i := range boosted {
		if boosted[i].ID != results[i].ID {
			t.Fatalf("order changed at %d: expected %s, got %s", i, results[i].ID, boosted[i].ID)
		}
	}
}

func TestApplyGravityBoost_OriginalScorePreserved(t *testing.T) {
	results := makeResults("a", "b")
	originalScores := make([]float64, len(results))
	for i, r := range results {
		originalScores[i] = r.RRFScore
	}
	blockDates := map[string][]time.Time{
		"a": {mustDate("2026-03-29")},
	}
	params := GravityParams{
		TargetDate:  mustDate("2026-03-29"),
		Direction:   "both",
		Cutoff:      60,
		Power:       1.5,
		BoostWeight: 0.30,
	}
	boosted := ApplyGravityBoost(results, blockDates, params)
	for i, r := range boosted {
		if r.RRFScoreOriginal == nil {
			t.Fatalf("RRFScoreOriginal not set for result %d (%s)", i, r.ID)
		}
		// Find original score by ID
		var expected float64
		for j, orig := range results {
			if orig.ID == r.ID {
				expected = originalScores[j]
				break
			}
		}
		if math.Abs(*r.RRFScoreOriginal-expected) > 0.0001 {
			t.Fatalf("RRFScoreOriginal for %s: expected %f, got %f", r.ID, expected, *r.RRFScoreOriginal)
		}
	}
}
