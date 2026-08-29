package armsweep

// The metric flip test of wave C3-4a (amendment design/05a §C3-2-D05-6).
//
// D-05 §4.5 (3) has always said it: "kippt ein Gate zwischen maschinell- und
// menschlich-geurteilter Rechnung, gilt das Gate als nicht entschieden". Until
// now that sentence could not be executed, because the two gold sources never
// existed over the same queries — the C2-6c calibration judged control draws,
// which carry no gold at all. On the 20 fully judged core queries of C3-4a they
// do, so the comparison runs twice over the SAME records and the same fusion,
// and only the gold set changes.
//
// That is what makes it a stronger test than kappa: kappa compares two verdict
// MARGINALS, this compares the two figures a gate is actually read off.
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx

import (
	"github.com/GottZ/ctx/internal/evalscore"
	"github.com/GottZ/ctx/internal/goldset"
)

// ScoreCaseGold measures one case against a NAMED gold set rather than against
// the one the record carries (§C3-2-D05-8 j). Everything else — fusion, basis,
// cut-offs — is what ScoreCaseOn does, and passing rec.GoldIDs reproduces it
// exactly.
func ScoreCaseGold(rec Record, cfg Config, basis string, goldIDs []string) CaseScore {
	gold := make(map[string]bool, len(goldIDs))
	for _, id := range goldIDs {
		gold[id] = true
	}
	ranked := RankedIDs(rec, cfg, basis)
	return CaseScore{
		Key:     rec.Key(),
		NDCG10:  evalscore.NDCGRanked(ranked, gold, NDCGCut),
		Recall5: evalscore.RecallAtK(ranked, gold, RecallCut),
		MRR10:   evalscore.MRRAtK(ranked, gold, MRRCut),
		Hit5:    evalscore.HitAtK(ranked, gold, RecallCut),
	}
}

// GoldFlip scores one variant against one baseline twice over the same records
// — once against goldA (Fable) and once against goldB (judge) — and reports the
// two mean ΔnDCG@10 plus the paired bootstrap CI of their per-case difference.
//
// The CI is taken over the DIFFERENCE of the two deltas rather than over each
// delta separately, and that is the point: the two computations share the
// ranking, the fusion and the case set, so an interval over the difference asks
// "does the gold source move the figure", which is the question the gate hangs
// on. Two independent intervals would answer a question nobody asked and would
// overlap on almost any real data.
//
// A record without an entry in BOTH gold maps is skipped rather than scored
// against an empty gold set: an unlabelled case scores 0 on every metric, and
// a 0 that came from a missing label is indistinguishable from a 0 the
// retrieval earned.
func GoldFlip(recs []Record, base, variant Config, goldA, goldB map[string][]string,
	level float64, seed int64,
) goldset.MetricFlip {
	out := goldset.MetricFlip{Metric: "nDCG@10"}
	var deltaA, deltaB, diff []float64
	for _, rec := range recs {
		ga, okA := goldA[rec.Key()]
		gb, okB := goldB[rec.Key()]
		if !okA || !okB {
			continue
		}
		da := ScoreCaseGold(rec, variant, RankingBasisFused, ga).NDCG10 -
			ScoreCaseGold(rec, base, RankingBasisFused, ga).NDCG10
		db := ScoreCaseGold(rec, variant, RankingBasisFused, gb).NDCG10 -
			ScoreCaseGold(rec, base, RankingBasisFused, gb).NDCG10
		deltaA = append(deltaA, da)
		deltaB = append(deltaB, db)
		diff = append(diff, da-db)
	}
	if len(diff) == 0 {
		return out
	}
	out.Available, out.N = true, len(diff)
	out.DeltaFable = evalscore.MeanOrZero(deltaA)
	out.DeltaJudge = evalscore.MeanOrZero(deltaB)
	out.DiffCILo, out.DiffCIHi = evalscore.PairedDiffCI(diff, level, seed)
	return out
}

// GoldOf projects a labelled case set onto the gold map GoldFlip reads.
func GoldOf(cases []goldset.Case) map[string][]string {
	out := make(map[string][]string, len(cases))
	for _, c := range cases {
		out[c.Key()] = c.GoldIDs
	}
	return out
}
