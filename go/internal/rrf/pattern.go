// Package rrf — pattern match engine for intent detection and write-side
// classification (Welle 41; WF T4 design/01 §4.4 #16 / §4.5).
//
// History: Welle 41 introduced HasAuditTrailIntent/AuditTrailFactor with a
// compiled-in 10-pattern audit-trail list (Iter 3 Pre-Empirie: Recall 0.86 /
// Precision 0.75; Iter 4 extension "dream v"/"performance"/"reset"/"baseline"
// → Recall 1.00, Precision 0.78). Wave T4 ends the pattern DUAL-USE: the
// list now lives as DATA in the block-type registry (M072 seeds, two
// independently editable fields — retrieval.intent_patterns read-side,
// classify.title_patterns write-side), and this file keeps only the ENGINE.
// Both consumers (blocktype.Set.DampedTypesFor and blocktype.Set.Classify)
// call MatchesAny with their own pattern source.
package rrf

import "strings"

// MatchesAny reports whether text contains any of the given patterns as a
// case-insensitive substring. Empty patterns are skipped (an empty pattern
// would match everything — a registry-config foot-gun, not a wildcard
// feature). nil/empty pattern list → false.
func MatchesAny(text string, patterns []string) bool {
	if len(patterns) == 0 {
		return false
	}
	lower := strings.ToLower(text)
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}
