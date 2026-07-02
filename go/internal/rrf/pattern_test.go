package rrf

import "testing"

// builtinAuditPatterns replays the M072 audit-trail seed list — engine tests
// keep exercising the REAL pattern shapes the registry ships (the list itself
// is data now; drift between seeds and builtin set is pinned by the blocktype
// golden integration test, not here).
var builtinAuditPatterns = []string{
	"session", "welle", "audit", "recurrent", "handover",
	"self-audit", "dream v", "performance", "reset", "baseline",
}

func TestMatchesAny(t *testing.T) {
	cases := []struct {
		text     string
		patterns []string
		want     bool
	}{
		// True positives — patterns from Welle 40 NEG flips.
		{"session 27 testcontainer behaviour", builtinAuditPatterns, true},
		{"session 22 audit ergebnis", builtinAuditPatterns, true},
		{"session 24 dream vor 2 wochen", builtinAuditPatterns, true},
		{"ctx security audit anfang april 2026", builtinAuditPatterns, true},
		{"dream v3 performance letzte woche", builtinAuditPatterns, true}, // Welle 41 Iter 4
		// True negatives — generic queries.
		{"What embedding changes happened recently?", builtinAuditPatterns, false},
		{"ddstatus mariadb donnerstags", builtinAuditPatterns, false},
		// Case-insensitive, both sides.
		{"SESSION 27", builtinAuditPatterns, true},
		{"Self-Audit gegen Warnings", builtinAuditPatterns, true},
		{"kleine welle im teich", []string{"WELLE"}, true},
		// Multi-pattern.
		{"recurrent session pattern audit", builtinAuditPatterns, true},
		// Empty + edge cases.
		{"", builtinAuditPatterns, false},
		{"sess", builtinAuditPatterns, false}, // partial match doesn't trigger (strict substring)
		{"anything", nil, false},              // nil pattern list → never
		{"anything", []string{}, false},       // empty pattern list → never
		{"anything", []string{""}, false},     // empty pattern skipped, not wildcard
	}
	for _, c := range cases {
		got := MatchesAny(c.text, c.patterns)
		if got != c.want {
			t.Errorf("MatchesAny(%q, %v) = %v, want %v", c.text, c.patterns, got, c.want)
		}
	}
}
