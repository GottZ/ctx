package evalscore_test

import (
	"math"
	"testing"

	"github.com/GottZ/ctx/internal/evalscore"
)

// goldSet is the label shape both metrics take: a set of relevant ids.
func goldSet(ids ...string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// TestRecallAtK pins Recall@k including the three degenerate cases that decide
// whether a slice mean is a number or a lie: no labels, no ranking, k past the
// end of the list.
func TestRecallAtK(t *testing.T) {
	ranked := []string{"a", "b", "c", "d", "e", "f"}
	cases := []struct {
		name   string
		ranked []string
		gold   map[string]bool
		k      int
		want   float64
	}{
		{"both gold ids inside the window", ranked, goldSet("a", "c"), 5, 1.0},
		{"one of two inside the window", ranked, goldSet("a", "f"), 5, 0.5},
		{"none inside the window", ranked, goldSet("f"), 5, 0.0},
		{"k longer than the ranking is not an error", ranked, goldSet("f"), 99, 1.0},
		{"duplicate ids in the ranking count once", []string{"a", "a", "b"}, goldSet("a"), 5, 1.0},
		{"no labels yields 0, never NaN", ranked, goldSet(), 5, 0.0},
		{"empty ranking yields 0", nil, goldSet("a"), 5, 0.0},
		{"k<=0 yields 0", ranked, goldSet("a"), 0, 0.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evalscore.RecallAtK(tc.ranked, tc.gold, tc.k)
			if math.Abs(got-tc.want) > 1e-12 {
				t.Errorf("RecallAtK = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMRRAtK pins the reciprocal rank of the FIRST relevant hit and the cutoff.
func TestMRRAtK(t *testing.T) {
	ranked := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k"}
	cases := []struct {
		name   string
		ranked []string
		gold   map[string]bool
		k      int
		want   float64
	}{
		{"first position", ranked, goldSet("a"), 10, 1.0},
		{"third position", ranked, goldSet("c"), 10, 1.0 / 3},
		{"first of several counts", ranked, goldSet("c", "e"), 10, 1.0 / 3},
		{"tenth position is still inside @10", ranked, goldSet("j"), 10, 0.1},
		{"eleventh position is outside @10", ranked, goldSet("k"), 10, 0.0},
		{"no labels yields 0", ranked, goldSet(), 10, 0.0},
		{"empty ranking yields 0", nil, goldSet("a"), 10, 0.0},
		{"k<=0 yields 0", ranked, goldSet("a"), 0, 0.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evalscore.MRRAtK(tc.ranked, tc.gold, tc.k)
			if math.Abs(got-tc.want) > 1e-12 {
				t.Errorf("MRRAtK = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHitAtK pins the binary outcome the paired tests consume: Recall@k > 0.
func TestHitAtK(t *testing.T) {
	ranked := []string{"a", "b", "c"}
	if !evalscore.HitAtK(ranked, goldSet("b"), 3) {
		t.Error("HitAtK = false for a hit at position 2")
	}
	if evalscore.HitAtK(ranked, goldSet("z"), 3) {
		t.Error("HitAtK = true without a hit")
	}
	if evalscore.HitAtK(ranked, goldSet(), 3) {
		t.Error("HitAtK = true without labels")
	}
}

// TestNDCGRanked is the id-keyed adapter over NDCGBinary: the driver holds a
// ranking of ids, not a score vector, and the conversion must not silently
// change the metric.
func TestNDCGRanked(t *testing.T) {
	ranked := []string{"a", "b", "c", "d"}
	if got := evalscore.NDCGRanked(ranked, goldSet("a"), 10); math.Abs(got-1.0) > 1e-12 {
		t.Errorf("perfect ranking: nDCG = %v, want 1", got)
	}
	// One relevant doc at position 2: DCG = 1/log2(3), ideal = 1.
	want := 1.0 / math.Log2(3)
	if got := evalscore.NDCGRanked(ranked, goldSet("b"), 10); math.Abs(got-want) > 1e-12 {
		t.Errorf("second position: nDCG = %v, want %v", got, want)
	}
	if got := evalscore.NDCGRanked(ranked, goldSet(), 10); got != 0 {
		t.Errorf("no labels: nDCG = %v, want 0", got)
	}
	// Two relevant docs but k=1: the ideal must be capped at k, so a single
	// hit at position 1 is a perfect score at that cutoff.
	if got := evalscore.NDCGRanked(ranked, goldSet("a", "b"), 1); math.Abs(got-1.0) > 1e-12 {
		t.Errorf("k=1 with two labels: nDCG = %v, want 1", got)
	}
}
