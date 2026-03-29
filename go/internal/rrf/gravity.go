// Package rrf — Post-RRF Temporal Gravity Reranker
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// Distance-only gravity formula. Blocks with dates closer to the query's
// target date get boosted; blocks without dates stay neutral (factor=1.0).
// Pure functions, no DB access.
//
// Source: https://github.com/GottZ/ctx
package rrf

import (
	"math"
	"sort"
	"time"
)

// GravityParams controls the temporal gravity computation.
type GravityParams struct {
	TargetDate  time.Time
	Direction   string  // "past", "future", "both"
	Cutoff      int     // max distance in days
	Power       float64 // falloff exponent (default 1.5)
	BoostWeight float64 // max boost fraction (default 0.30)
}

// ComputeGravity returns the total gravitational score for a set of dates
// relative to a target date. Distance-only formula (no mass).
// Pure function, no DB access.
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

		// Cutoff filter
		if math.Abs(distDays) > float64(params.Cutoff) {
			continue
		}

		// Asymmetric power: future decays 20% faster
		effPower := params.Power
		if distDays >= 0 {
			effPower = params.Power * 1.2
		}

		// Minimum distance 0.5 days to avoid singularity
		dist := math.Max(math.Abs(distDays), 0.5)
		total += 1.0 / math.Pow(dist, effPower)
	}
	return total
}

// ApplyGravityBoost reranks search results using temporal gravity.
// Blocks without dates get boost_factor=1.0 (neutral — no penalty).
// Returns re-sorted results. Original RRF scores preserved in RRFScoreOriginal.
func ApplyGravityBoost(results []SearchResult, blockDates map[string][]time.Time, params GravityParams) []SearchResult {
	if len(results) == 0 || params.BoostWeight == 0 {
		return results
	}
	if params.BoostWeight == 0 {
		params.BoostWeight = 0.30
	}

	// Compute gravity for each result
	gravities := make([]float64, len(results))
	maxGrav := 0.001 // avoid division by zero

	for i, r := range results {
		if dates, ok := blockDates[r.ID]; ok && len(dates) > 0 {
			g := ComputeGravity(dates, params)
			gravities[i] = g
			if g > maxGrav {
				maxGrav = g
			}
		}
	}

	// Apply normalized boost
	boosted := make([]SearchResult, len(results))
	copy(boosted, results)
	for i := range boosted {
		origScore := boosted[i].RRFScore
		boosted[i].RRFScoreOriginal = &origScore
		factor := 1.0 + params.BoostWeight*(gravities[i]/maxGrav)
		boosted[i].RRFScore *= factor
	}

	// Sort by boosted score descending
	sort.Slice(boosted, func(i, j int) bool {
		return boosted[i].RRFScore > boosted[j].RRFScore
	})

	return boosted
}
