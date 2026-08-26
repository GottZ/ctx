package derived

import "github.com/GottZ/ctx/internal/evalscore"

// Adequacy measures whether a claim is MORE than a copy of its quote:
//
//	compression = |TokenSet(claim)| / |TokenSet(quote)|
//	novelty     = |TokenSet(claim) \ TokenSet(quote)| / |TokenSet(claim)|
//
// This is the counter-metric to the anchoring rate, and it exists because the
// anchoring rate alone is trivially maximised (§4.4): a generator that quotes
// a whole source paragraph and asserts it as an "insight" reaches rate 1,0
// with value 0. G0–G7 cannot catch that — every one of those lines is a
// perfectly anchored citation. Report folds both numbers into the SAME verdict
// for that reason; a footnote nobody reads would leave the gate gameable.
//
// The tokeniser is evalscore.TokenSet (evalscore/evalscore.go:63), the one the
// eval side already scores title overlap with: lower-cased runs of
// [a-z0-9äöüß]. Reusing it keeps a claim's novelty here and its token F1 there
// on the same word boundaries — two tokenisers would drift, and the Goodhart
// threshold is calibrated against measured values, not against a definition.
// Its consequences are deliberate: punctuation, markup and CJK text carry no
// tokens, and a set (not a bag) means a claim that repeats a word ten times
// counts it once.
//
// Both values are SET ratios, so compression can exceed 1 — a claim with more
// distinct tokens than its short quote is expanding, not compressing, and the
// number should say so rather than clamp.
//
// EMPTY SETS ARE 0, NEVER NaN. An empty denominator would produce NaN, and a
// NaN novelty compares false against every threshold — the Goodhart rule would
// go silent on exactly the degenerate charges it should flag. So: an empty
// QUOTE token set gives compression 0, and an empty CLAIM token set gives
// novelty 0. A claim with tokens over an empty quote keeps novelty 1: every
// token of it is unsupported, which is the truthful reading and not a
// division artefact. In the gate path an empty quote cannot occur anyway —
// G2 (MinQuoteRunes) rejects it first.
func Adequacy(claim, quote string) (compression, novelty float64) {
	claimSet := evalscore.TokenSet(claim)
	quoteSet := evalscore.TokenSet(quote)

	if len(quoteSet) > 0 {
		compression = float64(len(claimSet)) / float64(len(quoteSet))
	}
	if len(claimSet) > 0 {
		unsupported := 0
		for token := range claimSet {
			if !quoteSet[token] {
				unsupported++
			}
		}
		novelty = float64(unsupported) / float64(len(claimSet))
	}
	return compression, novelty
}
