// Package rrf — Post-RRF Temporal Gravity Reranker
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// Two gravity formulas:
//  1. Linear (Distance-only): Power-Law p=1.5 on absolute day distance.
//     Blocks closer to the query's target date get boosted.
//  2. Cyclic (GottZ Cyclic Phase Model): Multi-dimensional Gaussian decay
//     on normalized phases [0,1] per dimension (weekday, month, quarter,
//     week, year). The upper end is closed and reachable: the last value of a
//     cycle normalizes to 1.0, which CyclicDistance folds onto 0.0 — the same
//     point on the ring. Blocks matching the query's cyclic pattern get boosted
//     continuously — Tuesday-blocks strongly for "dienstags" query,
//     Wednesday-blocks weakly, Saturday-blocks near-zero.
//
// Pure functions, no DB access.
//
// Source: https://github.com/GottZ/ctx
package rrf

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"
)

// GravityParams controls the temporal gravity computation.
//
// Every field except TargetDate is optional, so every zero value carries a
// meaning and each one is documented at its field. The two filters (Direction,
// Cutoff) switch OFF when unset, Power falls back to its default, and
// BoostWeight 0 turns the reranker off entirely — there is no hidden default
// behind it (Issue #40, findings 2 and 8c).
type GravityParams struct {
	// TargetDate is the date the query anchors on. Required — every distance
	// is measured against it.
	TargetDate time.Time

	// Direction restricts which side of TargetDate may contribute: "past"
	// drops dates after it, "future" drops dates before it. Zero value (empty
	// string): no direction filter, identical to "both".
	Direction string

	// Cutoff is the maximum absolute distance in days a date may have and
	// still contribute. Zero value (and any non-positive value): no distance
	// filter — the same convention as Direction, since both fields are
	// filters. A caller that wants a bounded window must set it explicitly.
	Cutoff int

	// Power is the falloff exponent of the power law. Zero value: the 1.5
	// default is applied; see the power profile on ComputeGravity.
	Power float64

	// BoostWeight is the maximum boost fraction ApplyGravityBoost may add
	// (0.30 = up to +30%). Zero value: gravity is off and ApplyGravityBoost
	// returns its input untouched. No default is substituted — 0 means off.
	BoostWeight float64
}

// ComputeGravity returns the total gravitational score for a set of dates
// relative to a target date. Distance-only formula (no mass).
// Pure function, no DB access.
//
// Forward Telescoping (B4 #2 / B5): older memories are coarser. The backward
// power is scaled down by 1 + 0.3*ln(1 + age_days/30) so older blocks get a
// wider gravitational well — matches Rubin & Baddeley 1989's age-dependent
// imprecision. Forward dates keep the 1.2× sharper cutoff (no telescoping
// of the future).
//
// Power profile by age (backward only, base power=1.5):
//
//	age=1d    → power=1.49 (≈unchanged)
//	age=30d   → power=1.24
//	age=180d  → power=0.97
//	age=365d  → power=0.86
func ComputeGravity(dates []time.Time, params GravityParams) float64 {
	if len(dates) == 0 {
		return 0
	}
	if params.Power == 0 {
		params.Power = 1.5
	}

	var total float64
	for _, d := range dates {
		distDays := d.Sub(params.TargetDate).Hours() / 24.0

		// Direction filter
		if params.Direction == "past" && distDays > 0 {
			continue
		}
		if params.Direction == "future" && distDays < 0 {
			continue
		}

		// Cutoff filter — Cutoff <= 0 means no distance filter at all.
		// Filtering on a zero cutoff would keep only exact timestamp equality
		// and silently reduce the reranker to a no-op (Issue #40, finding 8c).
		if params.Cutoff > 0 && math.Abs(distDays) > float64(params.Cutoff) {
			continue
		}

		// Asymmetric power:
		//   future (distDays >= 0): 1.2× sharper cutoff (existing behavior)
		//   backward: age-scaled wider decay (Branch Y Forward Telescoping)
		var effPower float64
		if distDays >= 0 {
			effPower = params.Power * 1.2
		} else {
			age := -distDays // positive number of days into the past
			effPower = params.Power / (1.0 + 0.3*math.Log1p(age/30.0))
		}

		// Minimum distance 0.5 days to avoid singularity
		dist := math.Max(math.Abs(distDays), 0.5)
		total += 1.0 / math.Pow(dist, effPower)
	}
	return total
}

// maxDateGravity returns the largest score a single date can contribute to
// ComputeGravity — the absolute anchor of the linear path.
//
// It falls out of the formula: the distance is clamped to at least 0.5 days
// (see ComputeGravity), and the sharpest exponent any date can draw is the
// forward one, power*1.2. The strongest possible term is therefore
// 1/0.5^(power*1.2) = 2^(power*1.2); at the production power of 1.5 that is
// 2^1.8 ≈ 3.482. Derived rather than frozen as a literal, so a changed power
// moves the anchor with it. The Power zero value resolves the same way it does
// in ComputeGravity (see GravityParams).
//
// Per block the sum over several dates is unbounded, so callers saturate at
// this anchor instead of normalizing (Issue #40, finding 3).
func maxDateGravity(power float64) float64 {
	if power == 0 {
		power = 1.5
	}
	return math.Pow(2, power*1.2)
}

// cyclicGravityAnchor returns the largest value ComputeCyclicGravity can return
// for these weights — the absolute anchor of the cyclic path.
//
// ComputeCyclicGravity sums weight*best with best = GaussianDecay(...) in
// [0,1], so the maximum is reached when every scorable dimension decays to
// exactly 1.0: the sum of those weights. The admission rules mirror
// ComputeCyclicGravity, otherwise the ratio would leave [0,1] in one direction
// or the other — "linear" belongs to the linear path, a dimension without a
// sigma cannot be scored and contributes nothing, and a non-positive weight
// adds no reachable mass. A return value of 0 means nothing here is scorable;
// the caller must skip the boost rather than divide (Issue #40, finding 3).
func cyclicGravityAnchor(dimWeights map[string]float64) float64 {
	var sum float64
	for dim, weight := range dimWeights {
		if dim == "linear" || weight <= 0 {
			continue
		}
		if _, ok := DimensionSigma[dim]; !ok {
			continue
		}
		sum += weight
	}
	return sum
}

// ApplyGravityBoost reranks search results using temporal gravity.
// Blocks without dates get boost_factor=1.0 (neutral — no penalty).
// The boost saturates against maxDateGravity, so the factor of a block depends
// on that block alone and not on what else is in the window.
// Returns re-sorted results. Original RRF scores preserved in RRFScoreOriginal.
// A zero params.BoostWeight returns the input untouched — gravity off, no
// RRFScoreOriginal written (see GravityParams).
func ApplyGravityBoost(results []SearchResult, blockDates map[string][]time.Time, params GravityParams) []SearchResult {
	if len(results) == 0 || params.BoostWeight == 0 {
		return results
	}

	// Compute gravity for each result
	gravities := make([]float64, len(results))
	for i, r := range results {
		if dates, ok := blockDates[r.ID]; ok && len(dates) > 0 {
			gravities[i] = ComputeGravity(dates, params)
		}
	}

	// Apply saturating boost. The anchor is the strongest single date this
	// power admits (maxDateGravity), not the strongest candidate in the window:
	// a block earns the full boost by sitting on the target date, never by
	// being the least bad match in a window that holds no good one. A block
	// with several close dates saturates instead of running away, because the
	// per-block sum is unbounded (Issue #40, finding 3).
	anchor := maxDateGravity(params.Power)
	boosted := make([]SearchResult, len(results))
	copy(boosted, results)
	for i := range boosted {
		origScore := boosted[i].RRFScore
		boosted[i].RRFScoreOriginal = &origScore
		factor := 1.0 + params.BoostWeight*math.Min(gravities[i]/anchor, 1.0)
		boosted[i].RRFScore *= factor
	}

	// Sort by boosted score descending. Stable, so blocks that tie keep the
	// order the RRF stage gave them and the same query returns the same
	// ranking twice (Issue #40, side finding N2).
	sort.SliceStable(boosted, func(i, j int) bool {
		return boosted[i].RRFScore > boosted[j].RRFScore
	})

	return boosted
}

// --- GottZ Cyclic Phase Model — Multi-Dimensional Cyclic Gravity ---.

// DimensionSigma holds the Gaussian decay sigma per cyclic dimension.
// Calibrated from cognitive science (Rubin & Baddeley 1989, Huttenlocher 1988)
// and the cyclic phase walk-throughs in the original vision spec.
//
// Smaller sigma = tighter gravity well (less forgiving of phase mismatch).
// weekday 0.07: Monday→Wednesday (dist 2/7 ≈ 0.286) decays to ~1e-4 — effectively zero.
// weekday 0.07: Monday→Tuesday (dist 1/7 ≈ 0.143) decays to ~0.13 — weak but non-zero.
//
// All dimensions listed here are cyclic. "year" is explicitly NOT in this map —
// it is monotonic/linear.
var DimensionSigma = map[string]float64{
	"weekday":  0.07, // 7-day cycle, adjacent-day decay ~0.13
	"month":    0.10, // month-of-year 12-cycle
	"quarter":  0.12, // 4-quarter cycle (wider — quarters are imprecise)
	"week":     0.08, // ISO week-of-year 52-cycle
	"monthday": 0.10, // day-of-month ~30-cycle (Monatsanfang/-ende patterns)
	"seasonal": 0.08, // day-of-year 365-cycle (annual seasonal patterns)
	"daily":    0.08, // hour-of-day 24-cycle (morgens/abends patterns)
}

// CyclicDistance returns the minimum wrap-around distance between two phases
// on a unit circle. Phases are in [0,1] — the interval is closed, because the
// last value of a cycle normalizes to exactly 1.0 (week 53, monthday 31,
// seasonal 366). The wrap below maps 1.0 onto 0.0, which is correct for a
// ring: both name the same point. Result is in [0, 0.5].
//
// Monday (0/7) vs Sunday (6/7):
//
//	direct:   |0 - 6/7| = 6/7 ≈ 0.857
//	wrapped:  1 - 6/7 = 1/7 ≈ 0.143
//	min:      1/7 ≈ 0.143 (they're neighbors on the cycle)
func CyclicDistance(a, b float64) float64 {
	d := math.Abs(a - b)
	if d > 0.5 {
		d = 1.0 - d
	}
	return d
}

// GaussianDecay returns exp(-distance² / (2*sigma²)).
// Returns 1.0 at distance=0, approaches 0 as distance grows relative to sigma.
//
// For sigma=0.07 (weekday):
//
//	distance=0.000 → 1.000 (exact match)
//	distance=0.143 → 0.128 (adjacent weekday, 1/7)
//	distance=0.286 → 0.00027 (2 weekdays apart)
//	distance=0.500 → ~0 (opposite side of cycle)
func GaussianDecay(distance, sigma float64) float64 {
	if sigma <= 0 {
		return 0
	}
	return math.Exp(-(distance * distance) / (2.0 * sigma * sigma))
}

// DimensionPhase converts an EAV (dimension, value) pair to a phase in [0,1].
// The upper end is reachable — week 53, monthday 31 and seasonal 366 each
// normalize to exactly 1.0 — and CyclicDistance folds 1.0 onto 0.0, the same
// point on the ring. Callers must not assume phase < 1.0.
// Matches the encoding used by store.ExpandDimensions:
//
//	weekday: ISO 1-7 (Mon=1..Sun=7) → (v-1)/7
//	month:   1-12 → (v-1)/12
//	quarter: 1-4  → (v-1)/4
//	week:    ISO 1-53 → (v-1)/52
//	year:    not cyclic in the [0,1) sense — returns error
//
// Returns (phase, nil) on success, (0, error) on invalid input.
func DimensionPhase(dimension, value string) (float64, error) {
	v, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("rrf: dimension phase: invalid value %q: %w", value, err)
	}
	switch dimension {
	case "weekday":
		if v < 1 || v > 7 {
			return 0, fmt.Errorf("rrf: weekday value out of range: %d", v)
		}
		return float64(v-1) / 7.0, nil
	case "month":
		if v < 1 || v > 12 {
			return 0, fmt.Errorf("rrf: month value out of range: %d", v)
		}
		return float64(v-1) / 12.0, nil
	case "quarter":
		if v < 1 || v > 4 {
			return 0, fmt.Errorf("rrf: quarter value out of range: %d", v)
		}
		return float64(v-1) / 4.0, nil
	case "week":
		if v < 1 || v > 53 {
			return 0, fmt.Errorf("rrf: week value out of range: %d", v)
		}
		return float64(v-1) / 52.0, nil
	case "monthday":
		if v < 1 || v > 31 {
			return 0, fmt.Errorf("rrf: monthday value out of range: %d", v)
		}
		// Use 30-day reference cycle — most months fit, Feb and 31-day months
		// introduce minor asymmetry but the Gaussian σ=0.10 absorbs it.
		return float64(v-1) / 30.0, nil
	case "seasonal":
		if v < 1 || v > 366 {
			return 0, fmt.Errorf("rrf: seasonal value out of range: %d", v)
		}
		return float64(v-1) / 365.0, nil
	case "daily":
		if v < 0 || v > 23 {
			return 0, fmt.Errorf("rrf: daily value out of range: %d", v)
		}
		return float64(v) / 24.0, nil
	case "year":
		// Year is not a cyclic dimension in the [0,1) sense — it monotonically
		// increases. Use linear gravity for year-distance scoring.
		return 0, fmt.Errorf("rrf: year is not a cyclic dimension")
	}
	return 0, fmt.Errorf("rrf: unknown dimension: %s", dimension)
}

// QueryPhase converts a target date to a phase for a given dimension.
// Same [0,1] range as DimensionPhase, upper end included: ISO week 53, day 31
// and year-day 366 all normalize to 1.0.
// Mirrors store.ExpandDimensions encoding:
//
//	2026-03-31 (Tuesday, March, Q1, week 14)
//	  weekday → (2-1)/7  = 0.143  (ISO Tuesday=2)
//	  month   → (3-1)/12 = 0.167
//	  quarter → (1-1)/4  = 0.0
//	  week    → (14-1)/52 = 0.25
func QueryPhase(dimension string, t time.Time) (float64, error) {
	// Go Weekday: Sunday=0, Monday=1 .. Saturday=6
	// ISO weekday: Monday=1 .. Sunday=7
	isoWd := int(t.Weekday())
	if isoWd == 0 {
		isoWd = 7 // Sunday
	}
	switch dimension {
	case "weekday":
		return float64(isoWd-1) / 7.0, nil
	case "month":
		return float64(int(t.Month())-1) / 12.0, nil
	case "quarter":
		q := (int(t.Month())-1)/3 + 1
		return float64(q-1) / 4.0, nil
	case "week":
		_, iso := t.ISOWeek()
		return float64(iso-1) / 52.0, nil
	case "monthday":
		return float64(t.Day()-1) / 30.0, nil
	case "seasonal":
		return float64(t.YearDay()-1) / 365.0, nil
	case "daily":
		return float64(t.Hour()) / 24.0, nil
	}
	return 0, fmt.Errorf("rrf: unknown dimension: %s", dimension)
}

// TemporalDim is the minimal representation of an EAV dimension row.
// Decouples rrf package from store package (avoids circular imports).
type TemporalDim struct {
	Dimension string
	Value     string
}

// ComputeCyclicGravity scores a block based on its EAV dimensions against
// the query's target date and dimension weights.
//
// Formula: gravity = Σ( dimWeight[d] × max( GaussianDecay(cyclicDist(queryPhase[d], blockPhase[d]), sigma[d]) ) )
//
// For each dimension in dimWeights (excluding "linear"):
//  1. Compute queryPhase[d] from queryDate
//  2. For each matching blockDim, compute decay and take the maximum
//     (a block with multiple dates matching the dimension gets credit for
//     its best match, not all of them)
//  3. Weighted sum across dimensions
//
// "linear" weight is ignored here — it's handled by ApplyGravityBoost.
// Returns 0 if dimWeights has no cyclic dimensions or block has no matching dims.
func ComputeCyclicGravity(dimWeights map[string]float64, blockDims []TemporalDim, queryDate time.Time) float64 {
	if len(dimWeights) == 0 || len(blockDims) == 0 {
		return 0
	}

	// Group block dimensions by dimension name for efficient lookup.
	byDim := make(map[string][]string, 5)
	for _, bd := range blockDims {
		byDim[bd.Dimension] = append(byDim[bd.Dimension], bd.Value)
	}

	var total float64
	for dim, weight := range dimWeights {
		if dim == "linear" {
			continue // handled elsewhere
		}
		sigma, ok := DimensionSigma[dim]
		if !ok {
			continue // unknown dimension
		}
		qPhase, err := QueryPhase(dim, queryDate)
		if err != nil {
			continue
		}
		values, hasDim := byDim[dim]
		if !hasDim {
			continue
		}
		// Take best match across all block values for this dimension.
		best := 0.0
		for _, v := range values {
			bPhase, err := DimensionPhase(dim, v)
			if err != nil {
				continue
			}
			d := CyclicDistance(qPhase, bPhase)
			decay := GaussianDecay(d, sigma)
			if decay > best {
				best = decay
			}
		}
		total += weight * best
	}
	return total
}

// ApplyCyclicGravityBoost reranks results using cyclic multi-dimensional gravity.
// Similar to ApplyGravityBoost but uses dimensions from the EAV table
// instead of linear date distances.
//
// blockDims: map from blockID to its EAV dimensions (fetched via store.FetchBlockDimensions)
// dimWeights: from parser (TemporalResult.DimensionWeights)
// queryDate: the query's target date (for phase computation)
// boostWeight: max multiplicative boost (0.30 = up to 30% score increase)
//
// Blocks with no matching dimensions get factor=1.0 (neutral, no penalty).
// The boost is anchored against cyclicGravityAnchor — the score a block would
// reach by matching every scorable dimension exactly — so the factor of a block
// depends on that block alone and not on what else is in the window. Weights
// that name nothing scorable leave the results untouched.
func ApplyCyclicGravityBoost(
	results []SearchResult,
	blockDims map[string][]TemporalDim,
	dimWeights map[string]float64,
	queryDate time.Time,
	boostWeight float64,
) []SearchResult {
	if len(results) == 0 || boostWeight == 0 {
		return results
	}

	// The anchor is what a perfect match scores, not what the best block in
	// this window happens to score. Without it a query for Tuesday that finds
	// no Tuesday block lifts the nearest Wednesday block onto the full boost,
	// flattening the 1.000 / 0.128 / 0.00027 gradation the sigmas are
	// calibrated for (Issue #40, finding 3). Zero means no dimension here can
	// be scored, so there is nothing to boost — and nothing to divide by.
	anchor := cyclicGravityAnchor(dimWeights)
	if anchor <= 0 {
		return results
	}

	// Compute cyclic gravity per block.
	gravities := make([]float64, len(results))
	for i, r := range results {
		if dims, ok := blockDims[r.ID]; ok {
			gravities[i] = ComputeCyclicGravity(dimWeights, dims, queryDate)
		}
	}

	// Apply the anchored boost.
	boosted := make([]SearchResult, len(results))
	copy(boosted, results)
	for i := range boosted {
		origScore := boosted[i].RRFScore
		boosted[i].RRFScoreOriginal = &origScore
		factor := 1.0 + boostWeight*(gravities[i]/anchor)
		boosted[i].RRFScore *= factor
	}

	// Sort by boosted score descending. Stable, so blocks that tie keep the
	// order the RRF stage gave them (Issue #40, side finding N2).
	sort.SliceStable(boosted, func(i, j int) bool {
		return boosted[i].RRFScore > boosted[j].RRFScore
	})

	return boosted
}
