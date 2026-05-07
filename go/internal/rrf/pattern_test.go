package rrf

import "testing"

func TestHasAuditTrailIntent(t *testing.T) {
	cases := []struct {
		query string
		want  bool
	}{
		// True positives — patterns from Welle 40 NEG flips.
		{"session 27 testcontainer behaviour", true},
		{"session 22 audit ergebnis", true},
		{"session 24 dream vor 2 wochen", true},
		{"ctx security audit anfang april 2026", true},
		// True negatives — generic queries.
		{"What embedding changes happened recently?", false},
		{"ddstatus mariadb donnerstags", false},
		{"dream v3 performance letzte woche", true}, // Welle 41 Iter 4: "dream v" + "performance" pattern
		// Case-insensitive.
		{"SESSION 27", true},
		{"Self-Audit gegen Warnings", true},
		// Multi-pattern.
		{"recurrent session pattern audit", true},
		// Empty + edge cases.
		{"", false},
		{"sess", false}, // partial match doesn't trigger (strict substring)
	}
	for _, c := range cases {
		got := HasAuditTrailIntent(c.query)
		if got != c.want {
			t.Errorf("HasAuditTrailIntent(%q) = %v, want %v", c.query, got, c.want)
		}
	}
}

func TestAuditTrailFactor(t *testing.T) {
	if got := AuditTrailFactor("session 27 testcontainer"); got != 1.0 {
		t.Errorf("audit-target query: factor = %g, want 1.0", got)
	}
	if got := AuditTrailFactor("recent embedding changes"); got != 0.3 {
		t.Errorf("generic query: factor = %g, want 0.3", got)
	}
	if got := AuditTrailFactor(""); got != 0.3 {
		t.Errorf("empty query: factor = %g, want 0.3", got)
	}
}
