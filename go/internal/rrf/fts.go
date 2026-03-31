package rrf

import (
	"strings"
)

// orStopwords are common function words filtered before OR-joining.
// PG's tsquery configs filter language stopwords, but high-frequency verbs like
// "run", "use", "make" still match too many blocks and dilute FTS ranking.
var orStopwords = map[string]bool{
	// English function words + high-frequency verbs
	"the": true, "and": true, "for": true, "are": true, "but": true,
	"not": true, "you": true, "all": true, "can": true, "had": true,
	"her": true, "was": true, "one": true, "our": true, "out": true,
	"has": true, "its": true, "let": true, "may": true, "who": true,
	"did": true, "get": true, "got": true, "him": true, "his": true,
	"how": true, "own": true, "say": true, "she": true,
	"too": true, "use": true, "way": true, "run": true, "set": true,
	"new": true, "now": true, "old": true, "see": true, "put": true,
	"does": true, "been": true, "have": true, "each": true, "make": true,
	"like": true, "just": true, "over": true, "such": true, "take": true,
	"than": true, "them": true, "very": true, "when": true, "come": true,
	"made": true, "find": true, "here": true, "many": true, "some": true,
	"what": true, "will": true, "with": true, "this": true, "that": true,
	"from": true, "they": true, "were": true, "said": true,
	"into": true, "more": true, "also": true, "then": true, "only": true,
	"which": true, "their": true, "about": true, "would": true, "there": true,
	"these": true, "other": true, "could": true, "after": true, "where": true,
	// German function words + high-frequency verbs
	"der": true, "die": true, "das": true, "den": true, "dem": true,
	"des": true, "ein": true, "eine": true, "und": true, "oder": true,
	"ist": true, "sind": true, "wird": true, "hat": true,
	"auf": true, "aus": true, "bei": true, "mit": true, "von": true,
	"zur": true, "zum": true, "als": true, "wie": true,
	"nur": true, "noch": true, "auch": true, "sich": true, "nicht": true,
	"nach": true, "vor": true, "über": true, "für": true, "durch": true,
}

// BuildORQuery converts a search query into OR-joined terms for websearch_to_tsquery.
// Removes stopwords, duplicates, expands hyphens, and caps at maxTerms to protect GIN scans at scale.
func BuildORQuery(query string, maxTerms int) string {
	if maxTerms <= 0 {
		maxTerms = 20
	}

	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		return ""
	}

	seen := make(map[string]bool, len(words))
	var terms []string

	for _, w := range words {
		// Strip common punctuation.
		w = strings.Trim(w, ".,;:!?\"'()[]{}…")
		if w == "" || len(w) < 2 {
			continue
		}
		if orStopwords[w] || seen[w] {
			continue
		}
		seen[w] = true
		terms = append(terms, w)

		// Hyphen expansion: "SHA-256" → also "SHA", "256".
		if strings.Contains(w, "-") {
			for _, part := range strings.Split(w, "-") {
				if part == "" || len(part) < 2 || seen[part] || orStopwords[part] {
					continue
				}
				seen[part] = true
				terms = append(terms, part)
			}
		}
	}

	if len(terms) == 0 {
		return ""
	}

	// Cap term count to protect GIN index at 1M+ blocks.
	if len(terms) > maxTerms {
		terms = terms[:maxTerms]
	}

	return strings.Join(terms, " OR ")
}
