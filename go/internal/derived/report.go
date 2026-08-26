package derived

import (
	"fmt"
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
	r.Passed, r.Reason = judge(r)
	return r
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
// an even count, and 0 on an empty set. It sorts a COPY: a fold that reorders
// its caller's slice would make the report's determinism depend on how often
// it was called.
func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := make([]float64, len(xs))
	copy(sorted, xs)
	sort.Float64s(sorted)

	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
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
