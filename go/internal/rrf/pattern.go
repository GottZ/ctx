// Package rrf — query pattern detection for audit-trail damping (Welle 41).
//
// HasAuditTrailIntent returns true if the query contains tokens that strongly
// indicate the user is asking about audit-trail content (session handovers,
// welle audits, bench-snapshots). When true, the caller should pass
// audit_trail_factor=1.0 to ctx_rrf (no damping). Otherwise audit-trail blocks
// are damped to keep them out of generic-recent-query top-5.
//
// Welle 41 Pre-Empirie: pattern set has Recall 0.86 / Precision 0.75 over the
// 70 eval-cyclic-gold cases (welle-41-pattern-audit.json). Single FN: M-003
// "dream v3 performance letzte woche" — temporal+technical phrasing without
// audit-keyword. Welle 42+ may broaden the pattern set or re-classify the
// edge case's expected block_role.
package rrf

import "strings"

// auditTrailPatterns are case-insensitive substrings whose presence in a
// user query suggests the user is targeting audit-trail content (session
// handovers, welle audits, bench-snapshots, decision-records).
var auditTrailPatterns = []string{
	"session",
	"welle",
	"audit",
	"recurrent",
	"handover",
	"self-audit",
}

// HasAuditTrailIntent returns true when the query contains any of the
// audit-trail signal patterns. Case-insensitive substring match.
func HasAuditTrailIntent(query string) bool {
	q := strings.ToLower(query)
	for _, p := range auditTrailPatterns {
		if strings.Contains(q, p) {
			return true
		}
	}
	return false
}

// AuditTrailFactor returns the damping factor to pass to ctx_rrf based on
// query intent. 1.0 (no damping) when audit-trail-intent detected, otherwise
// 0.3 (audit-trail-blocks damped under knowledge for generic-recent queries).
func AuditTrailFactor(query string) float64 {
	if HasAuditTrailIntent(query) {
		return 1.0
	}
	return 0.3
}
