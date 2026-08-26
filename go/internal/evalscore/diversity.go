package evalscore

import (
	"math"
	"sort"
)

// Diversity metrics over an ORDERED LIST OF IDS (design/05 §4.1b). RecallAtK
// and NDCGRanked in rank.go are blind to one failure mode of a corpus that
// keeps derived blocks in the same pool as their sources: a derived block can
// push its own source blocks out of the top-k without either number moving,
// because both sides of that trade are relevant. The two functions here make
// it visible — SRecallAtK over FACET coverage, AlphaNDCG over facet coverage
// with a redundancy discount.
//
// A facet (subtopic, aspect) is whatever the caller declares. For ctx it is a
// gold block of the source set, and a derived block covers exactly the facets
// of its source_block_ids.
//
// Both treat missing judgements and an empty ranking as 0, never NaN, for the
// same reason the four functions in rank.go do: one NaN erases a whole column
// of a report instead of costing one case.

// AlphaNDCGDefault is the redundancy tolerance α-nDCG is reported with when a
// caller has no reason to pick another one — the value Clarke et al. (2008)
// use throughout. It is a constant and deliberately not a config key: a metric
// whose parameter moves between two reports compares nothing.
const AlphaNDCGDefault = 0.5

// SRecallAtK is subtopic recall (Zhai et al. 2003): the share of FACETS that
// at least one of their blocks reaches inside the first k positions. aspects
// maps a facet to the ids covering it — the inverse of the map AlphaNDCG
// takes, because a coverage count is cheapest in that direction.
//
// The denominator is every facet the caller declares, a facet without any
// block included: an unreachable facet is a real gap in the corpus, and
// dropping it would make the score depend on which facets happen to have a
// block rather than on what the ranking delivered.
//
// k is clamped to the length of ranked, and a repeated id inside the window
// covers what it covers once — like RecallAtK, a repeat costs its position and
// retrieves nothing new.
func SRecallAtK(ranked []string, aspects map[string][]string, k int) float64 {
	if k <= 0 || len(ranked) == 0 || len(aspects) == 0 {
		return 0
	}
	if k > len(ranked) {
		k = len(ranked)
	}
	window := make(map[string]bool, k)
	for _, id := range ranked[:k] {
		window[id] = true
	}
	covered := 0
	for _, ids := range aspects {
		for _, id := range ids {
			if window[id] {
				covered++
				break
			}
		}
	}
	return float64(covered) / float64(len(aspects))
}

// AlphaNDCG is α-nDCG (Clarke et al. 2008) over an ordered id list. The gain
// of a document is the sum over ITS facets of (1−α)^(times that facet was
// already served by an earlier position), discounted by the 1/log2(rank+2)
// NDCGBinary also uses, and normalised by an ideal ranking over the same
// candidate pool.
//
// aspectsOf maps a block id to its facets and IS the candidate pool: it is
// everything the caller judged for this query, not only what this one ranking
// happens to contain. Two rankings judged against the same map are therefore
// directly comparable, which is what a base/condition comparison needs.
//
// α is the redundancy tolerance. At α=0 a repeated facet is worth as much as a
// new one and the metric stops discounting redundancy altogether; at α=1 a
// repeated facet is worth nothing. Values outside [0,1] are clamped.
//
// A repeated id contributes no gain while still consuming its position, and an
// id the caller did not judge contributes no gain — the same accounting
// RecallAtK applies.
//
// The normaliser is the GREEDY ideal ranking: pick, position by position, the
// document with the highest gain given what has been served, ties by ascending
// id so the number is reproducible. Choosing the true ideal is NP-hard and
// greedy is the standard approximation from the literature — but it is a LOWER
// bound on the ideal, so a pool of overlapping multi-facet documents can score
// marginally above 1. The result is not clamped to 1: a clamp would report
// exactly the case worth looking at as a perfect score.
//
// The normaliser depends on aspectsOf, k and α only — never on ranked — so a
// comparison BETWEEN rankings stays sound even where the absolute value is
// affected by that approximation.
//
// Cost is O(k · |aspectsOf| · facets) for the ideal, which is a per-query
// judgement pool of a handful of blocks, not the corpus.
func AlphaNDCG(ranked []string, aspectsOf map[string][]string, k int, alpha float64) float64 {
	if k <= 0 || len(ranked) == 0 || len(aspectsOf) == 0 {
		return 0
	}
	if k > len(ranked) {
		k = len(ranked)
	}
	alpha = max(0, min(1, alpha))

	facets := distinctFacets(aspectsOf)
	ideal := idealAlphaDCG(facets, k, alpha)
	if ideal == 0 {
		return 0
	}
	return rankedAlphaDCG(ranked, facets, k, alpha) / ideal
}

// distinctFacets drops repeated entries inside a document's facet list so a
// caller that names the same facet twice does not have it counted twice.
func distinctFacets(aspectsOf map[string][]string) map[string][]string {
	out := make(map[string][]string, len(aspectsOf))
	for id, list := range aspectsOf {
		if len(list) < 2 {
			out[id] = list
			continue
		}
		seen := make(map[string]bool, len(list))
		kept := make([]string, 0, len(list))
		for _, f := range list {
			if seen[f] {
				continue
			}
			seen[f] = true
			kept = append(kept, f)
		}
		out[id] = kept
	}
	return out
}

// alphaGain is the novelty-discounted gain of one document given how often
// each of its facets has already been served.
func alphaGain(facets []string, served map[string]int, alpha float64) float64 {
	gain := 0.0
	for _, f := range facets {
		gain += math.Pow(1-alpha, float64(served[f]))
	}
	return gain
}

// serveFacets folds a document's facets into the served counters.
func serveFacets(facets []string, served map[string]int) {
	for _, f := range facets {
		served[f]++
	}
}

// rankedAlphaDCG is the discounted gain the given ranking earns over its first
// k positions.
func rankedAlphaDCG(ranked []string, facets map[string][]string, k int, alpha float64) float64 {
	served := make(map[string]int, len(facets))
	delivered := make(map[string]bool, k)
	dcg := 0.0
	for rank := 0; rank < k; rank++ {
		id := ranked[rank]
		if delivered[id] {
			continue
		}
		delivered[id] = true
		list := facets[id]
		if len(list) == 0 {
			continue
		}
		dcg += alphaGain(list, served, alpha) / math.Log2(float64(rank)+2)
		serveFacets(list, served)
	}
	return dcg
}

// idealAlphaDCG is the greedy ideal: at every position the still unused
// document with the highest current gain, ties by ascending id. It stops as
// soon as nothing carries gain any more, which is what keeps a ranking longer
// than its judgement pool from being normalised against gain that does not
// exist.
func idealAlphaDCG(facets map[string][]string, k int, alpha float64) float64 {
	ids := make([]string, 0, len(facets))
	for id := range facets {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	served := make(map[string]int, len(facets))
	taken := make([]bool, len(ids))
	dcg := 0.0
	for rank := 0; rank < k; rank++ {
		best, bestGain := -1, 0.0
		for i, id := range ids {
			if taken[i] {
				continue
			}
			if g := alphaGain(facets[id], served, alpha); g > bestGain {
				best, bestGain = i, g
			}
		}
		if best < 0 {
			break
		}
		taken[best] = true
		serveFacets(facets[ids[best]], served)
		dcg += bestGain / math.Log2(float64(rank)+2)
	}
	return dcg
}
