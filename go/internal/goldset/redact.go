package goldset

import (
	"regexp"

	"github.com/GottZ/ctx/internal/sensitivity"
)

// reBearer covers the one credential shape the corpus scanner does not carry:
// an HTTP Bearer header pasted into a query. sensitivity.Scan catches the token
// only when it is itself a JWT or a vendor-prefixed key; a plain opaque bearer
// value would slip through, and that is exactly the case the wave gate probes.
var reBearer = regexp.MustCompile(`(?i)\b(?:bearer|authorization\s*[:=]\s*bearer)\s+[A-Za-z0-9._~+/=-]{12,}`)

// ScanQuery reports whether a query text carries a credential signal.
//
// Policy (design 04 §4.5): a query that fires here is DISCARDED, never carried
// on redacted. A part-redacted query text is no longer a real query and would
// destroy the external validity that G-REAL exists for. The returned Match is
// log-safe — it names the rule, never the matched secret.
func ScanQuery(q string) (sensitivity.Match, bool) {
	if reBearer.MatchString(q) {
		return sensitivity.Match{Kind: "bearer-token", Reason: "HTTP Bearer credential"}, true
	}
	return sensitivity.Scan(q)
}
