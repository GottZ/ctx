package armsweep

import (
	"fmt"
	"sort"

	"github.com/GottZ/ctx/internal/evalscore"
	"github.com/GottZ/ctx/internal/goldset"
)

// Metric cutoffs (§4.6). Fixed rather than configurable: a report whose k moved
// between runs is not comparable to the report before it, and the three
// cutoffs are what the design gates are stated in.
const (
	NDCGCut   = 10
	RecallCut = 5
	MRRCut    = 10
)

// NoiseDiscordanceMax is the G-NOISE ceiling on Recall@5 discordance between
// the two replicate dumps (§4.9): above 5 % the instrument's own repeat
// disagreement is large enough that a variant effect cannot be told from it.
const NoiseDiscordanceMax = 0.05

// SecondaryComparisons is the Bonferroni denominator: the 13 non-primary
// variants (S1-S4, V2-V5, V6a/b, V7a-c). V1 is the ONE pre-registered primary
// comparison and is read at 0.95; everything else is read at 1−0.05/13 and, if
// it clears, labelled a candidate rather than a result.
const SecondaryComparisons = 13

// PrimaryLevel and SecondaryLevel are the two confidence levels of §4.9.
const PrimaryLevel = 0.95

// SecondaryLevel is the Bonferroni-corrected level.
var SecondaryLevel = 1 - 0.05/float64(SecondaryComparisons)

// Report slice keys. G-Q is never scored as one slice: DERIV is the half
// variants are derived on and HOLD the half that confirms them, so pooling
// them would let a derivation grade its own homework.
const (
	SliceKI       = goldset.SliceKI
	SliceQDeriv   = goldset.SliceQ + "-" + goldset.SplitDeriv
	SliceQHold    = goldset.SliceQ + "-" + goldset.SplitHold
	SliceRealName = goldset.SliceReal
)

// ReportSlices is the canonical slice order of every report.
func ReportSlices() []string { return []string{SliceKI, SliceQDeriv, SliceQHold, SliceRealName} }

// SliceKeyOf maps a record to its report slice.
func SliceKeyOf(rec Record) string {
	if rec.Slice == goldset.SliceQ && rec.Split != "" {
		return rec.Slice + "-" + rec.Split
	}
	return rec.Slice
}

// CaseScore is one case under one configuration.
type CaseScore struct {
	Key     string  `json:"key"`
	NDCG10  float64 `json:"ndcg_10"`
	Recall5 float64 `json:"recall_5"`
	MRR10   float64 `json:"mrr_10"`
	Hit5    bool    `json:"hit_5"`
}

// ScoreCase re-fuses one dump record under one configuration and measures it.
//
// The ranking scored is the OFFLINE fusion, not the delivered one: that is the
// entire construction of the instrument. What the post-fusion stages did to the
// delivered order is recorded in the dump and reported in the stamp, but it is
// not what a weight vector is judged on.
func ScoreCase(rec Record, cfg Config) CaseScore {
	gold := make(map[string]bool, len(rec.GoldIDs))
	for _, id := range rec.GoldIDs {
		gold[id] = true
	}
	ranked := FusedIDs(Fuse(rec.Rows, cfg))
	return CaseScore{
		Key:     rec.Key(),
		NDCG10:  evalscore.NDCGRanked(ranked, gold, NDCGCut),
		Recall5: evalscore.RecallAtK(ranked, gold, RecallCut),
		MRR10:   evalscore.MRRAtK(ranked, gold, MRRCut),
		Hit5:    evalscore.HitAtK(ranked, gold, RecallCut),
	}
}

// SliceMetrics is one configuration's profile on one slice.
type SliceMetrics struct {
	Slice   string  `json:"slice"`
	N       int     `json:"n"`
	NDCG10  float64 `json:"ndcg_10"`
	Recall5 float64 `json:"recall_5"`
	MRR10   float64 `json:"mrr_10"`
	// NDCGCILo/Hi is the absolute bootstrap CI of the mean nDCG@10 — the
	// spread of the figure itself, not of a difference. A slice mean without
	// it is not interpretable at n=100.
	NDCGCILo float64 `json:"ndcg_10_ci_lo"`
	NDCGCIHi float64 `json:"ndcg_10_ci_hi"`
	// Unlabelled marks a slice that carries no relevance judgements yet
	// (G-REAL until wave B-W6). Reported and skipped, never scored as zero.
	Unlabelled bool `json:"unlabelled"`
}

// ConfigResult is one configuration across all report slices.
type ConfigResult struct {
	Config Config         `json:"config"`
	Dump   string         `json:"dump"`
	Slices []SliceMetrics `json:"slices"`
}

// Comparison is one variant against V0 on one slice.
type Comparison struct {
	Config      string                  `json:"config"`
	Slice       string                  `json:"slice"`
	N           int                     `json:"n"`
	Level       float64                 `json:"level"`
	DeltaNDCG   float64                 `json:"delta_ndcg_10"`
	CILo        float64                 `json:"ci_lo"`
	CIHi        float64                 `json:"ci_hi"`
	McNemar     evalscore.McNemarResult `json:"mcnemar_hit_5"`
	Discordance float64                 `json:"discordance_hit_5"`
}

// caseSet is the aligned per-case view one configuration produces on one slice.
type caseSet struct {
	keys   []string
	scores map[string]CaseScore
}

// scoreSlices scores one configuration over a record set, grouped by report
// slice. Keys are sorted, so every downstream pairing walks the same order.
func scoreSlices(recs []Record, cfg Config) map[string]*caseSet {
	out := map[string]*caseSet{}
	for _, rec := range recs {
		k := SliceKeyOf(rec)
		cs, ok := out[k]
		if !ok {
			cs = &caseSet{scores: map[string]CaseScore{}}
			out[k] = cs
		}
		cs.keys = append(cs.keys, rec.Key())
		cs.scores[rec.Key()] = ScoreCase(rec, cfg)
	}
	for _, cs := range out {
		sort.Strings(cs.keys)
	}
	return out
}

// labelled reports whether a slice's records carry relevance judgements.
func labelledCounts(recs []Record) map[string][2]int {
	out := map[string][2]int{}
	for _, rec := range recs {
		k := SliceKeyOf(rec)
		c := out[k]
		c[0]++
		if len(rec.GoldIDs) > 0 {
			c[1]++
		}
		out[k] = c
	}
	return out
}

// metricsOf aggregates a case set into the reported slice profile.
func metricsOf(slice string, cs *caseSet, unlabelled bool, seed int64) SliceMetrics {
	m := SliceMetrics{Slice: slice, Unlabelled: unlabelled}
	if cs == nil {
		return m
	}
	m.N = len(cs.keys)
	if unlabelled {
		return m
	}
	ndcg := make([]float64, 0, len(cs.keys))
	var rec, mrr []float64
	for _, k := range cs.keys {
		s := cs.scores[k]
		ndcg = append(ndcg, s.NDCG10)
		rec = append(rec, s.Recall5)
		mrr = append(mrr, s.MRR10)
	}
	m.NDCG10 = evalscore.MeanOrZero(ndcg)
	m.Recall5 = evalscore.MeanOrZero(rec)
	m.MRR10 = evalscore.MeanOrZero(mrr)
	m.NDCGCILo, m.NDCGCIHi = evalscore.PairedDiffCI(ndcg, PrimaryLevel, seed)
	return m
}

// compare pairs a variant against a baseline on one slice over the INTERSECTION
// of their case keys. The intersection is not a convenience: an exclusion that
// hit one dump and not the other would otherwise pair case i of one run with
// case i of a different case set.
func compare(name, slice string, base, variant *caseSet, level float64, seed int64) (Comparison, bool) {
	if base == nil || variant == nil {
		return Comparison{}, false
	}
	var deltas []float64
	var baseHit, varHit []bool
	for _, k := range base.keys {
		v, ok := variant.scores[k]
		if !ok {
			continue
		}
		b := base.scores[k]
		deltas = append(deltas, v.NDCG10-b.NDCG10)
		baseHit = append(baseHit, b.Hit5)
		varHit = append(varHit, v.Hit5)
	}
	if len(deltas) == 0 {
		return Comparison{}, false
	}
	mc, err := evalscore.McNemarPaired(baseHit, varHit)
	if err != nil {
		return Comparison{}, false
	}
	lo, hi := evalscore.PairedDiffCI(deltas, level, seed)
	return Comparison{
		Config: name, Slice: slice, N: len(deltas), Level: level,
		DeltaNDCG: evalscore.MeanOrZero(deltas), CILo: lo, CIHi: hi,
		McNemar: mc, Discordance: evalscore.RatioOrZero(mc.Discordant, len(deltas)),
	}, true
}

// NoiseGate is the G-NOISE verdict on one slice (§4.9): the replicate pair's
// own disagreement. It is the gate everything else hangs off — a variant effect
// smaller than the instrument's repeat noise is not an effect.
type NoiseGate struct {
	Slice       string   `json:"slice"`
	N           int      `json:"n"`
	Discordance float64  `json:"discordance_hit_5"`
	Threshold   float64  `json:"threshold"`
	CILo        float64  `json:"ci_lo"`
	CIHi        float64  `json:"ci_hi"`
	Pass        bool     `json:"pass"`
	Reasons     []string `json:"reasons,omitempty"`
}

// evaluateNoise applies the two G-NOISE conditions to a replicate comparison.
func evaluateNoise(cmp Comparison) NoiseGate {
	g := NoiseGate{
		Slice: cmp.Slice, N: cmp.N, Discordance: cmp.Discordance,
		Threshold: NoiseDiscordanceMax, CILo: cmp.CILo, CIHi: cmp.CIHi,
	}
	if cmp.Discordance > NoiseDiscordanceMax {
		g.Reasons = append(g.Reasons, fmt.Sprintf(
			"Recall@5 discordance %.4f exceeds %.2f — the replicate pair disagrees on %d of %d cases",
			cmp.Discordance, NoiseDiscordanceMax, cmp.McNemar.Discordant, cmp.N))
	}
	if cmp.CILo > 0 || cmp.CIHi < 0 {
		g.Reasons = append(g.Reasons, fmt.Sprintf(
			"paired 95%% CI of ΔnDCG@10 excludes 0 ([%.5f, %.5f]) — the two replicates differ systematically",
			cmp.CILo, cmp.CIHi))
	}
	g.Pass = len(g.Reasons) == 0
	return g
}

// WinGate is the G-WIN verdict for one variant (§4.9). Confirmed means the
// full gate cleared on G-Q-HOLD; Candidate means a Bonferroni-corrected
// secondary comparison cleared and is explicitly NOT a confirmed result.
type WinGate struct {
	Config    string   `json:"config"`
	Level     float64  `json:"level"`
	Primary   bool     `json:"primary"`
	Confirmed bool     `json:"confirmed"`
	Candidate bool     `json:"candidate"`
	Label     string   `json:"label"`
	Reasons   []string `json:"reasons,omitempty"`
}

// evaluateWin applies the four G-WIN conditions.
//
// hold is the variant-vs-V0 comparison on G-Q-HOLD, noiseRef the replicate
// pair's comparison on the same slice, others the comparisons on every OTHER
// labelled slice. All four conditions must hold; the reasons list says which
// did not, so a near-miss is readable rather than a bare false.
func evaluateWin(name string, primary bool, hold *Comparison, noiseRef *Comparison, others []Comparison) WinGate {
	g := WinGate{Config: name, Primary: primary, Level: PrimaryLevel}
	if !primary {
		g.Level = SecondaryLevel
	}
	if hold == nil {
		g.Reasons = append(g.Reasons, "no comparison on "+SliceQHold+" — the confirming half is missing")
		g.Label = "not evaluated"
		return g
	}
	if !(hold.CILo > 0) {
		g.Reasons = append(g.Reasons, fmt.Sprintf(
			"CI of ΔnDCG@10 on %s is [%.5f, %.5f] — it must exclude 0 from above", SliceQHold, hold.CILo, hold.CIHi))
	}
	switch {
	case noiseRef == nil:
		g.Reasons = append(g.Reasons, "no V0/V0' reference discordance on "+SliceQHold)
	case !(hold.Discordance > noiseRef.Discordance):
		g.Reasons = append(g.Reasons, fmt.Sprintf(
			"McNemar discordance %.4f does not exceed the V0/V0' reference %.4f — the variant moves fewer cases than the noise does",
			hold.Discordance, noiseRef.Discordance))
	}
	for _, o := range others {
		if o.CIHi < 0 {
			g.Reasons = append(g.Reasons, fmt.Sprintf(
				"slice %s regresses: CI [%.5f, %.5f] lies entirely below 0", o.Slice, o.CILo, o.CIHi))
		}
	}
	if len(g.Reasons) > 0 {
		g.Label = "no win"
		return g
	}
	if primary {
		g.Confirmed, g.Label = true, "confirmed"
		return g
	}
	g.Candidate, g.Label = true, "candidate, unconfirmed"
	return g
}

// soloProfile reads the four solo-arm nDCG@10 values off G-Q-DERIV, which is
// what the V6 weight derivation is defined on (§4.6).
func soloProfile(results []ConfigResult) SoloNDCG {
	get := func(name string) float64 {
		for _, r := range results {
			if r.Config.Name != name {
				continue
			}
			for _, s := range r.Slices {
				if s.Slice == SliceQDeriv {
					return s.NDCG10
				}
			}
		}
		return 0
	}
	return SoloNDCG{Semantic: get("S1"), FTSDe: get("S2"), FTSEn: get("S3"), Trigram: get("S4")}
}
