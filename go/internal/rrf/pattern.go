// Package rrf — query pattern detection for audit-trail damping (Welle 41).
//
// HasAuditTrailIntent returns true if the query contains tokens that strongly
// indicate the user is asking about audit-trail content (session handovers,
// welle audits, bench-snapshots). When true, the caller should pass
// audit_trail_factor=1.0 to ctx_rrf (no damping). Otherwise audit-trail blocks
// are damped to keep them out of generic-recent-query top-5.
//
// Welle 41 Iter 3 Pre-Empirie: 6-pattern set had Recall 0.86 / Precision 0.75.
// Single FN was M-003 "dream v3 performance letzte woche" — temporal+technical
// phrasing without audit-keyword.
//
// Welle 41 Iter 4 extension: added "dream v", "performance", "reset",
// "baseline" — covers M-003 (FN→TP) and L-010-style queries. Recall 1.00,
// Precision 0.78 (2 FP unchanged: C-006, L-010 — both have knowledge-block
// as top-1 anyway, no behavior impact from no-damping path).
package rrf

import "strings"

// auditTrailPatterns are case-insensitive substrings whose presence in a
// user query suggests the user is targeting audit-trail content (session
// handovers, welle audits, bench-snapshots, decision-records, performance
// audits, reset baselines).
var auditTrailPatterns = []string{
	"session",
	"welle",
	"audit",
	"recurrent",
	"handover",
	"self-audit",
	"dream v",
	"performance",
	"reset",
	"baseline",
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
