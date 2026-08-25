package evalscore

import (
	"math"
	"testing"
)

// mcnemarRefP is the exact two-sided p-value of the reference table b=8, c=10:
// 2·P(X <= 8) for X ~ Binomial(18, 0.5) = 2·106762/262144. It is a dyadic
// rational, so the closed form is exactly representable and the tolerance below
// only has to cover the regularized incomplete beta that computes it.
const mcnemarRefP = 0.8145294189453125

// TestMcNemarReference reproduces the reference row of the release verdict this
// instrument has to stay comparable with: b=8, c=10 gives 18 discordant pairs,
// net −2 and p ≈ 0.815 — a tie, not a win.
func TestMcNemarReference(t *testing.T) {
	res := McNemar(8, 10)
	if res.B != 8 || res.C != 10 {
		t.Errorf("b/c = %d/%d, want 8/10", res.B, res.C)
	}
	if res.Discordant != 18 {
		t.Errorf("discordant = %d, want 18", res.Discordant)
	}
	if res.Net != -2 {
		t.Errorf("net = %d, want -2", res.Net)
	}
	if math.Abs(res.P-mcnemarRefP) > 1e-12 {
		t.Errorf("p = %.17g, want %.17g (delta %g)", res.P, mcnemarRefP, res.P-mcnemarRefP)
	}
	if math.Abs(res.P-0.815) > 0.001 {
		t.Errorf("p = %v does not round to the reported 0.815", res.P)
	}
}

// TestMcNemarSignConvention: swapping the two counts must flip the sign of Net
// and leave everything else alone. The p-value is two-sided and therefore
// direction-blind — which is exactly why Net has to carry the direction.
func TestMcNemarSignConvention(t *testing.T) {
	fwd := McNemar(8, 10)
	rev := McNemar(10, 8)
	if rev.Net != -fwd.Net {
		t.Errorf("net %d and %d are not mirrored", fwd.Net, rev.Net)
	}
	if rev.Net != 2 {
		t.Errorf("net = %d, want +2 when the variant leads", rev.Net)
	}
	if rev.Discordant != fwd.Discordant {
		t.Errorf("discordant changed under swap: %d vs %d", fwd.Discordant, rev.Discordant)
	}
	if rev.P != fwd.P {
		t.Errorf("two-sided p changed under swap: %v vs %v", fwd.P, rev.P)
	}
}

// TestMcNemarEdges covers the tables that carry no evidence and the input that
// is not a table at all.
func TestMcNemarEdges(t *testing.T) {
	if res := McNemar(0, 0); res.P != 1 || res.Discordant != 0 || res.Net != 0 {
		t.Errorf("empty table: %+v, want discordant 0, net 0, p 1", res)
	}
	if res := McNemar(7, 7); res.P != 1 {
		t.Errorf("balanced table p = %v, want exactly 1", res.P)
	}
	// One-sided extreme: 2·P(X <= 0) for Binomial(5, 0.5) = 2/32.
	if res := McNemar(0, 5); math.Abs(res.P-0.0625) > 1e-12 {
		t.Errorf("p = %.17g, want 0.0625", res.P)
	}
	if res := McNemar(-1, 2); !math.IsNaN(res.P) {
		t.Errorf("negative counts must not produce a p-value, got %v", res.P)
	}
}

// TestMcNemarPaired pins which discordant column is which: the variant that
// rescues a case the baseline missed lands in B, the reverse in C.
func TestMcNemarPaired(t *testing.T) {
	baseline := []bool{true, false, false, true, true, false}
	variant := []bool{true, true, true, false, true, false}
	res, err := McNemarPaired(baseline, variant)
	if err != nil {
		t.Fatalf("McNemarPaired: %v", err)
	}
	if res.B != 2 || res.C != 1 {
		t.Errorf("b/c = %d/%d, want 2/1 (two rescues, one regression)", res.B, res.C)
	}
	if res.Discordant != 3 || res.Net != 1 {
		t.Errorf("discordant/net = %d/%d, want 3/1", res.Discordant, res.Net)
	}
	if _, err := McNemarPaired(baseline, variant[:3]); err == nil {
		t.Error("vectors of different length must be refused, not truncated")
	}
}

// bonferroniLevel is the corrected level of a secondary variant: 13 comparisons
// share the family-wise 5%.
const bonferroniLevel = 1 - 0.05/13

// pairedDeltas is a fixed per-case difference vector with the shape the sweep
// expects: most cases unchanged, a few clear gains, two regressions.
var pairedDeltas = []float64{
	-0.2, -0.05, 0, 0, 0, 0.01, 0.02, 0.03, 0.05, 0.08,
	0.1, 0.12, 0, 0, 0.04, -0.03, 0.07, 0.15, 0, 0.06,
}

// TestPairedDiffCIMatchesBootstrapAt95 anchors the new entry point on the old
// one: at level 0.95 it must return exactly what the pre-move bootstrap
// returned, so a difference CI and a score CI stay the same instrument.
func TestPairedDiffCIMatchesBootstrapAt95(t *testing.T) {
	lo, hi := PairedDiffCI(pairedDeltas, 0.95, 20260812)
	if lo != -0.013500000000000002 || hi != 0.051000000000000004 {
		t.Errorf("ci = [%.17g, %.17g], want [-0.013500000000000002, 0.051000000000000004]", lo, hi)
	}
	bLo, bHi := BootstrapCI(pairedDeltas, 20260812)
	if lo != bLo || hi != bHi {
		t.Errorf("PairedDiffCI at 0.95 = [%v, %v], BootstrapCI = [%v, %v]", lo, hi, bLo, bHi)
	}
}

// TestPairedDiffCIBonferroniIsWider: the correction has to cost something. On
// the same vector and the same seed the corrected interval must contain the
// uncorrected one and be strictly wider — an interval that ignored its level
// parameter would pass neither half.
func TestPairedDiffCIBonferroniIsWider(t *testing.T) {
	lo95, hi95 := PairedDiffCI(pairedDeltas, 0.95, 20260812)
	loB, hiB := PairedDiffCI(pairedDeltas, bonferroniLevel, 20260812)
	if loB > lo95 || hiB < hi95 {
		t.Errorf("corrected [%v, %v] does not contain uncorrected [%v, %v]", loB, hiB, lo95, hi95)
	}
	if hiB-loB <= hi95-lo95 {
		t.Errorf("corrected width %v is not larger than uncorrected width %v", hiB-loB, hi95-lo95)
	}
}

// TestPairedDiffCIDeterminism: same vector, same level, same seed, same
// interval — twice. A different seed is allowed to move it; a repeated call is
// not, or no report over this package is reproducible.
func TestPairedDiffCIDeterminism(t *testing.T) {
	lo1, hi1 := PairedDiffCI(pairedDeltas, bonferroniLevel, 20260812)
	lo2, hi2 := PairedDiffCI(pairedDeltas, bonferroniLevel, 20260812)
	if lo1 != lo2 || hi1 != hi2 {
		t.Errorf("repeat call drifted: [%v, %v] vs [%v, %v]", lo1, hi1, lo2, hi2)
	}
	lo3, hi3 := PairedDiffCI(pairedDeltas, bonferroniLevel, 20260813)
	if lo1 == lo3 && hi1 == hi3 {
		t.Error("a different seed produced an identical interval — the seed is not wired through")
	}
}

// TestPairedDiffCIEdges covers the empty vector and the clamped levels.
func TestPairedDiffCIEdges(t *testing.T) {
	if lo, hi := PairedDiffCI(nil, 0.95, 1); lo != 0 || hi != 0 {
		t.Errorf("empty vector must yield the zero interval, got [%v, %v]", lo, hi)
	}
	full0, full1 := PairedDiffCI(pairedDeltas, 1, 20260812)
	wide0, wide1 := PairedDiffCI(pairedDeltas, 2, 20260812)
	if full0 != wide0 || full1 != wide1 {
		t.Errorf("level above 1 must clamp to 1: [%v, %v] vs [%v, %v]", wide0, wide1, full0, full1)
	}
	lo, hi := PairedDiffCI(pairedDeltas, math.NaN(), 20260812)
	if lo != hi {
		t.Errorf("NaN level must clamp to 0 and collapse the interval, got [%v, %v]", lo, hi)
	}
	if lo95, hi95 := PairedDiffCI(pairedDeltas, 0.95, 20260812); full0 > lo95 || full1 < hi95 {
		t.Errorf("level 1 interval [%v, %v] must span the 95%% interval [%v, %v]", full0, full1, lo95, hi95)
	}
}
