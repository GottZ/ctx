package goldbench

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

// microF1 rechnet Precision/Recall/F1 aus micro-aggregierten Zählern.
func microF1(tp, fp, fn int) (precision, recall, f1 float64) {
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

// setCounts zählt TP/FP/FN zwischen zwei String-Mengen (exakter Match).
func setCounts(pred, gold map[string]bool) (tp, fp, fn int) {
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

// titleTokenRe zerlegt Titel in Score-Tokens: lowercase [a-z0-9äöüß]+.
var titleTokenRe = regexp.MustCompile(`[a-z0-9äöüß]+`)

// tokenSet liefert die Token-Menge eines Strings (lowercase, [a-z0-9äöüß]+).
func tokenSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, t := range titleTokenRe.FindAllString(strings.ToLower(s), -1) {
		out[t] = true
	}
	return out
}

// tokenF1 ist der Token-Overlap-F1 zweier Strings über tokenSet.
func tokenF1(pred, gold string) float64 {
	tp, fp, fn := setCounts(tokenSet(pred), tokenSet(gold))
	_, _, f1 := microF1(tp, fp, fn)
	return f1
}

// keywordMatch prüft den Achsen-Vertrag für keywords/tagging: ein Gold-Term
// gilt als getroffen, wenn er (lowercase) Substring eines Output-Terms ist
// oder umgekehrt.
func keywordMatch(gold, out string) bool {
	g := strings.ToLower(strings.TrimSpace(gold))
	o := strings.ToLower(strings.TrimSpace(out))
	if g == "" || o == "" {
		return false
	}
	return strings.Contains(o, g) || strings.Contains(g, o)
}

// keywordOverlap liefert Recall der Gold-Terme + Jaccard-Näherung.
// Jaccard nutzt die Zahl getroffener Gold-Terme als Schnittmengen-Proxy
// (Substring-Matching liefert keine exakte Mengen-Schnittmenge):
// |match| / (|gold| + |out| − |match|).
func keywordOverlap(goldTerms, outTerms []string) (recall, jaccard float64) {
	if len(goldTerms) == 0 {
		return 0, 0
	}
	matched := 0
	for _, g := range goldTerms {
		for _, o := range outTerms {
			if keywordMatch(g, o) {
				matched++
				break
			}
		}
	}
	recall = float64(matched) / float64(len(goldTerms))
	union := len(goldTerms) + len(outTerms) - matched
	if union > 0 {
		jaccard = float64(matched) / float64(union)
	}
	return recall, jaccard
}

// ndcgBinary rechnet nDCG@k mit binärer Relevanz: scores sind die Judge-Werte
// je Dokument (Index = Dokument-Position), relevant die Gold-Indizes.
// Ranking: Score absteigend, Ties stabil nach Original-Index (die Reihenfolge,
// in der ctx die Docs an den Judge gibt).
func ndcgBinary(scores []float64, relevant []int, k int) float64 {
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

// meanOrZero ist das arithmetische Mittel, 0 bei leerer Liste.
func meanOrZero(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

// ratioOrZero ist num/den, 0 bei den==0.
func ratioOrZero(num, den int) float64 {
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den)
}

// trgmSimilarity ist eine Go-Reimplementation der pg_trgm-similarity()
// (Postgres contrib/pg_trgm): lowercase, Nicht-Alphanumerik trennt Wörter,
// jedes Wort mit zwei führenden und einem abschließenden Leerzeichen gepolstert,
// Trigramm-Mengen, Ähnlichkeit = |A∩B| / |A∪B|.
//
// ABWEICHUNG (recurrence): In ctx liefert Phase 1 title_sim aus PG
// (internal/dream/recurrence.go:144 similarity(b.title, $2)); der Harness hat
// kein PG und rechnet den Wert strukturgleich in Go nach. Der Wert erscheint
// nur als Metadatum im Prompt-Header (recurrence.go:244).
func trgmSimilarity(a, b string) float64 {
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
