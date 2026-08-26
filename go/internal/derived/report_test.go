package derived

import (
	"encoding/json"
	"strings"
	"testing"
)

// honestQuote is the quote side of the honest charge: eight distinct tokens.
const honestQuote = "alpha beta gamma delta epsilon zeta eta theta"

// honestClaimText names three of those tokens and adds two of its own, so its
// novelty is exactly 2/5 = 0,4 — above GoodhartMinNovelty by a clear margin.
const honestClaimText = "alpha beta gamma novum secundum"

// copyClaim is a claim that IS its quote: the Goodhart failure mode in one
// line. The quote clears MinQuoteRunes so the charge is one a real gate could
// have produced.
func copyClaim() Claim {
	return Claim{
		Claim:    realClaimSource,
		Quote:    realClaimSource,
		SourceID: "00000000-0000-0000-0000-000000000001",
		Kind:     "finding",
	}
}

// honestClaim is a claim that reformulates its quote.
func honestClaim() Claim {
	return Claim{
		Claim:    honestClaimText,
		Quote:    honestQuote,
		SourceID: "00000000-0000-0000-0000-000000000002",
		Kind:     "finding",
	}
}

// keptItem builds one block of a charge in which every offered claim survived
// the gate — offered and kept are the same slice, rejects are the zeroed
// eight buckets CiteGate always returns.
func keptItem(claims ...Claim) Item {
	return Item{Claims: claims, Verdict: Verdict{Kept: claims, Rejects: newRejects()}}
}

// mixedItem builds one block in which the first keep claims survived and the
// rest were discarded by the named gate.
func mixedItem(claims []Claim, keep int, gate string) Item {
	v := Verdict{Kept: claims[:keep], Rejects: newRejects()}
	v.Rejects[gate] = len(claims) - keep
	return Item{Claims: claims, Verdict: v}
}

// repeat returns n copies of one claim.
func repeat(c Claim, n int) []Claim {
	out := make([]Claim, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, c)
	}
	return out
}

// copyCharge is the Goodhart charge of §7 M-W4: twenty claims across four
// blocks, every one of them anchored and every one of them a verbatim copy.
func copyCharge() []Item {
	items := make([]Item, 0, 4)
	for i := 0; i < 4; i++ {
		items = append(items, keptItem(repeat(copyClaim(), 5)...))
	}
	return items
}

// passedWithoutNoveltyClause is the Goodhart rule MINUS its novelty clause —
// the executable form of the negative probe §7 M-W4 asks for. Removing the
// clause from the real judge (or setting GoodhartMinNovelty to 0) turns the
// copy charge green; this function shows that outcome without mutating the
// source, and the test below asserts the real Report disagrees with it.
func passedWithoutNoveltyClause(r GateReport) bool {
	return r.ClaimsOffered > 0 && r.GroundingRate >= GoodhartMinGrounding
}

// TestGoodhartRuleRefusesTheCopyCharge is gate 3 of the wave: a charge with a
// perfect anchoring rate that consists entirely of copies must NOT pass.
//
// A pure anchoring rate is trivially maximised — the generator quotes the
// whole source paragraph and asserts it as an insight. Rate 1,0, value 0.
func TestGoodhartRuleRefusesTheCopyCharge(t *testing.T) {
	r := Report(copyCharge())

	if r.ClaimsOffered != 20 || r.ClaimsKept != 20 {
		t.Fatalf("charge is %d/%d, want 20/20", r.ClaimsKept, r.ClaimsOffered)
	}
	if !closeTo(r.GroundingRate, 1) {
		t.Fatalf("grounding rate = %v, want 1", r.GroundingRate)
	}
	if !closeTo(r.MedianNovelty, 0) {
		t.Fatalf("median novelty = %v, want 0", r.MedianNovelty)
	}
	if r.Passed {
		t.Fatalf("charge PASSED: %s", r.Reason)
	}
	if !strings.Contains(r.Reason, "kopiert statt extrahiert") {
		t.Fatalf("reason = %q, want the copy-instead-of-extraction reason", r.Reason)
	}
}

// TestGoodhartRuleWithoutTheNoveltyClauseWouldPass is the negative probe of
// gate 3: the same charge, judged by the anchoring rate alone, is green. The
// clause is therefore load-bearing and not decoration.
func TestGoodhartRuleWithoutTheNoveltyClauseWouldPass(t *testing.T) {
	r := Report(copyCharge())
	if !passedWithoutNoveltyClause(r) {
		t.Fatal("fixture error: the copy charge does not even reach the anchoring threshold")
	}
	if r.Passed {
		t.Fatal("Report agrees with the clause-less variant — the novelty clause is not wired")
	}
}

// TestReportPassesTheHonestCharge is gate 4: anchoring rate 0,96 at median
// novelty 0,4 is the shape the gate exists to let through.
func TestReportPassesTheHonestCharge(t *testing.T) {
	r := Report([]Item{mixedItem(repeat(honestClaim(), 25), 24, "g3")})

	if !closeTo(r.GroundingRate, 0.96) {
		t.Fatalf("grounding rate = %v, want 0.96 (24/25)", r.GroundingRate)
	}
	if !closeTo(r.MedianNovelty, 0.4) {
		t.Fatalf("median novelty = %v, want 0.4", r.MedianNovelty)
	}
	if !r.Passed {
		t.Fatalf("charge did NOT pass: %s", r.Reason)
	}
	if r.Rejects["g3"] != 1 {
		t.Fatalf("rejects = %v, want g3=1", r.Rejects)
	}
	if !closeTo(r.RejectionRate, 0.04) {
		t.Fatalf("rejection rate = %v, want 0.04", r.RejectionRate)
	}
}

// TestLowGroundingHasItsOwnReason is the second half of gate 4: a charge that
// fails at rate 0,5 fails for a DIFFERENT reason than the copy charge. Folding
// both into one verdict text would hide which of the two failure modes a run
// actually hit — and they call for opposite fixes.
func TestLowGroundingHasItsOwnReason(t *testing.T) {
	r := Report([]Item{mixedItem(repeat(honestClaim(), 20), 10, "g3")})

	if !closeTo(r.GroundingRate, 0.5) {
		t.Fatalf("grounding rate = %v, want 0.5", r.GroundingRate)
	}
	if r.Passed {
		t.Fatalf("charge PASSED at rate 0.5: %s", r.Reason)
	}
	if strings.Contains(r.Reason, "kopiert statt extrahiert") {
		t.Fatalf("reason = %q, want low anchoring, not the Goodhart reason", r.Reason)
	}
	if !strings.Contains(r.Reason, "niedrige Verankerung") {
		t.Fatalf("reason = %q, want the low-anchoring reason", r.Reason)
	}
}

// TestEmptyChargeIsNotAPass pins the degenerate case: nothing offered is
// nothing measured, and "nothing measured" is not a pass.
func TestEmptyChargeIsNotAPass(t *testing.T) {
	r := Report(nil)
	if r.Passed {
		t.Fatalf("empty charge PASSED: %s", r.Reason)
	}
	if r.Blocks != 0 || r.ClaimsOffered != 0 {
		t.Fatalf("blocks=%d offered=%d, want 0/0", r.Blocks, r.ClaimsOffered)
	}
	if len(r.Rejects) != len(GateKeys) {
		t.Fatalf("rejects = %v, want the eight zeroed buckets", r.Rejects)
	}
}

// TestReportStatesTheRejectionRateDoubleRole is gate 5 of the wave.
//
// When the gate writes fail-closed, the runtime hallucination rate is 0 BY
// CONSTRUCTION. What the report measures is the REJECTION rate — how much of
// what the generator produced is not anchorable. Without that sentence a
// reader takes the zero for quality instead of for construction, and the
// number becomes a claim the gate never made.
func TestReportStatesTheRejectionRateDoubleRole(t *testing.T) {
	r := Report(copyCharge())

	if r.Measured != MeasuredQuantity {
		t.Fatalf("measured = %q, want %q", r.Measured, MeasuredQuantity)
	}
	if r.Measured != "rejection_rate" {
		t.Fatalf("measured = %q, want the wire value rejection_rate", r.Measured)
	}

	var wire map[string]any
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if wire["measured"] != "rejection_rate" {
		t.Fatalf("wire measured = %v, want rejection_rate", wire["measured"])
	}

	const core = "Halluzinationsrate ist 0 by construction (fail-closed Gate); " +
		"gemessen wird die Verwurfsrate"
	if !strings.Contains(DualRoleNote, core) {
		t.Fatalf("DualRoleNote = %q, want it to carry the §4.4 sentence", DualRoleNote)
	}
	if !strings.Contains(r.String(), core) {
		t.Fatalf("rendering does not state the double role:\n%s", r.String())
	}
}

// TestReportIsDeterministic is gate 6: the same charge rendered twice is the
// same bytes. Report folds over maps (the reject buckets) and sorts floats for
// its medians — both are places where an implementation can be correct on
// average and unstable per run, which would make every A/B diff unreadable.
func TestReportIsDeterministic(t *testing.T) {
	items := append(copyCharge(), mixedItem(repeat(honestClaim(), 25), 24, "g6"))

	first, err := json.Marshal(Report(items))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for i := 0; i < 8; i++ {
		again, err := json.Marshal(Report(items))
		if err != nil {
			t.Fatalf("marshal %d: %v", i, err)
		}
		if string(again) != string(first) {
			t.Fatalf("run %d differs:\n%s\n%s", i, first, again)
		}
	}
	if a, b := Report(items).String(), Report(items).String(); a != b {
		t.Fatalf("rendering differs:\n%s\n%s", a, b)
	}
}

// TestReportRejectsCarryTheEightBuckets mirrors the CiteGate contract: the
// histogram is written verbatim into a report, where a missing key and a zero
// must not be distinguishable.
func TestReportRejectsCarryTheEightBuckets(t *testing.T) {
	r := Report([]Item{mixedItem(repeat(honestClaim(), 4), 2, "g0"), mixedItem(repeat(honestClaim(), 4), 3, "g0")})

	for _, k := range GateKeys {
		if _, ok := r.Rejects[k]; !ok {
			t.Fatalf("bucket %s missing: %v", k, r.Rejects)
		}
	}
	if len(r.Rejects) != len(GateKeys) {
		t.Fatalf("rejects = %v, want exactly the eight buckets", r.Rejects)
	}
	if r.Rejects["g0"] != 3 {
		t.Fatalf("g0 = %d, want 3 (2 + 1 summed over the charge)", r.Rejects["g0"])
	}
	if r.Blocks != 2 || r.ClaimsOffered != 8 || r.ClaimsKept != 5 {
		t.Fatalf("blocks=%d offered=%d kept=%d, want 2/8/5", r.Blocks, r.ClaimsOffered, r.ClaimsKept)
	}
}

// TestReportMedianOverKeptClaimsOnly pins WHICH claims the medians describe:
// the ones that survived. A median over the offered set would let a rejected
// copy drag the novelty of a charge that never wrote it.
func TestReportMedianOverKeptClaimsOnly(t *testing.T) {
	claims := append(repeat(honestClaim(), 3), repeat(copyClaim(), 3)...)
	r := Report([]Item{mixedItem(claims, 3, "g3")})

	if !closeTo(r.MedianNovelty, 0.4) {
		t.Fatalf("median novelty = %v, want 0.4 — the rejected copies must not count", r.MedianNovelty)
	}
	if !closeTo(r.MedianCompression, 5.0/8.0) {
		t.Fatalf("median compression = %v, want 5/8", r.MedianCompression)
	}
}
