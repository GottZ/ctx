// B-W1: the offline reference fusion. This file carries NO build tag on
// purpose — it is the pure half of the ctx_rrf_arms gate (migration
// 137_rrf_arms.sql) and must compile and run in the short unit loop, while
// arms_parity_integration_test.go drives the same function against a real
// Postgres.
//
// What it is: the arithmetic ctx_rrf performs in SQL (134_rrf_gen16_ann_
// embedding_filter.sql:331-345), re-expressed over the per-arm ranks that
// ctx_rrf_arms hands out. If this reproduces ctx_rrf row for row on a seeded
// corpus, then the arm ranks are a faithful decomposition of the live score —
// which is the whole claim migration 137 makes, and the only thing that makes
// an offline arm-weight sweep (Achse 04 §4.2) meaningful.
package rrf_test

import (
	"math"
	"sort"
	"testing"
)

// armsRRFK is ctx_rrf's reciprocal-rank constant (134:334-337, the `60 +`).
const armsRRFK = 60.0

// armsLiveWeights are the four channel weights ctx_rrf multiplies onto the
// reciprocal ranks, in the order the offline fusion consumes them:
// semantic 0.45 (134:334), fts_de 0.20 (:335), fts_en 0.25 (:336),
// trigram 0.10 (:337).
var armsLiveWeights = [4]float64{0.45, 0.20, 0.25, 0.10}

// armRow is one ctx_rrf_arms output row. A nil rank means the block is not in
// that arm — exactly the NULL the FULL OUTER JOIN produces, and the reason the
// fusion COALESCEs the reciprocal to 0 instead of skipping the row.
type armRow struct {
	ID       string
	Semantic *int
	FtsDE    *int
	FtsEN    *int
	Trigram  *int
	Cos      *float64
	Mass     float64
	Type     float64
}

// fusedRow is a scored candidate: the shape ctx_rrf projects before its
// ORDER BY / LIMIT.
type fusedRow struct {
	ID    string
	Score float64
	Cos   *float64
}

// recip mirrors `COALESCE(1.0 / (60 + rank), 0)`: a missing arm contributes
// nothing rather than removing the candidate.
func recip(rank *int, k float64) float64 {
	if rank == nil {
		return 0
	}
	return 1.0 / (k + float64(*rank))
}

// fuseArms recomputes ctx_rrf's score from the per-arm ranks and sorts
// descending, i.e. everything the SQL does except the final `LIMIT p_limit`.
//
// The term order and the placement of the mass/type factors deliberately copy
// the SQL expression (`w * mass * type * recip`, summed semantic → fts_de →
// fts_en → trigram) instead of the algebraically equivalent
// `mass * type * Σ w*recip`. Both are the same real number; only one is the
// same float64.
//
// The sort carries NO tiebreak, because ctx_rrf has none either (134:360 is a
// bare `ORDER BY r.score DESC`). Equal scores therefore come back in an
// unspecified order on both sides, which is why the parity gate compares tie
// groups as sets — see armsTieGroups.
func fuseArms(rows []armRow, weights [4]float64, k float64) []fusedRow {
	out := make([]fusedRow, 0, len(rows))
	for _, r := range rows {
		mt := r.Mass * r.Type
		score := weights[0]*mt*recip(r.Semantic, k) +
			weights[1]*mt*recip(r.FtsDE, k) +
			weights[2]*mt*recip(r.FtsEN, k) +
			weights[3]*mt*recip(r.Trigram, k)
		out = append(out, fusedRow{ID: r.ID, Score: score, Cos: r.Cos})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// armsTieGroups partitions an already-sorted fusion into runs of rows whose
// scores are indistinguishable at eps. Returned as half-open [start, end)
// index pairs covering the whole slice.
func armsTieGroups(rows []fusedRow, eps float64) [][2]int {
	var groups [][2]int
	for i := 0; i < len(rows); {
		j := i + 1
		for j < len(rows) && math.Abs(rows[j].Score-rows[i].Score) <= eps {
			j++
		}
		groups = append(groups, [2]int{i, j})
		i = j
	}
	return groups
}

func ptrInt(v int) *int { return &v }

// TestBW1FuseArmsHandComputed pins the offline fusion against numbers worked
// out by hand, so a regression in the reference implementation cannot hide
// behind "the database agrees with it".
func TestBW1FuseArmsHandComputed(t *testing.T) {
	cos := 0.75
	rows := []armRow{
		// All four arms, neutral factors: the plain four-term sum.
		{ID: "a", Semantic: ptrInt(1), FtsDE: ptrInt(1), FtsEN: ptrInt(1), Trigram: ptrInt(1), Cos: &cos, Mass: 1, Type: 1},
		// Lexical-only (no semantic rank, NULL cosine — the E-M6 shape).
		{ID: "b", FtsDE: ptrInt(2), FtsEN: ptrInt(3), Mass: 1, Type: 1},
		// Semantic-only, damped type and a mass factor below 1.
		{ID: "c", Semantic: ptrInt(2), Cos: &cos, Mass: 0.5, Type: 0.3},
		// No arm at all — cannot occur through the FULL OUTER JOIN, but the
		// zero-COALESCE path must still be a score of exactly 0.
		{ID: "d", Mass: 1, Type: 1},
	}

	wantA := 0.45*1*1*(1.0/61) + 0.20*1*1*(1.0/61) + 0.25*1*1*(1.0/61) + 0.10*1*1*(1.0/61)
	wantB := 0.20*1*1*(1.0/62) + 0.25*1*1*(1.0/63)
	wantC := 0.45 * 0.5 * 0.3 * (1.0 / 62)

	got := fuseArms(rows, armsLiveWeights, armsRRFK)
	if len(got) != 4 {
		t.Fatalf("fuseArms returned %d rows, want 4", len(got))
	}
	want := []fusedRow{{ID: "a", Score: wantA}, {ID: "b", Score: wantB}, {ID: "c", Score: wantC}, {ID: "d", Score: 0}}
	for i := range want {
		if got[i].ID != want[i].ID {
			t.Errorf("position %d: id = %s, want %s (descending score order)", i, got[i].ID, want[i].ID)
		}
		if math.Abs(got[i].Score-want[i].Score) > 1e-15 {
			t.Errorf("position %d (%s): score = %.17g, want %.17g", i, got[i].ID, got[i].Score, want[i].Score)
		}
	}
	if got[0].Cos == nil || *got[0].Cos != cos {
		t.Errorf("row a lost its cosine: %v", got[0].Cos)
	}
	if got[1].Cos != nil {
		t.Errorf("lexical-only row b must keep a NULL cosine, got %v", *got[1].Cos)
	}

	// The factored form is the same real number but not the same float64 —
	// this is why fuseArms mirrors the SQL term order literally.
	factored := 1.0 * 1.0 * (0.45*(1.0/61) + 0.20*(1.0/61) + 0.25*(1.0/61) + 0.10*(1.0/61))
	t.Logf("term-order sensitivity: expression form = %.20g, factored form = %.20g, delta = %.3g",
		wantA, factored, math.Abs(wantA-factored))
}

// TestBW1ArmsTieGroups pins the tie partitioning the parity gate relies on to
// tell "ctx_rrf ordered a tie differently" apart from a real ranking defect.
func TestBW1ArmsTieGroups(t *testing.T) {
	rows := []fusedRow{
		{ID: "a", Score: 0.5},
		{ID: "b", Score: 0.25},
		{ID: "c", Score: 0.25},
		{ID: "d", Score: 0.25},
		{ID: "e", Score: 0.1},
	}
	groups := armsTieGroups(rows, 1e-12)
	want := [][2]int{{0, 1}, {1, 4}, {4, 5}}
	if len(groups) != len(want) {
		t.Fatalf("groups = %v, want %v", groups, want)
	}
	for i := range want {
		if groups[i] != want[i] {
			t.Errorf("group %d = %v, want %v", i, groups[i], want[i])
		}
	}
}
