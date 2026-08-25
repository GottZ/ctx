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

func TestComputeGravity_BackwardPowerScalesWithAge(t *testing.T) {
	// Forward Telescoping (Branch Y / B4 #2): older blocks get wider decay,
	// so a single past-date scoring increases with age (within cutoff).
	// power(30d) = 1.5 / (1 + 0.3*ln(1+1)) = 1.5 / 1.208 ≈ 1.241
	// power(180d) = 1.5 / (1 + 0.3*ln(7)) = 1.5 / 1.583 ≈ 0.948
	target := mustDate("2026-03-29")
	params := GravityParams{
		TargetDate: target,
		Direction:  "past",
		Cutoff:     365,
		Power:      1.5,
	}

	cases := []struct {
		name      string
		date      time.Time
		minPower  float64 // assert effPower below this
		maxPower  float64 // assert effPower above this
	}{
		{"30d", target.Add(-30 * 24 * time.Hour), 1.20, 1.30},
		{"180d", target.Add(-180 * 24 * time.Hour), 0.92, 1.00},
		{"365d", target.Add(-365 * 24 * time.Hour), 0.83, 0.92},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			score := ComputeGravity([]time.Time{c.date}, params)
			// Reverse-engineer the effective power from the score.
			// score = 1 / dist^p → p = -log(score) / log(dist)
			distDays := -c.date.Sub(target).Hours() / 24.0
			expectedP := -math.Log(score) / math.Log(distDays)
			if expectedP < c.minPower || expectedP > c.maxPower {
				t.Errorf("%s effPower=%v not in [%v, %v] (score=%v)",
					c.name, expectedP, c.minPower, c.maxPower, score)
			}
		})
	}
}

func TestComputeGravity_OlderBoostedMoreThanLinear(t *testing.T) {
	// 90-day-old block under power-only=1.5 vs Branch-Y age-scaled.
	// Pre-Y:  score = 1 / 90^1.5  ≈ 0.00117
	// Post-Y: power(90d) = 1.5 / (1 + 0.3*ln(4)) = 1.5 / 1.416 ≈ 1.060
	//         score = 1 / 90^1.060 ≈ 0.00919
	// Both correctly under cutoff=365.
	target := mustDate("2026-03-29")
	dates := []time.Time{target.Add(-90 * 24 * time.Hour)}
	params := GravityParams{
		TargetDate: target,
		Direction:  "past",
		Cutoff:     365,
		Power:      1.5,
	}
	score := ComputeGravity(dates, params)
	// Old code would give 1/90^1.5 ≈ 0.00117 — Branch Y must be higher.
	if score < 0.005 {
		t.Errorf("90d-old block gravity=%v, expected >0.005 (forward telescoping should boost older)", score)
	}
}

func TestComputeGravity_ForwardUnchangedFromBranchY(t *testing.T) {
	// Branch Y only modifies backward decay. Forward keeps power*1.2 = 1.8.
	target := mustDate("2026-03-29")
	dates := []time.Time{target.Add(10 * 24 * time.Hour)} // 10d future
	params := GravityParams{
		TargetDate: target,
		Direction:  "future",
		Cutoff:     60,
		Power:      1.5,
	}
	score := ComputeGravity(dates, params)
	expected := 1.0 / math.Pow(10, 1.8) // ≈ 0.01585
	if math.Abs(score-expected) > 1e-4 {
		t.Errorf("forward 10d gravity=%v, want %v (1/10^1.8 unchanged)", score, expected)
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

// TestApplyGravityBoost_ExactHitGetsFullBoost was
// TestApplyGravityBoost_NormalizesToMaxGravity until the boost stopped being
// normalized against the strongest candidate in the window (Issue #40,
// finding 3). What it asserts is unchanged and still holds: a block sitting on
// the target date earns the whole boost. Only the reason changed — the block
// now reaches the absolute anchor instead of defining it.
func TestApplyGravityBoost_ExactHitGetsFullBoost(t *testing.T) {
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
	// "a" should get the full boost (gravity/anchor = 1.0) → factor = 1.30
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

// --- Cyclic Gravity Tests (GottZ Cyclic Phase Model) ---.

func TestCyclicDistance(t *testing.T) {
	tests := []struct {
		name     string
		a, b     float64
		expected float64
	}{
		{"identical", 0.5, 0.5, 0.0},
		{"small_direct", 0.1, 0.2, 0.1},
		{"large_wraps", 0.9, 0.1, 0.2},   // direct 0.8, wrapped 0.2
		{"opposite", 0.0, 0.5, 0.5},      // maximum distance
		{"monday_sunday", 0.0, 6.0 / 7.0, 1.0 / 7.0}, // they're neighbors
		{"monday_wednesday", 0.0, 2.0 / 7.0, 2.0 / 7.0},
		{"december_february", 11.0 / 12.0, 1.0 / 12.0, 2.0 / 12.0}, // month wrap
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CyclicDistance(tt.a, tt.b)
			if math.Abs(got-tt.expected) > 1e-9 {
				t.Errorf("CyclicDistance(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.expected)
			}
		})
	}
}

func TestGaussianDecay(t *testing.T) {
	tests := []struct {
		name      string
		distance  float64
		sigma     float64
		expected  float64 // approximate
		tolerance float64
	}{
		{"zero_distance", 0.0, 0.07, 1.0, 1e-9},
		{"one_sigma", 0.07, 0.07, 0.6065, 1e-3},                    // exp(-0.5)
		{"two_sigma", 0.14, 0.07, 0.1353, 1e-3},                    // exp(-2)
		{"weekday_adjacent", 1.0 / 7.0, 0.07, 0.128, 1e-2},         // ~0.128
		{"weekday_two_apart", 2.0 / 7.0, 0.07, 2.7e-4, 5e-4},       // ~0.00027
		{"zero_sigma_guard", 0.1, 0.0, 0.0, 1e-9},                  // guard against div0
		{"opposite_cycle", 0.5, 0.07, 1.3e-11, 1e-10},              // ~0
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GaussianDecay(tt.distance, tt.sigma)
			if math.Abs(got-tt.expected) > tt.tolerance {
				t.Errorf("GaussianDecay(%v, %v) = %v, want ≈%v (tol %v)",
					tt.distance, tt.sigma, got, tt.expected, tt.tolerance)
			}
		})
	}
}

func TestDimensionPhase(t *testing.T) {
	tests := []struct {
		dim, val string
		want     float64
		wantErr  bool
	}{
		{"weekday", "1", 0.0, false},            // Monday
		{"weekday", "2", 1.0 / 7.0, false},      // Tuesday
		{"weekday", "7", 6.0 / 7.0, false},      // Sunday
		{"month", "1", 0.0, false},              // January
		{"month", "12", 11.0 / 12.0, false},     // December
		{"quarter", "1", 0.0, false},
		{"quarter", "4", 0.75, false},
		{"week", "1", 0.0, false},
		{"week", "13", 12.0 / 52.0, false},
		{"monthday", "1", 0.0, false},
		{"monthday", "15", 14.0 / 30.0, false},  // mid-month
		{"monthday", "31", 1.0, false}, // end (30/30)
		{"seasonal", "1", 0.0, false},            // Jan 1
		{"seasonal", "365", 364.0 / 365.0, false}, // Dec 31 non-leap
		{"seasonal", "366", 1.0, false}, // Dec 31 leap (365/365)
		{"daily", "0", 0.0, false},       // midnight
		{"daily", "12", 0.5, false},      // noon
		{"daily", "23", 23.0 / 24.0, false}, // 11pm
		// Errors
		{"weekday", "0", 0.0, true},
		{"weekday", "8", 0.0, true},
		{"month", "13", 0.0, true},
		{"monthday", "0", 0.0, true},
		{"monthday", "32", 0.0, true},
		{"seasonal", "0", 0.0, true},
		{"seasonal", "367", 0.0, true},
		{"daily", "-1", 0.0, true},
		{"daily", "24", 0.0, true},
		{"year", "2026", 0.0, true}, // not cyclic
		{"unknown", "1", 0.0, true},
		{"weekday", "abc", 0.0, true},
	}
	for _, tt := range tests {
		t.Run(tt.dim+"_"+tt.val, func(t *testing.T) {
			got, err := DimensionPhase(tt.dim, tt.val)
			if tt.wantErr {
				if err == nil {
					t.Errorf("DimensionPhase(%q, %q) expected error, got %v", tt.dim, tt.val, got)
				}
				return
			}
			if err != nil {
				t.Errorf("DimensionPhase(%q, %q) unexpected error: %v", tt.dim, tt.val, err)
			}
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("DimensionPhase(%q, %q) = %v, want %v", tt.dim, tt.val, got, tt.want)
			}
		})
	}
}

func TestQueryPhase(t *testing.T) {
	// Tuesday, 2026-03-31, March, Q1, ISO week 14, day 31, day-of-year 90
	tuesday := mustDate("2026-03-31")
	tests := []struct {
		dim  string
		want float64
	}{
		{"weekday", 1.0 / 7.0},   // ISO Tuesday=2 → (2-1)/7
		{"month", 2.0 / 12.0},    // March=3 → 2/12
		{"quarter", 0.0},         // Q1 → (1-1)/4
		{"week", 13.0 / 52.0},    // week 14 → 13/52
		{"monthday", 1.0},          // day 31 → 30/30
		{"seasonal", 89.0 / 365.0}, // day-of-year 90 → 89/365
	}
	for _, tt := range tests {
		t.Run(tt.dim, func(t *testing.T) {
			got, err := QueryPhase(tt.dim, tuesday)
			if err != nil {
				t.Fatalf("QueryPhase(%q) error: %v", tt.dim, err)
			}
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("QueryPhase(%q, Tuesday) = %v, want %v", tt.dim, got, tt.want)
			}
		})
	}

	// Daily dimension requires hour info
	tueAfternoon := time.Date(2026, 3, 31, 14, 30, 0, 0, time.UTC)
	if p, _ := QueryPhase("daily", tueAfternoon); math.Abs(p-14.0/24.0) > 1e-9 {
		t.Errorf("daily phase 14:30 = %v, want %v", p, 14.0/24.0)
	}

	// Sunday ISO=7 edge case
	sunday := mustDate("2026-03-29")
	p, _ := QueryPhase("weekday", sunday)
	if math.Abs(p-6.0/7.0) > 1e-9 {
		t.Errorf("Sunday weekday phase = %v, want %v", p, 6.0/7.0)
	}
}

func TestComputeCyclicGravity_WalkThrough(t *testing.T) {
	// Walk-through from the vision spec: "immer dienstags" query, target=Tuesday.
	// Tuesday block → gravity ≈ 1.0
	// Wednesday block → gravity ≈ 0.128
	// Saturday block → gravity ≈ ~0 (almost opposite on the 7-cycle)

	tuesday := mustDate("2026-03-31")
	dimWeights := map[string]float64{"weekday": 1.0}

	tests := []struct {
		name     string
		dims     []TemporalDim
		expected float64
		tolerance float64
	}{
		{
			name:      "tuesday_block_exact_match",
			dims:      []TemporalDim{{"weekday", "2"}, {"month", "3"}},
			expected:  1.0,
			tolerance: 1e-9,
		},
		{
			name:      "wednesday_block_adjacent",
			dims:      []TemporalDim{{"weekday", "3"}, {"month", "3"}},
			expected:  0.128,
			tolerance: 0.005,
		},
		{
			name:      "monday_block_adjacent",
			dims:      []TemporalDim{{"weekday", "1"}},
			expected:  0.128, // same distance as Wednesday (1/7)
			tolerance: 0.005,
		},
		{
			name:      "thursday_block_two_apart",
			dims:      []TemporalDim{{"weekday", "4"}},
			expected:  0.000268, // GaussianDecay(2/7, 0.07) ≈ 0.000268
			tolerance: 1e-4,
		},
		{
			name: "saturday_block_far",
			dims: []TemporalDim{{"weekday", "6"}},
			// Tue phase=1/7, Sat phase=5/7 → dist = min(4/7, 3/7) = 3/7 ≈ 0.4286
			// GaussianDecay(3/7, 0.07) = exp(-18.74) ≈ 7.25e-9
			// Demonstrates sharp sigma=0.07 cutoff: ≥3 weekdays away = effectively zero.
			expected:  7.25e-9,
			tolerance: 1e-8,
		},
		{
			name:      "no_weekday_dim",
			dims:      []TemporalDim{{"month", "3"}},
			expected:  0.0, // no weekday match
			tolerance: 1e-9,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeCyclicGravity(dimWeights, tt.dims, tuesday)
			if math.Abs(got-tt.expected) > tt.tolerance {
				t.Errorf("ComputeCyclicGravity() = %v, want %v ± %v", got, tt.expected, tt.tolerance)
			}
		})
	}
}

func TestComputeCyclicGravity_MultiDim(t *testing.T) {
	// "am Dienstag im März" → {"linear": 0.5, "weekday": 0.3, "month": 0.2}
	// target: Tuesday 2026-03-31 (March)
	tuesday := mustDate("2026-03-31")
	dimWeights := map[string]float64{
		"linear":  0.5, // ignored by ComputeCyclicGravity
		"weekday": 0.3,
		"month":   0.2,
	}

	// Block: Tuesday in March → both match exactly
	// gravity = 0.3 * 1.0 + 0.2 * 1.0 = 0.5
	dims := []TemporalDim{{"weekday", "2"}, {"month", "3"}}
	got := ComputeCyclicGravity(dimWeights, dims, tuesday)
	if math.Abs(got-0.5) > 1e-9 {
		t.Errorf("full match: got %v, want 0.5", got)
	}

	// Block: Tuesday in September → weekday matches, month off by 6
	dims2 := []TemporalDim{{"weekday", "2"}, {"month", "9"}}
	got2 := ComputeCyclicGravity(dimWeights, dims2, tuesday)
	// weekday contributes 0.3 * 1.0 = 0.3
	// month: dist = min(|2/12-8/12|, 1 - |...|) = min(0.5, 0.5) = 0.5
	//        decay = GaussianDecay(0.5, 0.10) ≈ e^-12.5 ≈ 3.7e-6
	// total ≈ 0.3
	if math.Abs(got2-0.3) > 1e-3 {
		t.Errorf("weekday-match, month-far: got %v, want ≈0.3", got2)
	}
}

func TestComputeCyclicGravity_BestMatchPerDimension(t *testing.T) {
	// Block has MULTIPLE weekday entries (meeting Mon + result Tue).
	// Query "immer dienstags" → should take BEST match (Tue), not sum.
	tuesday := mustDate("2026-03-31")
	dimWeights := map[string]float64{"weekday": 1.0}
	dims := []TemporalDim{
		{"weekday", "1"}, // Monday
		{"weekday", "2"}, // Tuesday — best match
		{"weekday", "4"}, // Thursday
	}
	got := ComputeCyclicGravity(dimWeights, dims, tuesday)
	if math.Abs(got-1.0) > 1e-9 {
		t.Errorf("best-match: got %v, want 1.0 (Tuesday best)", got)
	}
}

func TestComputeCyclicGravity_LinearOnly(t *testing.T) {
	// When dimWeights only has "linear", cyclic gravity returns 0
	// (linear is handled by ApplyGravityBoost, not this function).
	dimWeights := map[string]float64{"linear": 1.0}
	dims := []TemporalDim{{"weekday", "2"}, {"month", "3"}}
	got := ComputeCyclicGravity(dimWeights, dims, mustDate("2026-03-31"))
	if got != 0 {
		t.Errorf("linear-only: got %v, want 0", got)
	}
}

func TestComputeCyclicGravity_EmptyInputs(t *testing.T) {
	now := mustDate("2026-03-31")
	// Empty weights
	if g := ComputeCyclicGravity(nil, []TemporalDim{{"weekday", "2"}}, now); g != 0 {
		t.Errorf("nil weights: got %v, want 0", g)
	}
	// Empty dims
	if g := ComputeCyclicGravity(map[string]float64{"weekday": 1.0}, nil, now); g != 0 {
		t.Errorf("nil dims: got %v, want 0", g)
	}
}

func TestApplyCyclicGravityBoost(t *testing.T) {
	// 3 blocks: Tuesday block (strong match), Wednesday block (weak), no-dim block (neutral).
	tuesday := mustDate("2026-03-31")
	dimWeights := map[string]float64{"weekday": 1.0}

	results := []SearchResult{
		{ID: "no-dim", RRFScore: 0.100},
		{ID: "tue-block", RRFScore: 0.080},
		{ID: "wed-block", RRFScore: 0.090},
	}
	blockDims := map[string][]TemporalDim{
		"tue-block": {{"weekday", "2"}},
		"wed-block": {{"weekday", "3"}},
		// no-dim has no entry
	}

	boosted := ApplyCyclicGravityBoost(results, blockDims, dimWeights, tuesday, 0.30)

	// Expected ordering: Tuesday block should rise above Wednesday block and no-dim
	// tue-block: 0.080 * (1 + 0.30 * 1.0/1.0) = 0.104
	// wed-block: 0.090 * (1 + 0.30 * 0.128/1.0) ≈ 0.0935
	// no-dim: 0.100 * (1 + 0.30 * 0/1.0) = 0.100
	if boosted[0].ID != "tue-block" {
		t.Errorf("expected tue-block first, got %s", boosted[0].ID)
	}
	if boosted[2].ID != "wed-block" {
		t.Errorf("expected wed-block last, got %s (order: %s, %s, %s)",
			boosted[2].ID, boosted[0].ID, boosted[1].ID, boosted[2].ID)
	}

	// Original scores preserved
	for _, r := range boosted {
		if r.RRFScoreOriginal == nil {
			t.Errorf("RRFScoreOriginal not set for %s", r.ID)
		}
	}
}

func TestApplyCyclicGravityBoost_NoOp(t *testing.T) {
	results := []SearchResult{{ID: "a", RRFScore: 0.1}}
	// Zero boost weight → no change
	boosted := ApplyCyclicGravityBoost(results, nil, map[string]float64{"weekday": 1.0}, mustDate("2026-03-31"), 0.0)
	if len(boosted) != 1 || boosted[0].RRFScore != 0.1 {
		t.Errorf("zero boost weight should no-op")
	}
	// Empty results
	empty := ApplyCyclicGravityBoost(nil, nil, map[string]float64{"weekday": 1.0}, mustDate("2026-03-31"), 0.3)
	if len(empty) != 0 {
		t.Errorf("empty results should return empty")
	}
}
