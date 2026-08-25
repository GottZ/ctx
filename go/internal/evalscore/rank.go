package evalscore

// Ranking metrics over an ORDERED LIST OF IDS, which is the shape a retrieval
// measurement actually holds — NDCGBinary above takes a per-document score
// vector plus positional labels, the shape an LLM-judge harness holds. Both
// exist because converting between them at every call site is where a metric
// quietly turns into a different metric.
//
// All four functions treat "no labels" and "no ranking" as 0, never NaN: a
// slice mean is taken over these values, and one NaN would erase a whole
// column of a report instead of costing one case.

// RecallAtK is the share of DISTINCT relevant ids that appear in the first k
// positions of ranked. A duplicate id in the ranking counts once — the second
// occurrence retrieves nothing new — while still consuming its position, which
// is the honest accounting for a list that a fold or an injection may have
// left with repeats.
//
// k is clamped to the length of ranked, so a cutoff past the end of a short
// ranking is a full-list recall and not an error.
func RecallAtK(ranked []string, gold map[string]bool, k int) float64 {
	if k <= 0 || len(ranked) == 0 || len(gold) == 0 {
		return 0
	}
	if k > len(ranked) {
		k = len(ranked)
	}
	seen := make(map[string]bool, k)
	hits := 0
	for i := 0; i < k; i++ {
		id := ranked[i]
		if seen[id] || !gold[id] {
			seen[id] = true
			continue
		}
		seen[id] = true
		hits++
	}
	return float64(hits) / float64(len(gold))
}

// MRRAtK is the reciprocal rank of the FIRST relevant id inside the first k
// positions, 0 if none is. Positions are 1-based, so a hit at the head is 1.
func MRRAtK(ranked []string, gold map[string]bool, k int) float64 {
	if k <= 0 || len(ranked) == 0 || len(gold) == 0 {
		return 0
	}
	if k > len(ranked) {
		k = len(ranked)
	}
	for i := 0; i < k; i++ {
		if gold[ranked[i]] {
			return 1.0 / float64(i+1)
		}
	}
	return 0
}

// HitAtK is the BINARY outcome of RecallAtK — "did anything relevant reach the
// window at all". It is the vector McNemarPaired consumes, and it lives here so
// the binary a paired test runs on is derived from the very same window the
// reported Recall@k is, rather than from a second definition somewhere.
func HitAtK(ranked []string, gold map[string]bool, k int) bool {
	return RecallAtK(ranked, gold, k) > 0
}

// NDCGRanked is NDCGBinary over an ordered id list: position i gets a strictly
// decreasing surrogate score, so the stable sort inside NDCGBinary reproduces
// the input order exactly, and the relevant positions are the indices carrying
// a gold id.
//
// The surrogate is len-i rather than -i so the values stay positive and the
// comparison in NDCGBinary never has to reason about signed zero.
func NDCGRanked(ranked []string, gold map[string]bool, k int) float64 {
	if len(ranked) == 0 || len(gold) == 0 {
		return 0
	}
	scores := make([]float64, len(ranked))
	var relevant []int
	seen := make(map[string]bool, len(ranked))
	for i, id := range ranked {
		scores[i] = float64(len(ranked) - i)
		if gold[id] && !seen[id] {
			relevant = append(relevant, i)
		}
		seen[id] = true
	}
	return NDCGBinary(scores, relevant, k)
}
