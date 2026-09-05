package derived

import (
	"math"
	"testing"

	"github.com/GottZ/ctx/internal/util"
)

// tokenQuote20 carries exactly twenty distinct scoring tokens of
// util.TokenSet, so compression against a four-token claim is a number
// the test can name instead of approximate.
const tokenQuote20 = "alpha beta gamma delta epsilon zeta eta theta iota kappa " +
	"lambda mu nu xi omicron pi rho sigma tau upsilon"

// tokenClaim4 carries exactly four distinct tokens, all of them inside
// tokenQuote20.
const tokenClaim4 = "alpha beta gamma delta"

// realClaimSource is the quote side of golden case (d): twelve distinct
// tokens.
const realClaimSource = "Der Embed-Backfill überspringt excluded-Typen und lässt " +
	"ihre Vektoren dauerhaft leer"

// realClaimReworded is the claim side of golden case (d): nine distinct
// tokens, three of which do not occur in realClaimSource.
const realClaimReworded = "Der Backfill lässt excluded Vektoren unberührt, verwaist und ungezählt"

// closeTo compares two floats at a tolerance far below every threshold this
// package decides on.
func closeTo(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

// wantTokens is a fixture guard: a golden case that silently changes its own
// token count would keep passing while measuring something else.
func wantTokens(t *testing.T, s string, n int) {
	t.Helper()
	if got := len(util.TokenSet(s)); got != n {
		t.Fatalf("fixture error: %q has %d tokens, the case is built on %d", s, got, n)
	}
}

// TestGoldenCaseC_VerbatimClaimHasZeroNovelty is golden case (c) of §7 M-W4:
// a claim that IS its quote adds nothing, and the Goodhart counter-metric has
// to say so with a zero.
//
// Red probe: an Adequacy that counts the claim tokens instead of the ones
// missing from the quote returns 1 here and this test fails.
func TestGoldenCaseC_VerbatimClaimHasZeroNovelty(t *testing.T) {
	compression, novelty := Adequacy(realClaimSource, realClaimSource)
	if !closeTo(novelty, 0) {
		t.Fatalf("novelty = %v, want 0 for a verbatim copy", novelty)
	}
	if !closeTo(compression, 1) {
		t.Fatalf("compression = %v, want 1 for a verbatim copy", compression)
	}
}

// TestGoldenCaseD_HalfRewordingIsBetweenZeroAndOne is golden case (d) of §7
// M-W4: a claim that reformulates part of its quote lands strictly inside
// (0,1) — three of nine claim tokens are new, so the value is exactly 1/3.
func TestGoldenCaseD_HalfRewordingIsBetweenZeroAndOne(t *testing.T) {
	wantTokens(t, realClaimSource, 12)
	wantTokens(t, realClaimReworded, 9)

	compression, novelty := Adequacy(realClaimReworded, realClaimSource)
	if !(novelty > 0 && novelty < 1) {
		t.Fatalf("novelty = %v, want strictly inside (0,1)", novelty)
	}
	if !closeTo(novelty, 3.0/9.0) {
		t.Fatalf("novelty = %v, want 3/9 (unberührt, verwaist, ungezählt are new)", novelty)
	}
	if !closeTo(compression, 9.0/12.0) {
		t.Fatalf("compression = %v, want 9/12", compression)
	}
}

// TestAdequacyCompressionIsClaimOverQuoteTokens is the §7 M-W4 addition: four
// claim tokens against twenty quote tokens are compression 0,2.
func TestAdequacyCompressionIsClaimOverQuoteTokens(t *testing.T) {
	wantTokens(t, tokenQuote20, 20)
	wantTokens(t, tokenClaim4, 4)

	compression, novelty := Adequacy(tokenClaim4, tokenQuote20)
	if !closeTo(compression, 0.2) {
		t.Fatalf("compression = %v, want 0.2 (4/20)", compression)
	}
	if !closeTo(novelty, 0) {
		t.Fatalf("novelty = %v, want 0 — every claim token is in the quote", novelty)
	}
}

// TestAdequacyEmptySetsAreZeroNotNaN pins the empty-set contract. A ratio over
// an empty denominator is NaN, and a NaN novelty would pass "< GoodhartMinNovelty"
// nowhere and fail every comparison silently — the Goodhart rule would go
// quiet exactly on the degenerate charges.
func TestAdequacyEmptySetsAreZeroNotNaN(t *testing.T) {
	cases := []struct {
		name            string
		claim, quote    string
		compress, novel float64
	}{
		{"both empty", "", "", 0, 0},
		{"empty claim", "", tokenQuote20, 0, 0},
		{"empty quote", tokenClaim4, "", 0, 1},
		{"punctuation only", "—— ...", "!!!", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compression, novelty := Adequacy(tc.claim, tc.quote)
			if math.IsNaN(compression) || math.IsNaN(novelty) {
				t.Fatalf("NaN: compression=%v novelty=%v", compression, novelty)
			}
			if !closeTo(compression, tc.compress) {
				t.Fatalf("compression = %v, want %v", compression, tc.compress)
			}
			if !closeTo(novelty, tc.novel) {
				t.Fatalf("novelty = %v, want %v", novelty, tc.novel)
			}
		})
	}
}
