package rrf

import (
	"math"
	"testing"
	"time"
)

// GravityParams is an exported type with four optional fields, so every field
// carries a zero-value contract whether or not it is written down. These tests
// pin all four (Issue #40, findings 2/8c) so a later caller that omits a field
// gets the documented behavior instead of a silent surprise.

// zeroTarget is the target date shared by the zero-value fixtures.
func zeroTarget() time.Time { return mustDate("2026-03-29") }

// zeroFixtureResults returns four results whose scores sit close enough
// together that a 0.30 boost reorders them — a fixture that flattens every
// difference would pass no matter what the params do.
func zeroFixtureResults() []SearchResult {
	return []SearchResult{
		{ID: "a", RRFScore: 0.100}, // no dates → neutral
		{ID: "b", RRFScore: 0.090}, // 40 days in the past
		{ID: "c", RRFScore: 0.080}, // 5 days in the future
		{ID: "d", RRFScore: 0.079}, // exactly on target → max gravity
	}
}

// zeroFixtureDates spans both directions and both sides of the 14-day
// production cutoff, so a direction filter and a cutoff filter each leave a
// visible trace.
func zeroFixtureDates() map[string][]time.Time {
	return map[string][]time.Time{
		"b": {mustDate("2026-02-17")}, // target - 40d
		"c": {mustDate("2026-04-03")}, // target + 5d
		"d": {zeroTarget()},           // target + 0d
	}
}

// assertSameRanking compares two rerankings by ID order and boosted score.
func assertSameRanking(t *testing.T, got, want []SearchResult, what string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: length %d, want %d", what, len(got), len(want))
	}
	for i := range got {
		if got[i].ID != want[i].ID {
			t.Fatalf("%s: rank %d is %q, want %q (got order %s, want order %s)",
				what, i, got[i].ID, want[i].ID, idsOf(got), idsOf(want))
		}
		if math.Abs(got[i].RRFScore-want[i].RRFScore) > 1e-12 {
			t.Fatalf("%s: score of %q is %.12f, want %.12f",
				what, got[i].ID, got[i].RRFScore, want[i].RRFScore)
		}
	}
}

func idsOf(rs []SearchResult) string {
	out := ""
	for _, r := range rs {
		out += r.ID + " "
	}
	return out
}

// TestGravityParams_ZeroValueContract pins the documented zero-value semantics
// of every GravityParams field. Each case leaves one field at its zero value
// and compares against reference params that spell the same contract out
// explicitly; identical output is the contract.
func TestGravityParams_ZeroValueContract(t *testing.T) {
	target := zeroTarget()

	cases := []struct {
		name     string
		contract string
		zero     GravityParams
		// ref spells the contract out explicitly. Ignored when wantOff.
		ref GravityParams
		// wantOff: the contract is "gravity off", so the expected output is
		// the input, untouched — no reorder, no score change, no
		// RRFScoreOriginal.
		wantOff bool
	}{
		{
			name:     "direction_empty_means_no_direction_filter",
			contract: `Direction "" keeps past and future dates, like "both"`,
			zero:     GravityParams{TargetDate: target, Cutoff: 60, Power: 1.5, BoostWeight: 0.30},
			ref:      GravityParams{TargetDate: target, Direction: "both", Cutoff: 60, Power: 1.5, BoostWeight: 0.30},
		},
		{
			name:     "power_zero_means_default_1_5",
			contract: "Power 0 applies the 1.5 default falloff exponent",
			zero:     GravityParams{TargetDate: target, Direction: "both", Cutoff: 60, BoostWeight: 0.30},
			ref:      GravityParams{TargetDate: target, Direction: "both", Cutoff: 60, Power: 1.5, BoostWeight: 0.30},
		},
		{
			name:     "boostweight_zero_means_off",
			contract: "BoostWeight 0 disables the reranker; results pass through untouched",
			zero:     GravityParams{TargetDate: target, Direction: "both", Cutoff: 60, Power: 1.5},
			wantOff:  true,
		},
		{
			name:     "cutoff_zero_means_no_cutoff_filter",
			contract: "Cutoff 0 applies no distance filter, like an unbounded cutoff",
			zero:     GravityParams{TargetDate: target, Direction: "both", Power: 1.5, BoostWeight: 0.30},
			ref:      GravityParams{TargetDate: target, Direction: "both", Cutoff: 36500, Power: 1.5, BoostWeight: 0.30},
		},
	}

	dates := zeroFixtureDates()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotBoost := ApplyGravityBoost(zeroFixtureResults(), dates, tc.zero)

			if tc.wantOff {
				assertSameRanking(t, gotBoost, zeroFixtureResults(), tc.contract)
				for _, r := range gotBoost {
					if r.RRFScoreOriginal != nil {
						t.Fatalf("%s: %q carries RRFScoreOriginal, so the results were rewritten", tc.contract, r.ID)
					}
				}
				return
			}

			// Per-block gravity: the raw score, before set-relative normalization
			// can hide a difference.
			for id, ds := range dates {
				got := ComputeGravity(ds, tc.zero)
				want := ComputeGravity(ds, tc.ref)
				if math.Abs(got-want) > 1e-12 {
					t.Fatalf("%s: gravity of %q is %.12f, want %.12f", tc.contract, id, got, want)
				}
			}

			// Full rerank: order and boosted scores must match the reference.
			wantBoost := ApplyGravityBoost(zeroFixtureResults(), dates, tc.ref)
			assertSameRanking(t, gotBoost, wantBoost, tc.contract)
		})
	}
}

// TestApplyGravityBoost_ZeroBoostWeightIsOffNotDefault separates "off" from
// "default 0.30" on a fixture where a 0.30 boost demonstrably reorders the set
// (block "d" sits on the target date and overtakes "a"). Passing this with
// BoostWeight 0 proves the early return wins over any default assignment.
func TestApplyGravityBoost_ZeroBoostWeightIsOffNotDefault(t *testing.T) {
	target := zeroTarget()
	dates := zeroFixtureDates()

	withDefault := ApplyGravityBoost(zeroFixtureResults(), dates, GravityParams{
		TargetDate: target, Direction: "both", Cutoff: 60, Power: 1.5, BoostWeight: 0.30,
	})
	if withDefault[0].ID != "d" {
		t.Fatalf("fixture no longer discriminates: BoostWeight 0.30 must lift %q to rank 0, got order %s", "d", idsOf(withDefault))
	}

	off := ApplyGravityBoost(zeroFixtureResults(), dates, GravityParams{
		TargetDate: target, Direction: "both", Cutoff: 60, Power: 1.5,
	})
	assertSameRanking(t, off, zeroFixtureResults(), "BoostWeight 0 is off, not 0.30")
}
