package blocktype

import (
	"fmt"
	"slices"
)

// D6, WRITE side: monotone narrowing (board decision E2-6, wave C2-4).
//
// THE RULE. A tenant overlay — a context_block_types row in a scope other than
// '_global' — may only NARROW the '_global' base policy of the same name.
// Widening is refused at the write path.
//
// The rule is exactly the ten axes listed below, and no more. It does NOT say
// "a tenant can never widen anything": the axes outside the list (the full
// enumeration is under WHAT IS DELIBERATELY NOT AN AXIS) stay freely
// overlayable in BOTH directions — a dream.link_classes allowlist can be
// overlaid back to nil = all (policy.go:191-193), a guard.candidates of
// same-scope back to all. That is inert today because the
// guard consumes the BASE generation (events/scheduler.go:1680,
// s.blocktypes.Snapshot()), not the tenant set. T12 MERKER: the per-tenant loop
// announced at events/scheduler.go:1673-1675 makes those axes reachable — the
// wave that builds it has to decide whether they join this list.
//
// WHY IT IS A WRITE RULE AND NOT A READ RULE. buildTenantSet still merges with
// "the tenant row WINS" (registry.go:252-291) and this file does not change
// that. Two reasons: the read path is the hot path at the 1M+ target scale
// (a per-resolve comparison against the base would run on every request), and
// silently narrowing a row that is already in the table would hide the fact
// that it is there.
//
// WHAT THE GATE DOES AND DOES NOT GUARANTEE. It makes a loosening row
// impossible to CREATE through any transport AS LONG AS THE '_global' BASE
// EXISTS BEFORE THE OVERLAY. It does not guarantee that no such row can come
// into existence at all, and two vectors are known and deliberately left open
// here (both are board points, not silent gaps):
//
//   - ORDER. A tenant may claim a name while it has no '_global' base — a
//     legitimate tenant-own type, admitted because there is nothing to narrow
//     (types_write.go, overlayBasePolicy returns ok=false). If that name LATER
//     becomes a '_global' builtin, the pre-existing row is retroactively a
//     widening overlay, and nothing re-checks it. The realistic trigger is the
//     migration runner (store/migrations.go:137), which executes the file body
//     directly and passes every Go gate — that is how tool-evidence (136),
//     insight and catalog (143) arrived. Closing it cheaply would mean checking,
//     on the '_global' write, whether an existing non-'_global' row of the same
//     name would be widened by the new base.
//   - PLANTED. A row written past the API (psql) still wins on read.
//
// In both cases the W01-7 invariant guard is what turns the state red — and it
// carries the //go:build integration tag, which CI does not run.
//
// WHERE THE CLAUSES COME FROM. Every axis below carries a named invariant of
// the two findings this wave closes — nothing is here for symmetry:
//
//	N1 guard.check                  D-INV 1  — a derivative in the dedup batch
//	N2 guard.candidate              D-INV 2  — a derivative archiving its source
//	N3 dream.linkable               D-INV 3  — dream links are Louvain's input
//	N4 overview.include             D-INV 4  + D-INV 9 (untrusted => !overview)
//	N5 digest.include               D-INV 5  + D-INV 10 (untrusted => !digest)
//	N6 retrieval.untrusted          the PREMISE of D-INV 9/10 and of the §4.8.3
//	                                inheritance clause: dropping the flag makes
//	                                foreign text read as first-party
//	N7 retrieval.shadow_measurable  the M-W2 G5 measurement seam (design/05 §4.2)
//	N8 retrieval.policy             D-INV 6 + OPS-W1 review #3: an overlay that
//	                                lifts a deny-listed type to a visible policy
//	                                puts it into p_types_visible, where migration
//	                                145's static conjunct then cuts its FTS
//	                                contribution (measured 67 -> 0 rows)
//	N9 retrieval.damping_factor     the same visibility axis, one level down:
//	                                a larger factor is a weaker damping
//	N10 retrieval.intent_patterns   a pattern LIFTS the damping to 1.0 for a
//	                                matching query (rrf.MatchesAny), so a new
//	                                pattern widens N9 without touching it
//
// WHAT IS DELIBERATELY NOT AN AXIS. guard.mode, guard.candidates, the guard
// thresholds, dream.link_classes, parent.*, workflow.*, structural_link_classes
// and classify.* carry no clause of either finding. They stay freely
// overlayable, which is what keeps a legitimate per-tenant type usable.
//
// ONE CONSEQUENCE WORTH KNOWING. A config is a COMPLETE policy, not a patch:
// DecodePolicy default-fills every absent section with its WIDE value
// (policy.go:377-383). An overlay body that omits a section therefore requests
// the wide default for it and is refused over a tight base. That is the
// existing config semantics — the read-side merge replaces a policy wholesale
// too — not an extra rule of this gate.

// BuiltinPolicy returns a copy of the compiled-in floor policy of name.
//
// It exists for the write gate: the base a tenant overlay must narrow against
// is the SAME merge Reload performs — the compiled-in floor with the '_global'
// table row over it (registry.go:393-411). A builtin whose '_global' ROW is
// gone (the B15 state W01-7 clause 11 describes) still resolves off this floor,
// and that is precisely the state in which the create path can be reached with
// a '_global' name.
func BuiltinPolicy(name string) (Policy, bool) {
	for _, p := range builtinPolicies() {
		if p.Name == name {
			return p, true
		}
	}
	return Policy{}, false
}

// NarrowingViolation reports the first axis on which overlay is WIDER than
// base, as a message carrying the field path and both values. Empty string =
// overlay only narrows (or matches) the base and is admissible.
//
// Order is N1..N10 as documented above; the first violation is reported, the
// same shape validatePolicy uses for its cross-field rules.
func NarrowingViolation(base, overlay Policy) string {
	for _, c := range []struct {
		field           string
		widened         bool
		baseVal, ovlVal any
	}{
		{"guard.check", overlay.Guard.Check && !base.Guard.Check, base.Guard.Check, overlay.Guard.Check},
		{"guard.candidate", overlay.Guard.Candidate && !base.Guard.Candidate, base.Guard.Candidate, overlay.Guard.Candidate},
		{"dream.linkable", overlay.Dream.Linkable && !base.Dream.Linkable, base.Dream.Linkable, overlay.Dream.Linkable},
		{"overview.include", overlay.Overview.Include && !base.Overview.Include, base.Overview.Include, overlay.Overview.Include},
		{"digest.include", overlay.Digest.Include && !base.Digest.Include, base.Digest.Include, overlay.Digest.Include},
		// Untrusted is the one axis whose TRUE is the narrow value: the flag
		// marks foreign text, and removing it launders it into first-party
		// material.
		{"retrieval.untrusted", base.Retrieval.Untrusted && !overlay.Retrieval.Untrusted, base.Retrieval.Untrusted, overlay.Retrieval.Untrusted},
		{"retrieval.shadow_measurable", overlay.Retrieval.ShadowMeasurable && !base.Retrieval.ShadowMeasurable,
			base.Retrieval.ShadowMeasurable, overlay.Retrieval.ShadowMeasurable},
		{"retrieval.policy", !retrievalNarrows(base.Retrieval.Kind, overlay.Retrieval.Kind),
			base.Retrieval.Kind, overlay.Retrieval.Kind},
	} {
		if c.widened {
			return fmt.Sprintf("%s: a tenant overlay may only narrow the '%s' base (base=%v, overlay=%v)",
				c.field, globalScope, c.baseVal, c.ovlVal)
		}
	}
	return dampingNarrowingViolation(base, overlay)
}

// retrievalNarrows reports whether moving from base to overlay is a narrowing
// of retrieval visibility.
//
// A partial order, NOT a rank: excluded is the minimum and always reachable,
// an unchanged kind is trivially admissible, and full-pass -> damped is the one
// genuine step between two visible kinds. Everything else — damped ->
// full-pass, excluded -> anything, and any move INTO aggregate-to-parent — is
// refused. aggregate-to-parent is deliberately not placed on the same axis: its
// hits fold onto a structural parent (the T11 fold), which is a different
// semantics rather than a weaker or stronger visibility, so a tenant cannot
// reach it from another kind.
func retrievalNarrows(base, overlay string) bool {
	switch {
	case overlay == base:
		return true
	case overlay == RetrievalExcluded:
		return true
	case base == RetrievalFullPass && overlay == RetrievalDamped:
		return true
	default:
		return false
	}
}

// dampingNarrowingViolation is N9 and N10 — the two axes that live INSIDE a
// damped policy and are invisible to the kind comparison.
//
// Both only apply when base and overlay are damped: from a full-pass base every
// damped configuration is a narrowing whatever its factor and patterns, and
// from an excluded base no damped overlay gets past retrievalNarrows at all.
func dampingNarrowingViolation(base, overlay Policy) string {
	if base.Retrieval.Kind != RetrievalDamped || overlay.Retrieval.Kind != RetrievalDamped {
		return ""
	}
	// A LARGER factor is a weaker damping — the score multiplier is applied to
	// every arm, so 0.9 leaves nine tenths of the score where 0.2 leaves a
	// fifth.
	if overlay.Retrieval.DampingFactor > base.Retrieval.DampingFactor {
		return fmt.Sprintf("retrieval.damping_factor: a tenant overlay may only narrow the '%s' base "+
			"(base=%v, overlay=%v — a larger factor is a weaker damping)",
			globalScope, base.Retrieval.DampingFactor, overlay.Retrieval.DampingFactor)
	}
	for _, pat := range overlay.Retrieval.IntentPatterns {
		if !slices.Contains(base.Retrieval.IntentPatterns, pat) {
			return fmt.Sprintf("retrieval.intent_patterns: a tenant overlay may only narrow the '%s' base "+
				"(%q is not in the base list — a matching pattern lifts the damping to 1.0)",
				globalScope, pat)
		}
	}
	return ""
}
