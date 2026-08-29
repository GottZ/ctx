package goldset

// The calibration of wave C3-4a (amendment design/05a §C3-2-D05-6, -8 e/f/h).
//
// Two things change against C2-6c, and only these two.
//
// FIRST, kappa is computed twice. The unweighted figure is the stabler number
// and is reported, but it describes a population that does not exist — the
// stratified sample over-represents S1/S2 by construction (its judge positive
// rate is 0.5417 against roughly 0.24 in the population). The WEIGHTED figure
// is the one with the population base rate behind it, so it is the one the
// threshold reads.
//
// SECOND, the threshold changed its job. Under E2-4 the machine was the gold
// source and kappa decided whether a gate could rest on it. Under E4-4
// ("fable-judge") Fable's verdicts ARE the gold, so kappa is a statement ABOUT
// the machine judge: below 0.6 the judge labels may not stand in for gold
// OUTSIDE the core, and the gate computation falls back to the 20 core queries.
// The number 0.6 is unchanged; what it decides is the REACH of the gold, not
// the verdict of the gate. The gate authority moves to the metric flip.
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// CalibrationPair is one calibrated cell: the machine verdict, Fable's verdict,
// and the draw facts the Horvitz-Thompson estimators need (§C3-2-D05-8 e).
type CalibrationPair struct {
	Slice       string
	Stratum     string
	QuerySHA256 string
	BlockID     string
	Weight      float64
	CoreQuery   bool
	Control     bool
	LLM         bool
	Fable       SheetVerdict
}

// UnweightedPairs projects calibration pairs onto the C2-6c pair type, so the
// existing Kappa keeps computing exactly what it computed before.
func UnweightedPairs(p []CalibrationPair) []JudgePair {
	out := make([]JudgePair, 0, len(p))
	for _, x := range p {
		out = append(out, JudgePair{Slice: x.Slice, LLM: x.LLM, Control: x.Fable.Relevant()})
	}
	return out
}

// JoinCalibration binds a filled sheet back to its draw key over
// (query_sha256, block_id) — never over the line number, because a sheet that
// was re-sorted or re-wrapped would then attach every verdict to the wrong cell
// without producing one error (§C3-2-D05-5, rule 6).
func JoinCalibration(k DrawKey, filled []FableJudgement) ([]CalibrationPair, error) {
	if len(k.Cells) == 0 {
		return nil, fmt.Errorf("Ziehungs-Schlüssel ohne Zellen — es gibt nichts zu verbinden")
	}
	answers := make(map[string]SheetVerdict, len(filled))
	for _, f := range filled {
		key := f.QuerySHA256 + "/" + f.BlockID
		if prev, dup := answers[key]; dup && prev != f.Verdict {
			return nil, fmt.Errorf("%s: zweimal mit verschiedenen Urteilen im Bogen", key)
		}
		answers[key] = f.Verdict
	}
	out := make([]CalibrationPair, 0, len(k.Cells))
	for _, c := range k.Cells {
		v, ok := answers[c.joinKey()]
		if !ok {
			return nil, fmt.Errorf("%s/%s: gezogen, aber ohne Urteil — "+
				"ein unvollständiger Bogen verzerrt jede Hochrechnung", c.QuerySHA256, c.BlockID)
		}
		out = append(out, CalibrationPair{
			Slice: c.Slice, Stratum: c.Stratum, QuerySHA256: c.QuerySHA256, BlockID: c.BlockID,
			Weight: c.Weight, CoreQuery: c.CoreQuery, Control: c.Control, LLM: c.LLMRelevant, Fable: v,
		})
	}
	if extra := len(answers) - len(out); extra > 0 {
		return nil, fmt.Errorf("der Bogen hält %d Urteile mehr als der Schlüssel Zellen — "+
			"Bogen und Schlüssel gehören nicht zusammen", extra)
	}
	return out, nil
}

// FableJudgements projects a filled sheet onto the Judgement form ApplyLabels
// reads, so the human verdicts can build a gold variant through exactly the
// path the machine verdicts use (§C3-2-D05-7, step 4).
//
// `stratum` restricts the projection; the core variant passes StratumCore. A
// verdict of `?` becomes `relevant = false` here rather than being dropped: the
// rule was declared before the run (§C3-2-D05-5), and a dropped cell would
// silently shrink the pool a Recall@5 denominator is computed over — which is a
// different error from the one the rule avoids.
func FableJudgements(k DrawKey, filled []FableJudgement, stratum string) (map[string][]Judgement, error) {
	pairs, err := JoinCalibration(k, filled)
	if err != nil {
		return nil, err
	}
	byCell := make(map[string]CalibrationPair, len(pairs))
	for _, p := range pairs {
		byCell[p.QuerySHA256+"/"+p.BlockID] = p
	}
	out := map[string][]Judgement{}
	for _, c := range k.Cells {
		if stratum != "" && c.Stratum != stratum {
			continue
		}
		p := byCell[c.joinKey()]
		key := CaseKey(c.Slice, c.Index, c.QuerySHA256)
		out[key] = append(out[key], Judgement{
			Slice: c.Slice, Index: c.Index, QuerySHA256: c.QuerySHA256,
			BlockID: c.BlockID, Relevant: p.Fable.Relevant(),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: keine Zelle im Ziehungs-Schlüssel — die Gold-Variante wäre leer", stratum)
	}
	for key := range out {
		sort.Slice(out[key], func(i, j int) bool { return out[key][i].BlockID < out[key][j].BlockID })
	}
	return out, nil
}

// WeightedKappaResult is Cohen's kappa on the Horvitz-Thompson weighted table.
type WeightedKappaResult struct {
	Slice string  `json:"slice,omitempty"`
	N     int     `json:"n"`
	W     float64 `json:"weight_total"`

	BothW    float64 `json:"both_relevant_w"`
	LLMOnlyW float64 `json:"llm_only_w"`
	FableW   float64 `json:"fable_only_w"`
	NeitherW float64 `json:"neither_relevant_w"`

	Agreement float64 `json:"observed_agreement"`
	Expected  float64 `json:"expected_agreement"`
	Kappa     float64 `json:"kappa"`
	// MarginalP stays the EXACT McNemar over the UNWEIGHTED discordant counts.
	// The exact test is a statement about counts of pairs; feeding it weighted
	// pseudo-counts would report a precision the sample does not have.
	MarginalP     float64 `json:"marginal_p"`
	NotComputable bool    `json:"not_computable"`
}

// KappaWeighted computes kappa over the weighted 2x2 table.
func KappaWeighted(p []CalibrationPair) WeightedKappaResult {
	r := WeightedKappaResult{N: len(p)}
	b, c := 0, 0
	for _, x := range p {
		fable := x.Fable.Relevant()
		r.W += x.Weight
		switch {
		case x.LLM && fable:
			r.BothW += x.Weight
		case x.LLM:
			r.LLMOnlyW += x.Weight
			b++
		case fable:
			r.FableW += x.Weight
			c++
		default:
			r.NeitherW += x.Weight
		}
	}
	r.MarginalP = mcNemarExact(b, c)
	if r.N == 0 || r.W <= 0 {
		r.NotComputable = true
		return r
	}
	r.Agreement = (r.BothW + r.NeitherW) / r.W
	llmRel, fableRel := (r.BothW+r.LLMOnlyW)/r.W, (r.BothW+r.FableW)/r.W
	r.Expected = llmRel*fableRel + (1-llmRel)*(1-fableRel)
	if 1-r.Expected <= 1e-12 {
		r.NotComputable = true
		return r
	}
	r.Kappa = (r.Agreement - r.Expected) / (1 - r.Expected)
	return r
}

// StratumStats is one stratum's profile, including the `?`-rate that
// §C3-2-D05-5 declares a quality figure of the RUBRIC rather than of a judge.
type StratumStats struct {
	Stratum       string  `json:"stratum"`
	N             int     `json:"n"`
	Population    int     `json:"population"`
	Weight        float64 `json:"weight"`
	LLMRelevant   int     `json:"llm_relevant"`
	FableRelevant int     `json:"fable_relevant"`
	Both          int     `json:"both_relevant"`
	Unsure        int     `json:"unsure"`
	UnsureRate    float64 `json:"unsure_rate"`
}

// RatioEstimate is a Horvitz-Thompson ratio with its 95 % interval.
type RatioEstimate struct {
	Value   float64 `json:"value"`
	CILo    float64 `json:"ci_lo"`
	CIHi    float64 `json:"ci_hi"`
	Num     float64 `json:"numerator_w"`
	Den     float64 `json:"denominator_w"`
	N       int     `json:"n"`
	Defined bool    `json:"defined"`
}

// CalibrationResult is everything one calibration run reports.
type CalibrationResult struct {
	Slice      string              `json:"slice,omitempty"`
	Pairs      int                 `json:"calibration_pairs"`
	Unweighted KappaResult         `json:"kappa_unweighted"`
	Weighted   WeightedKappaResult `json:"kappa_weighted"`
	Rho        RatioEstimate       `json:"rho_sensitivity"`
	Pi         RatioEstimate       `json:"pi_precision"`
	Strata     []StratumStats      `json:"strata"`
	Core       StratumStats        `json:"core"`

	ControlHits int     `json:"control_hits"`
	ControlN    int     `json:"control_n"`
	ControlRate float64 `json:"control_hit_rate"`
}

// z95 is the two-sided normal quantile of the 95 % interval — the campaign's
// standing level (marginalAlpha = 0.05), not a new number.
const z95 = 1.959963984540054

// Calibrate computes the whole calibration profile.
//
// The partition is the substance of §C3-2-D05-3: kappa, rho and pi are read
// over the four sampling strata ONLY. The core is a census and carries no
// sampling variance; the controls measure the pooling bias and were never an
// instrument of agreement — putting either into kappa is exactly the error the
// amendment corrects.
func Calibrate(pairs []CalibrationPair) CalibrationResult {
	res := CalibrationResult{}
	sample := make([]CalibrationPair, 0, len(pairs))
	byStratum := map[string][]CalibrationPair{}
	for _, p := range pairs {
		if res.Slice == "" {
			res.Slice = p.Slice
		}
		byStratum[p.Stratum] = append(byStratum[p.Stratum], p)
		switch p.Stratum {
		case StratumS0:
			res.ControlN++
			if p.Fable.Relevant() {
				res.ControlHits++
			}
		case StratumCore:
		default:
			sample = append(sample, p)
		}
	}
	if res.ControlN > 0 {
		res.ControlRate = float64(res.ControlHits) / float64(res.ControlN)
	}
	res.Pairs = len(sample)
	res.Unweighted = Kappa(UnweightedPairs(sample))
	res.Weighted = KappaWeighted(sample)
	res.Rho = ratioHT(sample,
		func(p CalibrationPair) float64 { return boolTo(p.LLM && p.Fable.Relevant()) },
		func(p CalibrationPair) float64 { return boolTo(p.Fable.Relevant()) })
	res.Pi = ratioHT(sample,
		func(p CalibrationPair) float64 { return boolTo(p.LLM && p.Fable.Relevant()) },
		func(p CalibrationPair) float64 { return boolTo(p.LLM) })
	for _, s := range sortedStrata(byStratum) {
		st := statsOf(s, byStratum[s])
		if s == StratumCore {
			res.Core = st
			continue
		}
		res.Strata = append(res.Strata, st)
	}
	return res
}

func sortedStrata(m map[string][]CalibrationPair) []string {
	out := make([]string, 0, len(m))
	for s := range m {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return stratumOrder[out[i]] < stratumOrder[out[j]] })
	return out
}

func statsOf(name string, p []CalibrationPair) StratumStats {
	st := StratumStats{Stratum: name, N: len(p)}
	if len(p) > 0 {
		st.Weight = p[0].Weight
		st.Population = int(math.Round(st.Weight * float64(len(p))))
	}
	for _, x := range p {
		if x.LLM {
			st.LLMRelevant++
		}
		if x.Fable.Relevant() {
			st.FableRelevant++
		}
		if x.LLM && x.Fable.Relevant() {
			st.Both++
		}
		if x.Fable == SheetUnsure {
			st.Unsure++
		}
	}
	if st.N > 0 {
		st.UnsureRate = float64(st.Unsure) / float64(st.N)
	}
	return st
}

func boolTo(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// ratioHT is the stratified Horvitz-Thompson ratio estimator with the
// linearised variance and the finite-population correction:
//
//	R = Y/X, e_i = y_i − R·x_i,
//	V(R) = X^-2 · Σ_h N_h²(1−f_h)/n_h · s²_h(e),  f_h = n_h/N_h.
//
// The correction is not cosmetic here: S1 draws 25 % of its stratum, and
// ignoring f would inflate the interval by a quarter on exactly the stratum the
// sensitivity figure rests on.
func ratioHT(p []CalibrationPair, num, den func(CalibrationPair) float64) RatioEstimate {
	byStratum := map[string][]CalibrationPair{}
	for _, x := range p {
		byStratum[x.Stratum] = append(byStratum[x.Stratum], x)
	}
	var y, x float64
	for _, cells := range byStratum {
		for _, c := range cells {
			y += c.Weight * num(c)
			x += c.Weight * den(c)
		}
	}
	est := RatioEstimate{Num: y, Den: x, N: len(p)}
	if x <= 0 {
		return est
	}
	est.Value, est.Defined = y/x, true
	variance := 0.0
	for _, cells := range byStratum {
		n := len(cells)
		if n < 2 {
			continue
		}
		w := cells[0].Weight
		nPop := w * float64(n)
		f := float64(n) / nPop
		mean := 0.0
		e := make([]float64, n)
		for i, c := range cells {
			e[i] = num(c) - est.Value*den(c)
			mean += e[i]
		}
		mean /= float64(n)
		ss := 0.0
		for _, v := range e {
			ss += (v - mean) * (v - mean)
		}
		s2 := ss / float64(n-1)
		variance += nPop * nPop * (1 - f) / float64(n) * s2
	}
	se := math.Sqrt(variance) / x
	est.CILo, est.CIHi = clamp01(est.Value-z95*se), clamp01(est.Value+z95*se)
	return est
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

// ------------------------------------------------------------- metric flip.

// MetricFlip is the outcome of the §C3-2-D05-6 flip test: the SAME comparison
// scored once against Fable gold and once against judge gold on the core.
//
// It is a plain result type rather than a computation because the computation
// lives in armsweep, which imports this package — the dependency only runs one
// way, and a second copy of the scorer here would be a second scorer.
type MetricFlip struct {
	Available  bool    `json:"available"`
	Metric     string  `json:"metric"`
	N          int     `json:"n"`
	DeltaFable float64 `json:"delta_fable_gold"`
	DeltaJudge float64 `json:"delta_judge_gold"`
	DiffCILo   float64 `json:"diff_ci_lo"`
	DiffCIHi   float64 `json:"diff_ci_hi"`
}

// SignFlip reports the sign change of the two deltas.
func (f MetricFlip) SignFlip() bool {
	return f.Available && ((f.DeltaFable > 0 && f.DeltaJudge < 0) || (f.DeltaFable < 0 && f.DeltaJudge > 0))
}

// CIExcludesZero reports whether the paired interval of the DIFFERENCE of the
// two computations excludes 0 — the second half of the rule, and the one that
// catches a gold source that changes the size of an effect without changing its
// direction.
func (f MetricFlip) CIExcludesZero() bool {
	return f.Available && (f.DiffCILo > 0 || f.DiffCIHi < 0)
}

// Flipped is the gate condition.
func (f MetricFlip) Flipped() bool { return f.SignFlip() || f.CIExcludesZero() }

// ------------------------------------------------------------------ gates.

// The two reaches of the judge labels (§C3-2-D05-6).
const (
	GoldReachFull     = "voll"
	GoldReachCoreOnly = "nur-kern"
)

// CalibrationThresholds are the stated rules of the run. Every one of them is a
// visible lead setting without a derivation — there is no prior measurement any
// of them could follow from, which is the same reason the kappa threshold has
// no default in the command layer. They are overridable through DECISIONS.
type CalibrationThresholds struct {
	KappaMin   float64
	RhoMin     float64
	RhoCILoMin float64
	PiMin      float64
	PiCILoMin  float64
	UnsureMax  float64
}

// DefaultCalibrationThresholds are the numbers §C3-2-D05-5 and -6 name.
func DefaultCalibrationThresholds() CalibrationThresholds {
	return CalibrationThresholds{
		KappaMin: math.NaN(), RhoMin: 0.80, RhoCILoMin: 0.70,
		PiMin: 0.70, PiCILoMin: 0.60, UnsureMax: 0.10,
	}
}

// CalibratedGateReport is the C3-4a Kipp-Report.
//
// Seven conditions, all fail-closed and all able to move a gate only towards
// "nicht entschieden" — never towards "trägt". The kappa condition is the one
// exception and it is deliberate: it does not decide the gate at all any more,
// it restricts the REACH of the judge labels, which is recorded in GoldReach
// and in the notes rather than in the verdict.
func CalibratedGateReport(res map[string]CalibrationResult, flips map[string]MetricFlip,
	th CalibrationThresholds,
) []GateVerdict {
	out := make([]GateVerdict, 0, len(judgeGates))
	for _, g := range judgeGates {
		v := GateVerdict{
			Name: g.Name, Slices: g.Slices, Decides: g.Decides,
			Verdict: GateCarries, GoldReach: GoldReachFull, Kappa: map[string]KappaResult{},
		}
		for _, s := range g.Slices {
			r, ok := res[s]
			v.Kappa[s] = r.Unweighted
			if !ok || r.Pairs == 0 {
				v.Reasons = append(v.Reasons,
					fmt.Sprintf("%s: keine kalibrierte Zelle — κ nicht berechenbar", s))
				v.GoldReach = GoldReachCoreOnly
				continue
			}
			appendKappaReach(&v, s, r, th)
			v.Reasons = append(v.Reasons, marginReasons(s, r)...)
			v.Reasons = append(v.Reasons, unsureReasons(s, r, th)...)
			v.Reasons = append(v.Reasons, ratioReasons(s, r, th)...)
			v.Reasons = append(v.Reasons, flipReasons(s, flips[s])...)
		}
		if len(v.Reasons) > 0 {
			v.Verdict = GateUndecided
		}
		out = append(out, v)
	}
	return out
}

// appendKappaReach applies condition 3: below the threshold the judge labels
// stop standing in for gold outside the core.
func appendKappaReach(v *GateVerdict, slice string, r CalibrationResult, th CalibrationThresholds) {
	switch {
	case r.Weighted.NotComputable:
		v.GoldReach = GoldReachCoreOnly
		v.Notes = append(v.Notes, fmt.Sprintf(
			"%s: κ_w nicht berechenbar (erwartete Übereinstimmung = 1 bei n=%d) — "+
				"Judge-Labels tragen außerhalb des Kerns nicht", slice, r.Weighted.N))
	case math.IsNaN(th.KappaMin):
	case r.Weighted.Kappa < th.KappaMin:
		v.GoldReach = GoldReachCoreOnly
		v.Notes = append(v.Notes, fmt.Sprintf(
			"%s: κ_w=%.4f unter der vorab genannten Schranke %.4f (n=%d, ungewichtet %.4f) — "+
				"die Gate-Rechnung läuft nur auf den Kern-Queries, mit entsprechend größerer MDE",
			slice, r.Weighted.Kappa, th.KappaMin, r.Weighted.N, r.Unweighted.Kappa))
	default:
		v.Notes = append(v.Notes, fmt.Sprintf(
			"%s: κ_w=%.4f ≥ %.4f (ungewichtet %.4f) — Judge-Labels tragen als Gold-Ersatz",
			slice, r.Weighted.Kappa, th.KappaMin, r.Unweighted.Kappa))
	}
}

// marginReasons applies condition 4: the marginal shift between the two judges.
func marginReasons(slice string, r CalibrationResult) []string {
	if r.Weighted.MarginalP >= marginalAlpha {
		return nil
	}
	return []string{fmt.Sprintf(
		"%s: Kipp der Randverteilungen — die beiden Urteiler nennen verschiedene Anteile relevant "+
			"(McNemar exakt p=%.5f < %.2f; %d nur maschinell, %d nur Fable)",
		slice, r.Weighted.MarginalP, marginalAlpha, r.Unweighted.LLMOnly, r.Unweighted.ControlOnly)}
}

// unsureReasons applies condition 7: the `?`-rate per stratum.
func unsureReasons(slice string, r CalibrationResult, th CalibrationThresholds) []string {
	var out []string
	for _, s := range r.Strata {
		if s.UnsureRate > th.UnsureMax {
			out = append(out, fmt.Sprintf(
				"%s: `?`-Rate %.4f in Schicht %s über der Schwelle %.2f (%d von %d) — "+
					"der Lauf hält an; Rubrik oder Auszugslänge ist das Problem, nicht das Urteil",
				slice, s.UnsureRate, s.Stratum, th.UnsureMax, s.Unsure, s.N))
		}
	}
	return out
}

// ratioReasons applies condition 6: sensitivity and precision of the judge.
func ratioReasons(slice string, r CalibrationResult, th CalibrationThresholds) []string {
	var out []string
	check := func(name string, e RatioEstimate, min, ciMin float64, why string) {
		if !e.Defined {
			out = append(out, fmt.Sprintf("%s: %s nicht berechenbar (Nenner 0 bei n=%d)", slice, name, e.N))
			return
		}
		if e.Value < min {
			out = append(out, fmt.Sprintf("%s: %s=%.4f unter der vorab genannten Schranke %.4f — %s",
				slice, name, e.Value, min, why))
		}
		if e.CILo < ciMin {
			out = append(out, fmt.Sprintf("%s: %s-CI-Untergrenze %.4f unter %.4f ([%.4f, %.4f])",
				slice, name, e.CILo, ciMin, e.CILo, e.CIHi))
		}
	}
	check("ρ", r.Rho, th.RhoMin, th.RhoCILoMin,
		"fehlendes Gold verkleinert den Recall@5-Nenner und verzerrt ihn nach oben")
	check("π", r.Pi, th.PiMin, th.PiCILoMin, "Falsch-Gold hebt nDCG@10")
	return out
}

// flipReasons applies condition 5, the new primary authority.
//
// An ABSENT flip computation is a reason, not a pass. Condition 5 is the gate's
// primary authority under §C3-2-D05-6, and a gate that carried because nobody
// ran the check would be exactly the self-confirmation this tool exists to
// prevent — the same fail-closed direction as condition 1.
func flipReasons(slice string, f MetricFlip) []string {
	if !f.Available {
		return []string{fmt.Sprintf(
			"%s: keine Kipp-Rechnung auf dem Kern vorgelegt — die Primär-Autorität "+
				"nach §C3-2-D05-6 (Fable-Gold gegen Judge-Gold) fehlt", slice)}
	}
	if !f.Flipped() {
		return nil
	}
	what := "gepaartes 95-%-CI der Differenz schließt 0 aus"
	if f.SignFlip() {
		what = "Vorzeichenwechsel"
	}
	return []string{fmt.Sprintf(
		"%s: Metrik-Kipp auf dem Kern — %s (%s: Fable-Gold %+.5f, Judge-Gold %+.5f, "+
			"CI der Differenz [%+.5f, %+.5f], n=%d)",
		slice, what, f.Metric, f.DeltaFable, f.DeltaJudge, f.DiffCILo, f.DiffCIHi, f.N)}
}

// RenderCalibrationReport is the human form of the C3-4a Kipp-Report.
func RenderCalibrationReport(th CalibrationThresholds, res map[string]CalibrationResult,
	flips map[string]MetricFlip, gates []GateVerdict,
) string {
	var b strings.Builder
	b.WriteString("# Kalibrierung C3-4a — κ, κ_w, ρ, π und Kipp-Report\n\n")
	fmt.Fprintf(&b, "Urteiler: %s.\n", FableJudge)
	fmt.Fprintf(&b, "Vorab genannte Schranken: κ_w ≥ %.4f · ρ ≥ %.4f (CI-Untergrenze ≥ %.4f) · "+
		"π ≥ %.4f (CI-Untergrenze ≥ %.4f) · `?`-Rate ≤ %.2f je Schicht. "+
		"Kipp-Prüfung: exakter McNemar-Test, α = %.2f.\n\n",
		th.KappaMin, th.RhoMin, th.RhoCILoMin, th.PiMin, th.PiCILoMin, th.UnsureMax, marginalAlpha)
	names := make([]string, 0, len(res))
	for s := range res {
		names = append(names, s)
	}
	sort.Strings(names)
	b.WriteString("## Kalibrier-Stichprobe je Slice\n\n")
	b.WriteString("| Slice | n | κ ungewichtet | κ_w | McNemar p | ρ | ρ-CI | π | π-CI |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: | ---: | :--- | ---: | :--- |\n")
	for _, s := range names {
		r := res[s]
		fmt.Fprintf(&b, "| %s | %d | %s | %s | %.5f | %s | [%.4f, %.4f] | %s | [%.4f, %.4f] |\n",
			s, r.Pairs, kappaCell(r.Unweighted.Kappa, r.Unweighted.NotComputable),
			kappaCell(r.Weighted.Kappa, r.Weighted.NotComputable), r.Weighted.MarginalP,
			ratioCell(r.Rho), r.Rho.CILo, r.Rho.CIHi, ratioCell(r.Pi), r.Pi.CILo, r.Pi.CIHi)
	}
	for _, s := range names {
		writeStrata(&b, s, res[s])
	}
	b.WriteString("\n## Metrik-Kipp auf dem Gold-Kern\n\n")
	if len(flips) == 0 {
		b.WriteString("Keine Kipp-Rechnung vorgelegt — die Gate-Autorität nach §C3-2-D05-6 fehlt.\n")
	}
	for _, s := range names {
		f, ok := flips[s]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "- %s · %s: Fable-Gold %+.5f, Judge-Gold %+.5f, CI der Differenz "+
			"[%+.5f, %+.5f] (n=%d) ⇒ %s\n", s, f.Metric, f.DeltaFable, f.DeltaJudge,
			f.DiffCILo, f.DiffCIHi, f.N, flipWord(f))
	}
	b.WriteString("\n## Kipp-Report je Gate\n\n")
	for _, g := range gates {
		fmt.Fprintf(&b, "### %s — %s\n\n", g.Name, g.Verdict)
		fmt.Fprintf(&b, "Entscheidet: %s\nRuht auf: %s\nGold-Reichweite: %s\n",
			g.Decides, strings.Join(g.Slices, ", "), g.GoldReach)
		for _, n := range g.Notes {
			fmt.Fprintf(&b, "- (Reichweite) %s\n", n)
		}
		for _, r := range g.Reasons {
			fmt.Fprintf(&b, "- %s\n", r)
		}
		b.WriteString("\n")
	}
	b.WriteString("Ein Gate mit dem Vermerk „" + GateUndecided + "“ ist weder als erreicht noch als\n" +
		"verfehlt ausgewiesen. Die Gold-Reichweite „" + GoldReachCoreOnly + "“ heißt: die Gate-Rechnung\n" +
		"läuft ausschließlich auf den Kern-Queries, weil die Judge-Labels außerhalb des Kerns nicht als\n" +
		"Gold-Ersatz tragen (§C3-2-D05-6). Die Rausch-Kontrollen der Schicht S0 gehen NICHT in κ ein;\n" +
		"sie speisen ausschließlich die Kontroll-Trefferquote.\n")
	return b.String()
}

func writeStrata(b *strings.Builder, slice string, r CalibrationResult) {
	fmt.Fprintf(b, "\n### Schichten %s\n\n", slice)
	b.WriteString("| Schicht | n | N (Population) | Gewicht | Judge relevant | Fable relevant | beide | `?` | `?`-Rate |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	rows := append([]StratumStats(nil), r.Strata...)
	if r.Core.N > 0 {
		rows = append([]StratumStats{r.Core}, rows...)
	}
	for _, s := range rows {
		fmt.Fprintf(b, "| %s | %d | %d | %.4f | %d | %d | %d | %d | %.4f |\n",
			s.Stratum, s.N, s.Population, s.Weight, s.LLMRelevant, s.FableRelevant, s.Both, s.Unsure, s.UnsureRate)
	}
	if r.ControlN > 0 {
		fmt.Fprintf(b, "\nKontroll-Trefferquote (S0, außerhalb von κ): **%.4f** (%d von %d).\n",
			r.ControlRate, r.ControlHits, r.ControlN)
	}
}

func kappaCell(v float64, notComputable bool) string {
	if notComputable {
		return "n/a"
	}
	return fmt.Sprintf("%.4f", v)
}

func ratioCell(e RatioEstimate) string {
	if !e.Defined {
		return "n/a"
	}
	return fmt.Sprintf("%.4f", e.Value)
}

func flipWord(f MetricFlip) string {
	if f.Flipped() {
		return "Kipp"
	}
	return "kein Kipp"
}
