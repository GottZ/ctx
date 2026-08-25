package goldbench

import (
	"strings"

	"github.com/GottZ/ctx/internal/evalscore"
)

// Die allgemeinen Scoring-Primitive (micro-F1, Token-F1, nDCG@k, die
// Aggregations-Helfer, die pg_trgm-Nachrechnung und das Bootstrap-CI) liegen in
// internal/evalscore und werden auch vom Retrieval-Sweep genutzt. Hier bleiben
// die unexportierten Namen als Delegationen stehen — die Achsen-Scorer rufen
// sie unverändert auf — und darunter die Metriken, die am ctx-Achsen-Vertrag
// hängen und deshalb goldbench-spezifisch sind (keywords/tagging).

// microF1 rechnet Precision/Recall/F1 aus micro-aggregierten Zählern.
func microF1(tp, fp, fn int) (precision, recall, f1 float64) {
	return evalscore.MicroF1(tp, fp, fn)
}

// setCounts zählt TP/FP/FN zwischen zwei String-Mengen (exakter Match).
func setCounts(pred, gold map[string]bool) (tp, fp, fn int) {
	return evalscore.SetCounts(pred, gold)
}

// tokenF1 ist der Token-Overlap-F1 zweier Strings.
func tokenF1(pred, gold string) float64 {
	return evalscore.TokenF1(pred, gold)
}

// ndcgBinary rechnet nDCG@k mit binärer Relevanz.
func ndcgBinary(scores []float64, relevant []int, k int) float64 {
	return evalscore.NDCGBinary(scores, relevant, k)
}

// meanOrZero ist das arithmetische Mittel, 0 bei leerer Liste.
func meanOrZero(vals []float64) float64 {
	return evalscore.MeanOrZero(vals)
}

// ratioOrZero ist num/den, 0 bei den==0.
func ratioOrZero(num, den int) float64 {
	return evalscore.RatioOrZero(num, den)
}

// trgmSimilarity ist die Go-Reimplementation der pg_trgm-similarity().
func trgmSimilarity(a, b string) float64 {
	return evalscore.TrgmSimilarity(a, b)
}

// bootstrapCI liefert das 95%-Perzentil-Bootstrap-CI des Mittelwerts der
// per-Case-Scores (1000 Resamples, deterministisch geseedet).
func bootstrapCI(vals []float64, seed int64) (lo, hi float64) {
	return evalscore.BootstrapCI(vals, seed)
}

// keywordMatch prüft den Achsen-Vertrag für keywords/tagging: ein Gold-Term
// gilt als getroffen, wenn er (lowercase) Substring eines Output-Terms ist
// oder umgekehrt. Metrik v2 (SC-2): Terme unter 3 Zeichen matchen nur exakt —
// die Substring-Richtung würde sonst von Kurz-Tokens trivial erfüllt.
func keywordMatch(gold, out string) bool {
	g := strings.ToLower(strings.TrimSpace(gold))
	o := strings.ToLower(strings.TrimSpace(out))
	if g == "" || o == "" {
		return false
	}
	if len(g) < 3 || len(o) < 3 {
		return g == o
	}
	return strings.Contains(o, g) || strings.Contains(g, o)
}

// keywordCapN ist der Prediction-Cap der keywords/tagging-Achsen (Metrik v2,
// SC-1): gescored werden höchstens die ersten 10 Output-Terme — der
// ctx-Vertrag verlangt 5–8 Konzepte (dream/keywords.go), Über-Generierung
// darf den Recall nicht gratis maximieren.
const keywordCapN = 10

// keywordSetF1 ist die v2-Primärmetrik für keywords/tagging: Set-F1 über die
// gecappten Output-Terme. precision = getroffene Output-Terme / |out_cap|,
// recall = getroffene Gold-Terme / |gold|.
func keywordSetF1(goldTerms, outTerms []string) float64 {
	if len(goldTerms) == 0 {
		return 0
	}
	out := outTerms
	if len(out) > keywordCapN {
		out = out[:keywordCapN]
	}
	matchedGold := 0
	for _, g := range goldTerms {
		for _, o := range out {
			if keywordMatch(g, o) {
				matchedGold++
				break
			}
		}
	}
	matchedOut := 0
	for _, o := range out {
		for _, g := range goldTerms {
			if keywordMatch(g, o) {
				matchedOut++
				break
			}
		}
	}
	if len(out) == 0 {
		return 0
	}
	p := float64(matchedOut) / float64(len(out))
	r := float64(matchedGold) / float64(len(goldTerms))
	if p+r == 0 {
		return 0
	}
	return 2 * p * r / (p + r)
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
