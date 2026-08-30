package derived

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// GoodhartMinGrounding and GoodhartMinNovelty are the two thresholds of the
// Goodhart rule, fixed in advance (§4.8, §4.9): a charge with an anchoring
// rate of at least GoodhartMinGrounding AND a median novelty below
// GoodhartMinNovelty is NOT passed — it copies instead of extracting.
//
// They are CONSTANTS, not config keys, for the same reason MinQuoteRunes and
// MinKeepRatio are (§4.4.1): an operator who sets the novelty floor to 0 has
// switched the counter-metric off without anything turning red anywhere, and a
// gate whose threshold is a settings write is not a gate (W19). The values are
// a POLICY and are named as one — 0,15 says "at least one claim token in seven
// has to be the model's own wording", and §4.8 puts the target at 0,30 with
// 0,15 as the refusal floor.
//
// THE KEY distill.novelty_floor IS NOT A COUNTER-EXAMPLE TO THAT (wave C5-E).
// It configures the ARM's write-path screen, whose default IS
// GoodhartMinNovelty, and its 0 does not soften this verdict — it only returns
// the arm to writing what it wrote before the floor existed. The number a run
// is JUDGED by stays here, uneditable, whatever the arm was configured to
// discard.
const (
	GoodhartMinGrounding = 0.95
	GoodhartMinNovelty   = 0.15
)

// MeasuredQuantity is the wire value of GateReport.Measured, and it is a
// constant because the whole point of the field is that it can never say
// anything else. See DualRoleNote.
const MeasuredQuantity = "rejection_rate"

// DualRoleNote is the mandatory line of every gate report (§4.4, last
// paragraph).
//
// The gate has two roles: a test instrument for the measurement waves, and the
// write-time gate D-02/D-03 install. In the second role it writes fail-closed,
// which means the runtime hallucination rate is 0 BY CONSTRUCTION — not
// because the generator is good. What is actually measured is the REJECTION
// rate: how much of what the generator produced is not anchorable. That number
// is a generator-quality metric AND a cost metric (discarded GPU seconds).
// Without this sentence in the report somebody reads a 0 as quality instead of
// as construction, so it is rendered unconditionally and not left to callers.
const DualRoleNote = "Halluzinationsrate ist 0 by construction (fail-closed Gate); " +
	"gemessen wird die Verwurfsrate."

// Item is one block of a charge: what the arm OFFERED for it, and what
// CiteGate returned.
//
// CALLER CONTRACT: Claims is the slice that was handed to CiteGate, complete —
// kept plus rejected. Report derives the offered count from it and does NOT
// reconstruct it from the reject buckets, because a caller that prefilters in
// a map stage rejects lines the binding gate never saw (§4.4.2), and those
// must not silently inflate the anchoring rate of the run that wrote the
// block. A caller that passes only the survivors as Claims therefore reports a
// rate of 1,0 and measures nothing — the one place where this report trusts
// its caller, stated rather than hidden.
type Item struct {
	Claims  []Claim
	Verdict Verdict
}

// GateReport folds one charge — the blocks of ONE run — to the gate metrics.
//
// It carries the anchoring rate and its complement the rejection rate, the
// summed reject histogram, and the two Adequacy medians over the claims that
// SURVIVED. The medians are deliberately over the survivors: a rejected copy
// never became a block, and letting it drag the novelty of the run would judge
// the charge on text nobody can read.
//
// Median, not mean: a single 300-token quote with a four-token claim would
// pull a mean compression far enough to hide a charge of copies, and the
// distribution of both values across a real charge is not symmetric.
type GateReport struct {
	// Blocks is the number of items in the charge.
	Blocks int `json:"blocks"`

	// ClaimsOffered and ClaimsKept are summed over the charge.
	ClaimsOffered int `json:"claims_offered"`
	ClaimsKept    int `json:"claims_kept"`

	// GroundingRate is ClaimsKept/ClaimsOffered, RejectionRate its
	// complement. Both are 0 on an empty charge — see Reason, which says so
	// in words rather than leaving a 0 to be read as a measurement.
	GroundingRate float64 `json:"grounding_rate"`
	RejectionRate float64 `json:"rejection_rate"`

	// Rejects is the summed histogram. Like Verdict.Rejects it always
	// carries exactly the eight keys g0…g7, zeros included: a missing key
	// and a zero must not be distinguishable.
	Rejects map[string]int `json:"rejects"`

	// MedianNovelty and MedianCompression are the Adequacy medians over the
	// kept claims of the whole charge.
	MedianNovelty     float64 `json:"median_novelty"`
	MedianCompression float64 `json:"median_compression"`

	// THE NOVELTY DISTRIBUTION (wave C5-A, entscheid C5-2). The median above is
	// one number about a set, and wave C4-R measured what it cannot see: run 2
	// wrote 7 claims at novelty 0 and 18,8 % below GoodhartMinNovelty while the
	// median stood at 0,4286 — comfortably above the 0,30 the wave had fixed as
	// its criterion in advance. The median was not wrong; it was silent about
	// exactly the tail that carries the cost, and the report was the instrument
	// that had to be re-read by hand out of the raw answers to see it.
	//
	// So the shape of the set travels WITH the set's middle. The five values
	// below are what entscheid C5-2 makes the criterion of the following
	// measurement waves — "p10 ≥ 0,15 UND Anteil novelty 0 ≤ 1 %" — and they are
	// here so that criterion can be read off the instrument instead of
	// recomputed beside it.
	//
	// THEY ARE OUTPUT AND NOT A GATE — in THIS package, and that has not
	// changed: judge() is untouched, and the five values below are computed and
	// reported, never enforced here.
	//
	// THE ARM'S WRITE PATH NOW HAS ONE (wave C5-E, migration 151). The diagnosis
	// this wave waited for was made: C5-A-M measured p10 = 0,0385, 27,1 % of
	// published claims below GoodhartMinNovelty and 5,85 % at exactly 0 on the
	// root stand, and the E-6 full backfill would have written that tail into
	// the corpus once and for good. The distiller therefore screens each claim
	// against distill.novelty_floor (default GoodhartMinNovelty, 0 = off) after
	// its seven evidence gates and books the discard in distill_run.rej_novelty.
	//
	// WHICH MOVES WHAT THESE FIVE NUMBERS DESCRIBE, and it is stated here rather
	// than left to be discovered: they are computed over the claims a charge
	// KEPT (see the type comment), so for a run of the armed arm they describe
	// the distribution AFTER the floor — the C5-2 wave criterion ("p10 ≥ 0,15
	// UND Anteil novelty 0 ≤ 1 %") is from that wave on a statement about the
	// published layer, not about what the generator offered. The BEFORE-picture
	// did not disappear with it: rej_novelty counts, per run, exactly how many
	// claims the floor removed from this distribution, so the pre-gate lage
	// stays readable next to the post-gate one instead of being replaced by it.
	// A report over an UNARMED run (floor 0, and every measurement up to and
	// including C5-A-M) is unchanged in meaning.
	//
	// NoveltyN is the size of the set all five describe. It equals ClaimsKept by
	// construction — the medians and quantiles are over the SURVIVORS (see the
	// type comment) — and it is a field anyway, because a distribution reported
	// without its n is a shape without a scale, and the two drifting apart is
	// exactly the kind of wiring fault a reader should see rather than assume
	// away.
	NoveltyN   int     `json:"novelty_n"`
	NoveltyP10 float64 `json:"novelty_p10"`
	NoveltyP25 float64 `json:"novelty_p25"`

	// NoveltyBelowFloorShare is the share of kept claims under
	// GoodhartMinNovelty, NoveltyZeroShare the share at exactly 0 — a claim
	// whose every token stands in its own quote, i.e. a copy that the anchoring
	// rate rewards. Both are 0 on an empty charge, like the rates above.
	NoveltyBelowFloorShare float64 `json:"novelty_below_floor_share"`
	NoveltyZeroShare       float64 `json:"novelty_zero_share"`

	// Measured is always MeasuredQuantity. It is a field and not a comment
	// because a consumer that reads this JSON has to be able to see, in the
	// data, which quantity the numbers are.
	Measured string `json:"measured"`

	// Passed is the Goodhart verdict, Reason its one-line justification.
	Passed bool   `json:"passed"`
	Reason string `json:"reason"`
}

// Report folds a charge to its GateReport. It is pure and deterministic: the
// same items produce byte-identical JSON and byte-identical rendering, which
// is what makes two runs of a measurement wave comparable at all.
func Report(items []Item) GateReport {
	r := GateReport{
		Blocks:   len(items),
		Rejects:  newRejects(),
		Measured: MeasuredQuantity,
	}
	var novelties, compressions []float64

	for _, it := range items {
		r.ClaimsOffered += len(it.Claims)
		r.ClaimsKept += len(it.Verdict.Kept)
		for _, key := range GateKeys {
			r.Rejects[key] += it.Verdict.Rejects[key]
		}
		for _, c := range it.Verdict.Kept {
			compression, novelty := Adequacy(c.Claim, c.Quote)
			compressions = append(compressions, compression)
			novelties = append(novelties, novelty)
		}
	}

	if r.ClaimsOffered > 0 {
		r.GroundingRate = float64(r.ClaimsKept) / float64(r.ClaimsOffered)
		r.RejectionRate = 1 - r.GroundingRate
	}
	r.MedianNovelty = median(novelties)
	r.MedianCompression = median(compressions)
	r.NoveltyN = len(novelties)
	r.NoveltyP10 = quantile(novelties, 0.10)
	r.NoveltyP25 = quantile(novelties, 0.25)
	// AGAINST THE CONSTANT, not the configured gate floor (review C5-E finding
	// 4): this report is a pure function without config access, so the share
	// counts against GoodhartMinNovelty — which equals the gate's DEFAULT.
	// Under a non-default distill.novelty_floor the two measure different
	// lines, and the gate's truth is the journal's rej_novelty counter, not
	// this display share.
	r.NoveltyBelowFloorShare = share(novelties, func(n float64) bool { return n < GoodhartMinNovelty })
	// EXACT EQUALITY, not a tolerance. Adequacy computes novelty as
	// unsupported/len(claimSet) and returns a literal 0 for the empty claim set,
	// so a value of 0 is arrived at exactly and never approached — a tolerance
	// here would fold the smallest genuine novelty in with the copies, which is
	// the one distinction this share exists to make.
	r.NoveltyZeroShare = share(novelties, func(n float64) bool { return n == 0 })
	r.Passed, r.Reason = judge(r)
	return r
}

// share is the fraction of xs satisfying pred, and 0 on an empty set — the same
// answer the rates above give, for the same reason: an empty charge measured
// nothing, and Reason says so in words.
func share(xs []float64, pred func(float64) bool) float64 {
	if len(xs) == 0 {
		return 0
	}
	n := 0
	for _, x := range xs {
		if pred(x) {
			n++
		}
	}
	return float64(n) / float64(len(xs))
}

// judge applies the Goodhart rule and names WHICH of the failure modes was
// hit. The two failures call for opposite fixes — a copying generator needs a
// different prompt, a low-anchoring one needs a different model or a different
// source set — so folding them into one verdict text would cost the report its
// only actionable half.
func judge(r GateReport) (bool, string) {
	switch {
	case r.ClaimsOffered == 0:
		return false, "leere Charge: keine Claims angeboten, es wurde nichts gemessen"
	case r.GroundingRate >= GoodhartMinGrounding && r.MedianNovelty < GoodhartMinNovelty:
		return false, fmt.Sprintf(
			"kopiert statt extrahiert: Verankerungs-Rate %.4f ≥ %.2f bei Median-novelty %.4f < %.2f",
			r.GroundingRate, GoodhartMinGrounding, r.MedianNovelty, GoodhartMinNovelty)
	case r.GroundingRate < GoodhartMinGrounding:
		return false, fmt.Sprintf(
			"niedrige Verankerung: Verankerungs-Rate %.4f < %.2f (Verwurfsrate %.4f)",
			r.GroundingRate, GoodhartMinGrounding, r.RejectionRate)
	default:
		return true, fmt.Sprintf(
			"Verankerungs-Rate %.4f ≥ %.2f bei Median-novelty %.4f ≥ %.2f",
			r.GroundingRate, GoodhartMinGrounding, r.MedianNovelty, GoodhartMinNovelty)
	}
}

// median returns the middle value of xs, the mean of the two middle values on
// an even count, and 0 on an empty set.
//
// It is quantile at q = 0,5 and NOT a second implementation, so the median and
// the quantiles of the same report cannot describe two different orderings of
// one set. That the delegation preserves the old behaviour exactly — including
// the mean of the two middles on an even count — is a property of the
// interpolation quantile uses, and TestMedianIsTheHalfQuantile pins it rather
// than leaving it to be re-derived from the formula.
func median(xs []float64) float64 { return quantile(xs, 0.5) }

// quantile returns the q-quantile of xs by LINEAR INTERPOLATION between the two
// neighbouring order statistics — h = (n-1)·q, then x[⌊h⌋] plus the fraction of
// the step to the next value. It sorts a COPY: a fold that reorders its
// caller's slice would make the report's determinism depend on how often it was
// called.
//
// WHY INTERPOLATION AND NOT NEAREST RANK. The report already publishes a median
// defined as the mean of the two middle values, and this is the definition that
// reproduces it for every n — a nearest-rank quantile would answer a different
// number at q = 0,5 than the field beside it, and a reader comparing p25 to the
// median would be comparing two conventions. It is also the definition of the
// tooling the measurement waves cross-check against (numpy's default, R's type
// 7), so a p10 read here and a p10 recomputed there are the same quantity.
//
// The value is CLAMPED to the sample, never extrapolated: on a small charge the
// p10 of ten claims is the smallest one, and saying so is the honest answer —
// inventing a value below the sample would put a number under the C5-2
// criterion that no claim ever had. 0 on an empty set, like every other value
// of an empty charge.
func quantile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := make([]float64, len(xs))
	copy(sorted, xs)
	sort.Float64s(sorted)

	if q <= 0 {
		return sorted[0]
	}
	if q >= 1 {
		return sorted[len(sorted)-1]
	}
	h := float64(len(sorted)-1) * q
	lo := int(math.Floor(h))
	if lo >= len(sorted)-1 {
		return sorted[len(sorted)-1]
	}
	frac := h - float64(lo)
	if frac == 0.5 {
		// The exact half-step is the plain mean. The general form below rounds
		// twice and differs from (a+b)/2 by one ULP on a measurable share of
		// neighbour pairs (review C5-A finding 2) — and the half-step is the
		// one fraction the median delegation above promises to reproduce
		// EXACTLY. The median of an even count always lands here: h is an odd
		// integer times 0.5, which IEEE-754 represents without error.
		return (sorted[lo] + sorted[lo+1]) / 2
	}
	return sorted[lo] + frac*(sorted[lo+1]-sorted[lo])
}

// String renders the report for a human reader — the eval log, a wave report,
// the console. Every number carries a fixed number of decimals so two runs
// diff cleanly, and the reject buckets are emitted in GateKeys order rather
// than in map order.
func (r GateReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Zitat-Gate-Report — %d Blöcke, %d/%d Claims verankert\n",
		r.Blocks, r.ClaimsKept, r.ClaimsOffered)
	fmt.Fprintf(&b, "Verankerungs-Rate %.4f · Verwurfsrate %.4f\n",
		r.GroundingRate, r.RejectionRate)
	fmt.Fprintf(&b, "Median-novelty %.4f · Median-compression %.4f\n",
		r.MedianNovelty, r.MedianCompression)
	// The distribution line (wave C5-A). It stands NEXT TO the median rather
	// than replacing it: the median is what the earlier waves were judged on,
	// and a report that silently swapped the quantity would make two runs
	// incomparable across the wave that changed it.
	fmt.Fprintf(&b, "novelty-Verteilung (n=%d) p10 %.4f · p25 %.4f · Median %.4f · "+
		"Anteil < %.2f: %.4f · Anteil = 0: %.4f\n",
		r.NoveltyN, r.NoveltyP10, r.NoveltyP25, r.MedianNovelty,
		GoodhartMinNovelty, r.NoveltyBelowFloorShare, r.NoveltyZeroShare)
	fmt.Fprintf(&b, "Verwürfe %s\n", rejectLine(r.Rejects))
	fmt.Fprintf(&b, "Gemessene Größe: %s — %s\n", r.Measured, DualRoleNote)
	fmt.Fprintf(&b, "Ergebnis: %s — %s\n", verdictWord(r.Passed), r.Reason)
	return b.String()
}

// rejectLine renders the histogram in gate order, zeros included.
func rejectLine(rejects map[string]int) string {
	parts := make([]string, 0, len(GateKeys))
	for _, key := range GateKeys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, rejects[key]))
	}
	return strings.Join(parts, " ")
}

// verdictWord spells the boolean out, because "false" next to a rate of 1,0
// is exactly the line a reader misreads.
func verdictWord(passed bool) string {
	if passed {
		return "bestanden"
	}
	return "NICHT bestanden"
}
