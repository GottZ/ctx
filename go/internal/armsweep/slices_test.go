package armsweep_test

import (
	"math"
	"testing"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/goldset"
	"github.com/GottZ/ctx/internal/rrf"
)

// --- Gate (d): multi-gold. The new slices carry MORE THAN ONE gold id per
// case, and Recall@5 must count distinct gold hits against the full gold set -
// a case with 8 gold ids and 3 of them in the window is 0.375, not 1.0.

func TestMultiGoldRecallOverEightIDs(t *testing.T) {
	t.Parallel()
	gold := []string{"g1", "g2", "g3", "g4", "g5", "g6", "g7", "g8"}
	// Ranking of five: three gold ids inside the Recall@5 window.
	ranked := []string{"g4", "x1", "g1", "x2", "g7"}

	rec := armsweep.Record{
		Slice:       goldset.SliceSess,
		Index:       0,
		QuerySHA256: goldset.SHA256Hex("was wurde am 2026-08-20 gearbeitet"),
		GoldIDs:     gold,
	}
	for i, id := range ranked {
		rank := i + 1
		rec.Rows = append(rec.Rows, rrf.ArmRow{
			ID: id, RankSemantic: &rank, MassFactor: 1, TypeFactor: 1,
		})
	}
	got := armsweep.ScoreCase(rec, armsweep.ConfigV0())
	if math.Abs(got.Recall5-0.375) > 1e-12 {
		t.Errorf("Recall@5 = %v, want 0.375 (3 of 8 gold ids)", got.Recall5)
	}
	if !got.Hit5 {
		t.Error("Hit@5 false although three gold ids are in the window")
	}
	if got.NDCG10 <= 0 {
		t.Errorf("nDCG@10 = %v, want > 0", got.NDCG10)
	}
	// The multi-gold case must land in its own slice, not in a G-Q bucket.
	if k := armsweep.SliceKeyOf(rec); k != goldset.SliceSess {
		t.Errorf("SliceKeyOf = %q, want %q", k, goldset.SliceSess)
	}
}

// SliceKeyOf keeps its old behaviour for the existing slices: G-Q splits by
// DERIV/HOLD, everything else is its own key.
func TestSliceKeyOfUnchangedForExistingSlices(t *testing.T) {
	t.Parallel()
	cases := []struct {
		slice, split, want string
	}{
		{goldset.SliceKI, "", armsweep.SliceKI},
		{goldset.SliceQ, goldset.SplitDeriv, armsweep.SliceQDeriv},
		{goldset.SliceQ, goldset.SplitHold, armsweep.SliceQHold},
		{goldset.SliceReal, "", armsweep.SliceRealName},
		{goldset.SliceMH, "", armsweep.SliceMH},
		{goldset.SliceGlob, "", armsweep.SliceGlob},
		{goldset.SliceGlobKonstr, "", armsweep.SliceGlobKonstr},
	}
	for _, tc := range cases {
		got := armsweep.SliceKeyOf(armsweep.Record{Slice: tc.slice, Split: tc.split})
		if got != tc.want {
			t.Errorf("SliceKeyOf(%s/%s) = %q, want %q", tc.slice, tc.split, got, tc.want)
		}
	}
}

// --- Gate (f): G-GLOB-KONSTR is reported as its own slice row but is NEVER a
// rollout criterion - its gold comes from graph_cluster_member, so it measures
// that a cluster block finds cluster members (design/05 §4.5, the circular trap).

func TestReportSlicesCarryTheNewRolloutSlices(t *testing.T) {
	t.Parallel()
	want := []string{
		armsweep.SliceKI, armsweep.SliceQDeriv, armsweep.SliceQHold, armsweep.SliceRealName,
		armsweep.SliceSess, armsweep.SliceMH, armsweep.SliceGlob,
	}
	got := armsweep.ReportSlices()
	if len(got) != len(want) {
		t.Fatalf("ReportSlices() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ReportSlices()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestGlobKonstrIsFloorOnlyNeverRollout(t *testing.T) {
	t.Parallel()
	for _, s := range armsweep.ReportSlices() {
		if s == armsweep.SliceGlobKonstr {
			t.Fatalf("%s is in ReportSlices() - the floor check would become a rollout criterion",
				armsweep.SliceGlobKonstr)
		}
	}
	floor := armsweep.FloorSlices()
	if len(floor) != 1 || floor[0] != armsweep.SliceGlobKonstr {
		t.Fatalf("FloorSlices() = %v, want [%s]", floor, armsweep.SliceGlobKonstr)
	}
	// ... but it IS a row of the report census.
	census := armsweep.CensusSlices()
	seen := false
	for _, s := range census {
		if s == armsweep.SliceGlobKonstr {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("CensusSlices() = %v does not carry %s as its own row", census, armsweep.SliceGlobKonstr)
	}
	// Wave X-W0b added a THIRD category to the registry: the G-REAL regime
	// strata, reported like the floor check and, like it, never a rollout
	// criterion. The invariant is tightened rather than relaxed — the census is
	// still exactly the three registry lists and nothing else.
	if len(census) != len(armsweep.ReportSlices())+len(armsweep.StratumSlices())+len(floor) {
		t.Errorf("CensusSlices() = %v, want ReportSlices() plus StratumSlices() plus FloorSlices()", census)
	}
}

// The census must actually produce the floor row when records for it exist -
// the row is what makes the declared bias visible in the report.
func TestSliceCensusCarriesFloorRow(t *testing.T) {
	t.Parallel()
	recs := []armsweep.Record{
		{Slice: goldset.SliceGlobKonstr, Index: 0, QuerySHA256: goldset.SHA256Hex("a"), GoldIDs: []string{"g1", "g2"}},
		{Slice: goldset.SliceGlob, Index: 0, QuerySHA256: goldset.SHA256Hex("b")},
	}
	profiles := armsweep.BuildSliceProfiles(recs)
	var floor *armsweep.SliceProfile
	for i := range profiles {
		if profiles[i].Slice == armsweep.SliceGlobKonstr {
			floor = &profiles[i]
		}
	}
	if floor == nil {
		t.Fatal("no G-GLOB-KONSTR row in the slice census")
	}
	if floor.RolloutCriterion {
		t.Error("the floor row claims to be a rollout criterion")
	}
	if floor.Note == "" {
		t.Error("the floor row carries no note declaring it a floor check")
	}
}
