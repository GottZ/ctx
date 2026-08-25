package evalscore

import (
	"fmt"
	"math"
	"math/rand"
	"sort"

	"gonum.org/v1/gonum/stat/distuv"
)

// McNemarResult is the b / c / discordant / net / p table of a paired binary
// comparison, in the column order the retrieval reports use.
//
// B and C are the two discordant counts: B is the number of cases the variant
// wins and the baseline loses, C the reverse. Concordant cases (both hit, both
// miss) carry no information about the difference and appear in neither.
// Discordant is B+C, Net is B−C.
type McNemarResult struct {
	B          int     `json:"b"`
	C          int     `json:"c"`
	Discordant int     `json:"discordant"`
	Net        int     `json:"net"`
	P          float64 `json:"p"`
}

// McNemar runs the exact McNemar test on the two discordant counts of a paired
// binary comparison.
//
// Exact (binomial) rather than the chi-square approximation with continuity
// correction: the tables this instrument produces sit in the range where the
// approximation drifts. On the reference pair b=8, c=10 the exact test gives
// 0.8145 and the corrected chi-square 0.8137 — the exact value is the one the
// reference row carries, and it needs no small-count special case, so one code
// path covers 18 and 650 discordant pairs alike. The cost is a regularized
// incomplete beta per call, which is irrelevant next to a retrieval dump.
//
// Under the null hypothesis each discordant pair is a fair coin, so B is
// Binomial(B+C, 0.5). P is the two-sided p-value by min-tail doubling,
// 2·P(X <= min(B,C)), capped at 1. B == C therefore yields exactly 1, and so
// does B == C == 0, where there is nothing to test. Negative counts are not a
// table and yield NaN.
func McNemar(b, c int) McNemarResult {
	res := McNemarResult{B: b, C: c, Discordant: b + c, Net: b - c, P: 1}
	if b < 0 || c < 0 {
		res.P = math.NaN()
		return res
	}
	if res.Discordant == 0 {
		return res
	}
	k := b
	if c < k {
		k = c
	}
	p := 2 * distuv.Binomial{N: float64(res.Discordant), P: 0.5}.CDF(float64(k))
	if p > 1 {
		p = 1
	}
	res.P = p
	return res
}

// McNemarPaired counts the discordant pairs of two per-case outcome vectors and
// runs McNemar on them. baseline[i] and variant[i] must be the outcome of the
// same case: B counts the cases where the variant hits and the baseline misses,
// C the reverse — the convention McNemarResult documents, pinned in one place
// so no caller has to rediscover which column is which.
//
// Vectors of different length are refused instead of truncated: a silently
// shortened comparison reports a table over a case set nobody chose.
func McNemarPaired(baseline, variant []bool) (McNemarResult, error) {
	if len(baseline) != len(variant) {
		return McNemarResult{}, fmt.Errorf("evalscore: paired vectors differ in length (%d vs %d)", len(baseline), len(variant))
	}
	b, c := 0, 0
	for i := range baseline {
		switch {
		case variant[i] && !baseline[i]:
			b++
		case baseline[i] && !variant[i]:
			c++
		}
	}
	return McNemar(b, c), nil
}

// PairedDiffCI is the percentile bootstrap CI of the mean of a paired
// difference vector — one delta per case, variant minus baseline — at an
// explicit confidence level.
//
// Bootstrap rather than a t interval, for three reasons. It is the same
// estimator BootstrapCI already applies to the per-case scores, so a difference
// CI and an absolute CI in the same report are read off the same instrument.
// It assumes nothing about the shape of the per-query delta distribution, which
// is bounded, granular and carries most of its mass at exactly zero — the case
// where the normal approximation behind a t interval is least defensible. And
// it stays deterministic under a fixed seed, so a report can be reproduced.
// The price is resolution: with 1000 resamples the tails are read off single
// order statistics, and at a Bonferroni level the lower bound rests on the 2nd
// smallest resample mean.
//
// level is the confidence level, not alpha — 0.95 for a primary comparison,
// 1−0.05/13 for a Bonferroni-corrected secondary one. It travels as a parameter
// because the correction belongs to the comparison, not to the estimator; a
// level outside [0, 1] is clamped, level 1 spans the full resample range and
// level 0 collapses the interval onto the median resample mean. seed makes the
// draw reproducible; the function touches no global randomness. An empty vector
// yields the zero interval, matching BootstrapCI.
func PairedDiffCI(deltas []float64, level float64, seed int64) (lo, hi float64) {
	return bootstrapPercentileCI(deltas, level, seed)
}

// bootstrapResamples is the resample count of both bootstrap entry points.
const bootstrapResamples = 1000

// bootstrapPercentileCI is the shared kernel: bootstrapResamples resamples of
// the mean from a seeded generator, sorted, read at the two tail percentiles of
// level.
func bootstrapPercentileCI(vals []float64, level float64, seed int64) (lo, hi float64) {
	if len(vals) == 0 {
		return 0, 0
	}
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // Bootstrap-Resampling ist Statistik, keine Kryptographie — Determinismus (fester Seed) ist hier die Anforderung.
	means := make([]float64, bootstrapResamples)
	for i := 0; i < bootstrapResamples; i++ {
		sum := 0.0
		for j := 0; j < len(vals); j++ {
			sum += vals[rng.Intn(len(vals))]
		}
		means[i] = sum / float64(len(vals))
	}
	sort.Float64s(means)
	alpha := 1 - clampLevel(level)
	return means[percentileIndex(alpha/2, bootstrapResamples)], means[percentileIndex(1-alpha/2, bootstrapResamples)]
}

// clampLevel maps a confidence level into [0, 1]; NaN is treated as 0.
func clampLevel(level float64) float64 {
	switch {
	case math.IsNaN(level) || level < 0:
		return 0
	case level > 1:
		return 1
	default:
		return level
	}
}

// percentileIndex is the nearest-rank index of quantile q over n sorted values,
// rounded rather than ceiled: with q derived from a level via floating point,
// a tail that lands on an exact rank must not be pushed one position out by a
// last-bit error. At q=0.025/0.975 and n=1000 it yields 24 and 974 — the
// indices the 95% CI has always been read at.
func percentileIndex(q float64, n int) int {
	idx := int(math.Round(q*float64(n))) - 1
	if idx < 0 {
		return 0
	}
	if idx >= n {
		return n - 1
	}
	return idx
}
