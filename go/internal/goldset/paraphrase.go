package goldset

import (
	"math/rand/v2"
	"regexp"
	"strings"
	"unicode"
)

// Title paraphrasing for G-KI is RULE-BASED, not model-based. Three reasons,
// all specific to this slice:
//
//  1. G-KI is declared a floor slice with a known trigram/title-FTS bias
//     (design 04 §4.5). A model paraphrase would not remove that bias — it
//     would only add an unpinnable provenance dependency to a slice whose whole
//     purpose is a cheap, reproducible boundary check.
//  2. Reproducibility: seed plus rules regenerate the slice byte-identically
//     forever. A pinned model does not survive an engine or template change.
//  3. The generator endpoint is live production serving. 300 sequential calls
//     buy no measurement resolution here; G-Q is where model-shaped language
//     actually matters, and that is where the calls go.
//
// The rules stay LIGHT on purpose — the slice must remain a known-item task.

var (
	// titleSeparator splits a title into segments. Only spaced dashes, pipes
	// and a colon followed by space count; an in-word hyphen (ctx-goldbench)
	// and a bare colon (12:30) must not split.
	titleSeparator = regexp.MustCompile(`\s+[—–|]\s+|:\s+|\s+-\s+`)
	// wsRun collapses arbitrary whitespace.
	wsRun = regexp.MustCompile(`\s+`)
)

// fillerTokens are low-information function words in the two corpus languages.
// Dropping exactly one is the mildest paraphrase that still moves the string.
var fillerTokens = map[string]bool{
	"der": true, "die": true, "das": true, "den": true, "dem": true, "des": true,
	"ein": true, "eine": true, "einer": true, "eines": true, "einem": true,
	"und": true, "oder": true, "für": true, "mit": true, "von": true, "zu": true,
	"im": true, "in": true, "am": true, "auf": true, "als": true, "bei": true,
	"the": true, "a": true, "an": true, "of": true, "for": true, "with": true,
	"and": true, "or": true, "to": true, "on": true, "at": true, "by": true,
}

// ParaphraseTitle turns a block title into a lightly paraphrased known-item
// query. Deterministic in (title, seed).
func ParaphraseTitle(title string, seed int64) string {
	base := normalizeTitle(title)
	if base == "" {
		return ""
	}
	//nolint:gosec // reproducibility, not unpredictability
	r := rand.New(rand.NewPCG(uint64(seed), hashSeed(title)))

	segments := splitSegments(base)
	cand := pickSegments(segments, r)
	tokens := strings.Fields(cand)
	tokens = dropOneFiller(tokens, r)
	tokens = lowerNonAcronyms(tokens)

	out := balanceBrackets(strings.Join(tokens, " "))
	if len(tokens) < 2 || strings.EqualFold(out, base) {
		// Fall back to the mildest transform that is still not the title
		// verbatim: case-folded full title. If even that equals the input the
		// title was already lowercase, and the query is the title — declared,
		// not hidden: such a case is the strongest possible known-item probe.
		out = balanceBrackets(strings.Join(lowerNonAcronyms(strings.Fields(base)), " "))
	}
	return out
}

// balanceBrackets drops unmatched brackets. Segment reordering routinely cuts
// through a parenthetical, and a stray "(" is a token the original title never
// had — noise the gold query should not carry into the measurement.
func balanceBrackets(s string) string {
	pairs := map[rune]rune{')': '(', ']': '[', '}': '{'}
	opens := map[rune]bool{'(': true, '[': true, '{': true}

	drop := map[int]bool{}
	var stack []int
	for i, r := range s {
		switch {
		case opens[r]:
			stack = append(stack, i)
		case pairs[r] != 0:
			if len(stack) == 0 || rune(s[stack[len(stack)-1]]) != pairs[r] {
				drop[i] = true
				continue
			}
			stack = stack[:len(stack)-1]
		}
	}
	for _, i := range stack {
		drop[i] = true
	}
	if len(drop) == 0 {
		return s
	}
	var b strings.Builder
	for i, r := range s {
		if !drop[i] {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(wsRun.ReplaceAllString(b.String(), " "))
}

// normalizeTitle strips decorative leading/trailing runes (emoji, bullets,
// quotes) and collapses whitespace.
func normalizeTitle(t string) string {
	t = wsRun.ReplaceAllString(strings.TrimSpace(t), " ")
	t = strings.TrimFunc(t, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	return strings.TrimSpace(t)
}

func splitSegments(s string) []string {
	raw := titleSeparator.Split(s, -1)
	out := make([]string, 0, len(raw))
	for _, seg := range raw {
		if seg = strings.TrimSpace(seg); seg != "" {
			out = append(out, seg)
		}
	}
	if len(out) == 0 {
		return []string{s}
	}
	return out
}

// pickSegments chooses which part of a multi-segment title carries the query.
// Single-segment titles pass through unchanged.
func pickSegments(segs []string, r *rand.Rand) string {
	if len(segs) < 2 {
		return segs[0]
	}
	head, tail := segs[0], strings.Join(segs[1:], " ")
	switch r.IntN(3) {
	case 0:
		if len(strings.Fields(tail)) >= 3 {
			return tail // drop the qualifier prefix
		}
	case 1:
		if len(strings.Fields(head)) >= 3 {
			return head // drop the qualifier suffix
		}
	}
	return tail + " " + head // reorder
}

// dropOneFiller removes a single function word, chosen by the seed, as long as
// enough tokens remain for the query to stay a known-item task.
func dropOneFiller(tokens []string, r *rand.Rand) []string {
	if len(tokens) < 5 {
		return tokens
	}
	idx := make([]int, 0, len(tokens))
	for i, t := range tokens {
		if fillerTokens[strings.ToLower(strings.Trim(t, ".,;"))] {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return tokens
	}
	drop := idx[r.IntN(len(idx))]
	out := make([]string, 0, len(tokens)-1)
	out = append(out, tokens[:drop]...)
	return append(out, tokens[drop+1:]...)
}

// lowerNonAcronyms case-folds ordinary words and leaves identifiers alone.
// Lowercasing "SGLang" or "PR#39" would degrade the query into a different
// task; lowercasing "Retrieval" is the paraphrase.
func lowerNonAcronyms(tokens []string) []string {
	out := make([]string, len(tokens))
	for i, t := range tokens {
		if isIdentifierToken(t) {
			out[i] = t
			continue
		}
		out[i] = strings.ToLower(t)
	}
	return out
}

// isIdentifierToken reports whether a token must survive verbatim: it carries a
// digit, is an all-caps acronym, or is CamelCase / dotted / underscored.
func isIdentifierToken(t string) bool {
	letters, upper := 0, 0
	for i, r := range t {
		switch {
		case unicode.IsDigit(r):
			return true
		case r == '_' || r == '/' || r == '.':
			return true
		case unicode.IsLetter(r):
			letters++
			if unicode.IsUpper(r) {
				upper++
				if i > 0 {
					return true // inner capital: CamelCase, ctx_RRF, SGLang
				}
			}
		}
	}
	return letters >= 2 && upper == letters
}

// hashSeed derives a per-title stream id so two titles with the same global
// seed do not receive the same transform sequence.
func hashSeed(s string) uint64 {
	var h uint64 = 1469598103934665603
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}
