package evalscore

import (
	"math"
	"testing"
)

func almostEqual(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

// TestMicroF1 pins the micro-aggregated counter kernel, including the
// zero-denominator branches that must not produce NaN.
func TestMicroF1(t *testing.T) {
	p, r, f1 := MicroF1(2, 1, 1)
	almostEqual(t, p, 2.0/3.0, "precision")
	almostEqual(t, r, 2.0/3.0, "recall")
	almostEqual(t, f1, 2.0/3.0, "f1")

	if _, _, zero := MicroF1(0, 0, 0); zero != 0 {
		t.Errorf("empty counters must score 0, got %v", zero)
	}
}

// TestSetCounts pins TP/FP/FN over exact string sets.
func TestSetCounts(t *testing.T) {
	pred := map[string]bool{"a": true, "b": true, "c": true}
	gold := map[string]bool{"b": true, "d": true}
	tp, fp, fn := SetCounts(pred, gold)
	if tp != 1 || fp != 2 || fn != 1 {
		t.Errorf("tp/fp/fn = %d/%d/%d, want 1/2/1", tp, fp, fn)
	}
}

// TestTokenF1 pins the tokenizer contract: case and punctuation are ignored,
// umlauts are kept as characters (so a missing umlaut is a miss, not a match).
func TestTokenF1(t *testing.T) {
	almostEqual(t, TokenF1("Graph-Cache für pgvector", "graph cache FÜR pgvector!"), 1.0, "identical")
	almostEqual(t, TokenF1("alpha beta", "gamma delta"), 0.0, "disjoint")
	if TokenF1("läuft", "lauft") != 0 {
		t.Error("umlaut and its ASCII spelling must not share a token")
	}
}

// TestNDCGBinary pins the ranking metric and its stable tie order.
func TestNDCGBinary(t *testing.T) {
	almostEqual(t, NDCGBinary([]float64{9, 1, 0}, []int{0}, 15), 1.0, "perfect")
	almostEqual(t, NDCGBinary([]float64{9, 1}, []int{1}, 15), 1.0/math.Log2(3), "rank 2")
	almostEqual(t, NDCGBinary([]float64{1, 2}, nil, 15), 0, "no relevant doc")
	almostEqual(t, NDCGBinary(nil, []int{0}, 15), 0, "no docs")
	// Ties keep the original order, so the relevant doc that entered first wins
	// the higher rank — the property that makes a dump reproducible.
	almostEqual(t, NDCGBinary([]float64{5, 5, 5}, []int{0}, 1), 1.0, "tie head")
	almostEqual(t, NDCGBinary([]float64{5, 5, 5}, []int{2}, 1), 0.0, "tie tail")
}

// TestMeanAndRatio pin the aggregation helpers on their empty inputs.
func TestMeanAndRatio(t *testing.T) {
	almostEqual(t, MeanOrZero([]float64{1, 2, 3}), 2, "mean")
	almostEqual(t, MeanOrZero(nil), 0, "mean of nothing")
	almostEqual(t, RatioOrZero(3, 4), 0.75, "ratio")
	almostEqual(t, RatioOrZero(3, 0), 0, "ratio without denominator")
}

// TestTrgmSimilarity pins the pg_trgm reimplementation.
func TestTrgmSimilarity(t *testing.T) {
	almostEqual(t, TrgmSimilarity("Wochenbericht KW12", "Wochenbericht KW12"), 1.0, "identical")
	if s := TrgmSimilarity("Wochenbericht KW12", "Wochenbericht KW13"); s <= 0.5 || s >= 1 {
		t.Errorf("one changed digit must land strictly between 0.5 and 1, got %v", s)
	}
	almostEqual(t, TrgmSimilarity("abc", "xyz"), 0, "disjoint")
	almostEqual(t, TrgmSimilarity("", "abc"), 0, "empty side")
}

// TestBootstrapCIGolden is the numeric anchor of the move: the two intervals
// were produced by the pre-move body (goldbench score.go, means[24]/means[974])
// and must survive the level-parameterized kernel bit for bit. A drift here
// means every historic goldbench CI has become incomparable.
func TestBootstrapCIGolden(t *testing.T) {
	vals := make([]float64, 25)
	for i := range vals {
		vals[i] = float64(i) / 24.0
	}
	lo, hi := BootstrapCI(vals, 20260812)
	if lo != 0.38666666666666671 || hi != 0.61833333333333329 {
		t.Errorf("ci = [%.17g, %.17g], want [0.38666666666666671, 0.61833333333333329]", lo, hi)
	}
	lo, hi = BootstrapCI(nil, 1)
	if lo != 0 || hi != 0 {
		t.Errorf("empty input must yield the zero interval, got [%v, %v]", lo, hi)
	}
}

// TestPercentileIndexTails pins the two indices the 95% interval has always
// been read at, against the last-bit error that a ceil-based rank would incur.
// alpha is derived exactly as bootstrapPercentileCI derives it — a constant
// expression folded at compile time carries different bits than the runtime
// subtraction and would test a quantile production never asks for.
func TestPercentileIndexTails(t *testing.T) {
	alpha := 1 - clampLevel(0.95)
	if got := percentileIndex(alpha/2, bootstrapResamples); got != 24 {
		t.Errorf("lower tail index %d, want 24", got)
	}
	if got := percentileIndex(1-alpha/2, bootstrapResamples); got != 974 {
		t.Errorf("upper tail index %d, want 974", got)
	}
	if got := percentileIndex(0, 1000); got != 0 {
		t.Errorf("q=0 index %d, want 0", got)
	}
	if got := percentileIndex(1, 1000); got != 999 {
		t.Errorf("q=1 index %d, want 999", got)
	}
}
