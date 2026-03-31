// Package dream implements the async cross-reference engine ("Dream Mode").
// Picks low-quality blocks, extracts keywords, searches for related blocks,
// evaluates relationships via LLM, and creates cross-reference links.
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx
package dream

import (
	"strings"
	"unicode"
)

// stopwordsDE contains common German stopwords.
var stopwordsDE = map[string]bool{
	"der": true, "die": true, "das": true, "den": true, "dem": true, "des": true,
	"ein": true, "eine": true, "einer": true, "einem": true, "einen": true, "eines": true,
	"und": true, "oder": true, "aber": true, "als": true, "auch": true,
	"auf": true, "aus": true, "bei": true, "bis": true, "für": true,
	"mit": true, "nach": true, "von": true, "vor": true, "zu": true, "zum": true, "zur": true,
	"ist": true, "sind": true, "war": true, "wird": true, "wurde": true, "werden": true,
	"hat": true, "haben": true, "hatte": true, "kann": true, "nicht": true,
	"sich": true, "wie": true, "was": true, "wir": true, "ich": true, "sie": true, "er": true,
	"es": true, "nur": true, "noch": true, "über": true, "dass": true, "wenn": true,
	"dann": true, "schon": true, "mehr": true, "sehr": true, "hier": true, "dort": true,
	"diese": true, "dieser": true, "dieses": true, "diesem": true, "diesen": true,
	"keine": true, "kein": true, "keiner": true, "keinem": true, "keinen": true,
}

// stopwordsEN contains common English stopwords.
var stopwordsEN = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "but": true,
	"in": true, "on": true, "at": true, "to": true, "for": true, "of": true,
	"with": true, "by": true, "from": true, "as": true, "is": true, "are": true,
	"was": true, "were": true, "be": true, "been": true, "being": true,
	"have": true, "has": true, "had": true, "do": true, "does": true, "did": true,
	"will": true, "would": true, "could": true, "should": true, "may": true, "might": true,
	"shall": true, "can": true, "not": true, "no": true, "nor": true,
	"this": true, "that": true, "these": true, "those": true,
	"it": true, "its": true, "he": true, "she": true, "we": true, "they": true,
	"you": true, "i": true, "me": true, "my": true, "your": true, "our": true,
	"his": true, "her": true, "their": true, "them": true,
	"what": true, "which": true, "who": true, "whom": true, "how": true, "when": true,
	"where": true, "why": true, "if": true, "then": true, "than": true,
	"so": true, "too": true, "very": true, "just": true, "only": true,
	"also": true, "more": true, "most": true, "some": true, "any": true,
	"all": true, "each": true, "every": true, "both": true, "few": true,
	"into": true, "about": true, "after": true, "before": true, "between": true,
}

// ExtractKeywords extracts the top-N most distinctive terms from content.
// Deterministic: stopword filter + longest/rarest terms. No LLM, no corpus scan.
// Title terms get priority (appear in both title and content = more distinctive).
func ExtractKeywords(title, content string, limit int) []string {
	if limit <= 0 {
		limit = 5
	}

	titleTokens := tokenize(title)
	contentTokens := tokenize(content)

	// Count term frequency in content.
	freq := make(map[string]int)
	for _, t := range contentTokens {
		freq[t]++
	}

	// Score each unique term.
	type scored struct {
		term  string
		score float64
	}
	seen := make(map[string]bool)
	var candidates []scored

	for term, count := range freq {
		if seen[term] {
			continue
		}
		seen[term] = true

		if isStopword(term) || len(term) < 3 {
			continue
		}

		// Score heuristic: longer terms are more specific,
		// terms in title get 2x bonus, moderate frequency preferred.
		s := float64(len(term))

		// Title presence bonus.
		for _, tt := range titleTokens {
			if tt == term {
				s *= 2.0
				break
			}
		}

		// Moderate frequency bonus (2-5 occurrences = sweet spot).
		if count >= 2 && count <= 5 {
			s *= 1.5
		} else if count > 10 {
			// Very frequent in content = likely generic.
			s *= 0.5
		}

		candidates = append(candidates, scored{term, s})
	}

	// Sort by score descending, then alphabetically for determinism (insertion sort — small N).
	for i := 1; i < len(candidates); i++ {
		for j := i; j > 0 && (candidates[j].score > candidates[j-1].score ||
			(candidates[j].score == candidates[j-1].score && candidates[j].term < candidates[j-1].term)); j-- {
			candidates[j], candidates[j-1] = candidates[j-1], candidates[j]
		}
	}

	// Take top N.
	result := make([]string, 0, limit)
	for i := 0; i < len(candidates) && len(result) < limit; i++ {
		result = append(result, candidates[i].term)
	}
	return result
}

// tokenize splits text into lowercase tokens, stripping punctuation.
func tokenize(text string) []string {
	var tokens []string
	for _, word := range strings.Fields(text) {
		// Strip leading/trailing punctuation.
		clean := strings.TrimFunc(word, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-'
		})
		if clean == "" {
			continue
		}
		tokens = append(tokens, strings.ToLower(clean))
	}
	return tokens
}

// isStopword checks if a lowercased term is a stopword in DE or EN.
func isStopword(term string) bool {
	return stopwordsDE[term] || stopwordsEN[term]
}
