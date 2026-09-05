// Package evalscore holds the scoring primitives the ctx evaluation harnesses
// share: the metric kernels (micro-F1, token-F1, nDCG@k), the aggregation
// helpers, the pg_trgm similarity reimplementation and the deterministic
// percentile bootstrap CI. Paired statistics for A/B comparisons live in
// paired.go.
//
// The primitives were moved out of internal/goldbench/score.go unchanged;
// goldbench delegates to them and keeps its own axis-contract scorers. Their
// doc comments stay in the original German so the move is diff-verifiable —
// new code in this package is documented in English.
//
// Nothing here talks to a database, an LLM or a clock: every function is a
// pure computation over its arguments, and the one randomized function takes
// its seed as a parameter. That is what makes a report over this package
// reproducible.
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx
package evalscore

import (
	"math"
	"sort"
	"strings"

	"github.com/GottZ/ctx/internal/util"
)

// MicroF1 rechnet Precision/Recall/F1 aus micro-aggregierten Zählern.
func MicroF1(tp, fp, fn int) (precision, recall, f1 float64) {
	if tp+fp > 0 {
		precision = float64(tp) / float64(tp+fp)
	}
	if tp+fn > 0 {
		recall = float64(tp) / float64(tp+fn)
	}
	if precision+recall > 0 {
		f1 = 2 * precision * recall / (precision + recall)
	}
	return precision, recall, f1
}

// SetCounts zählt TP/FP/FN zwischen zwei String-Mengen (exakter Match).
func SetCounts(pred, gold map[string]bool) (tp, fp, fn int) {
	for p := range pred {
		if gold[p] {
			tp++
		} else {
			fp++
		}
	}
	for g := range gold {
		if !pred[g] {
			fn++
		}
	}
	return tp, fp, fn
}

// TokenF1 ist der Token-Overlap-F1 zweier Strings über util.TokenSet.
func TokenF1(pred, gold string) float64 {
	tp, fp, fn := SetCounts(util.TokenSet(pred), util.TokenSet(gold))
	_, _, f1 := MicroF1(tp, fp, fn)
	return f1
}

// NDCGBinary rechnet nDCG@k mit binärer Relevanz: scores sind die Judge-Werte
// je Dokument (Index = Dokument-Position), relevant die Gold-Indizes.
// Ranking: Score absteigend, Ties stabil nach Original-Index (die Reihenfolge,
// in der ctx die Docs an den Judge gibt).
func NDCGBinary(scores []float64, relevant []int, k int) float64 {
	if len(scores) == 0 {
		return 0
	}
	rel := map[int]bool{}
	for _, r := range relevant {
		if r >= 0 && r < len(scores) {
			rel[r] = true
		}
	}
	if len(rel) == 0 {
		return 0
	}
	idx := make([]int, len(scores))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return scores[idx[a]] > scores[idx[b]] })

	if k > len(idx) {
		k = len(idx)
	}
	dcg := 0.0
	for rank := 0; rank < k; rank++ {
		if rel[idx[rank]] {
			dcg += 1.0 / math.Log2(float64(rank)+2)
		}
	}
	ideal := 0.0
	n := len(rel)
	if n > k {
		n = k
	}
	for rank := 0; rank < n; rank++ {
		ideal += 1.0 / math.Log2(float64(rank)+2)
	}
	if ideal == 0 {
		return 0
	}
	return dcg / ideal
}

// MeanOrZero ist das arithmetische Mittel, 0 bei leerer Liste.
func MeanOrZero(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

// RatioOrZero ist num/den, 0 bei den==0.
func RatioOrZero(num, den int) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}

// TrgmSimilarity ist eine Go-Reimplementation der pg_trgm-similarity()
// (Postgres contrib/pg_trgm): lowercase, Nicht-Alphanumerik trennt Wörter,
// jedes Wort mit zwei führenden und einem abschließenden Leerzeichen gepolstert,
// Trigramm-Mengen, Ähnlichkeit = |A∩B| / |A∪B|.
//
// ABWEICHUNG (recurrence): In ctx liefert Phase 1 title_sim aus PG
// (internal/dream/recurrence.go:144 similarity(b.title, $2)); der Harness hat
// kein PG und rechnet den Wert strukturgleich in Go nach. Der Wert erscheint
// nur als Metadatum im Prompt-Header (recurrence.go:244).
func TrgmSimilarity(a, b string) float64 {
	ta, tb := trigramSet(a), trigramSet(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	inter := 0
	for t := range ta {
		if tb[t] {
			inter++
		}
	}
	union := len(ta) + len(tb) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// trigramSet baut die pg_trgm-Trigramm-Menge eines Strings.
func trigramSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r <= 127
	}) {
		padded := "  " + w + " "
		runes := []rune(padded)
		for i := 0; i+3 <= len(runes); i++ {
			out[string(runes[i:i+3])] = true
		}
	}
	return out
}

// BootstrapCI liefert das 95%-Perzentil-Bootstrap-CI des Mittelwerts der
// per-Case-Scores (1000 Resamples, deterministisch geseedet). Metrik v2:
// Achsen-Differenzen ohne CI sind bei kleinen n (23–36 Fälle) nicht
// interpretierbar — das CI macht das Rauschen sichtbar statt es zu verstecken.
//
// Das Niveau ist hier fest; der Kern mit explizitem Niveau ist PairedDiffCI.
func BootstrapCI(vals []float64, seed int64) (lo, hi float64) {
	return bootstrapPercentileCI(vals, 0.95, seed)
}
