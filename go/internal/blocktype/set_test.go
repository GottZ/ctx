package blocktype

import (
	"reflect"
	"testing"
)

func builtinTestSet(t *testing.T) *Set {
	t.Helper()
	s, err := NewSet(builtinPolicies())
	if err != nil {
		t.Fatalf("builtin set: %v", err)
	}
	return s
}

func TestBuiltinSetShape(t *testing.T) {
	s := builtinTestSet(t)
	// Six builtins since Welle I-C: the four M035 enum classes + issue/comment.
	want := []string{"audit-trail", "comment", "issue", "knowledge", "reference", "system-meta"}
	if got := s.Names(); !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
	if s.Default().Name != "knowledge" {
		t.Errorf("Default() = %q, want knowledge", s.Default().Name)
	}
	// Retrieval-visible = full-pass|damped|aggregate. issue is full-pass; comment
	// and system-meta are excluded (comment INTERIM-excluded until the T11 fold).
	if got := s.VisibleTypes(); !reflect.DeepEqual(got, []string{"audit-trail", "issue", "knowledge", "reference"}) {
		t.Errorf("VisibleTypes() = %v (comment + system-meta must be excluded)", got)
	}
	// guard.check: the 4 builtins + issue; comment is OUT (guard.check=false).
	if got := s.GuardCheckTypes(); !reflect.DeepEqual(got, []string{"audit-trail", "issue", "knowledge", "reference", "system-meta"}) {
		t.Errorf("GuardCheckTypes() = %v, want 4 builtins + issue (comment out)", got)
	}
	if got := s.GuardCandidateTypes(); !reflect.DeepEqual(got, []string{"audit-trail", "issue", "knowledge", "reference", "system-meta"}) {
		t.Errorf("GuardCandidateTypes() = %v, want 4 builtins + issue (comment out)", got)
	}
	// The four M035 classes keep the guard bestand — archive persist + cross-
	// scope candidates. Builtins are constructed directly (not via DecodePolicy),
	// so these fields must be set explicitly or the guard silently flag-persists.
	for _, n := range []string{"knowledge", "reference", "audit-trail", "system-meta"} {
		if got := s.GuardMode(n); got != GuardModeArchive {
			t.Errorf("GuardMode(%q) = %q, want archive (builtin bestand)", n, got)
		}
		if s.GuardSameScopeOnly(n) {
			t.Errorf("GuardSameScopeOnly(%q) = true, want false (builtin cross-scope bestand)", n)
		}
	}
	// issue deviates by design (§4.7): flag persist (never auto-archive) +
	// same-scope candidates (never a cross-tenant match).
	if got := s.GuardMode("issue"); got != GuardModeFlag {
		t.Errorf("GuardMode(issue) = %q, want flag (§4.7)", got)
	}
	if !s.GuardSameScopeOnly("issue") {
		t.Errorf("GuardSameScopeOnly(issue) = false, want true (§5.3)")
	}
	if dup, review := s.GuardThresholds("issue"); dup != 0.97 || review != 0.90 {
		t.Errorf("issue thresholds = (%v, %v), want (0.97, 0.90)", dup, review)
	}
	// dream.linkable: 3 builtins + issue; comment + system-meta are out.
	if got := s.DreamLinkableTypes(); !reflect.DeepEqual(got, []string{"audit-trail", "issue", "knowledge", "reference"}) {
		t.Errorf("DreamLinkableTypes() = %v (comment + system-meta must be out)", got)
	}
	// digest/overview stay the four builtins — issue + comment are EXCLUDED so a
	// 10k-issue repo never floods the topic-map or Louvain clustering (§6.8).
	if got := s.DigestTypes(); !reflect.DeepEqual(got, []string{"audit-trail", "knowledge", "reference", "system-meta"}) {
		t.Errorf("DigestTypes() = %v, want the 4 builtins (issue+comment excluded)", got)
	}
	if got := s.OverviewTypes(); !reflect.DeepEqual(got, []string{"audit-trail", "knowledge", "reference", "system-meta"}) {
		t.Errorf("OverviewTypes() = %v, want the 4 builtins (issue+comment excluded)", got)
	}
	// The aggregate-to-parent gate stays CLOSED: comment ships excluded, NOT
	// aggregate, because the T11 fold has no consumer (kein Gate aufweichen).
	if got := s.AggregateTypes(); len(got) != 0 {
		t.Errorf("AggregateTypes() = %v, want empty (T11 fold gate stays closed)", got)
	}
	// issue carries the structural-link write allowlist (§4.1).
	if p, ok := s.Resolve("issue"); !ok || !reflect.DeepEqual(p.StructuralLinkClasses, []string{"references", "duplicate-of"}) {
		t.Errorf("issue StructuralLinkClasses = %v, want [references duplicate-of]", p.StructuralLinkClasses)
	}
}

// TestDampedTypesForAuditTrailGolden pins the generalized damping against
// FIXED expectations captured from rrf.AuditTrailFactor before T4 retired it
// (lift ⇔ the old factor was 1.0). The old function is gone — these literals
// are the frozen contract.
func TestDampedTypesForAuditTrailGolden(t *testing.T) {
	s := builtinTestSet(t)
	cases := []struct {
		query string
		lift  bool // true ⇔ old rrf.AuditTrailFactor(query) == 1.0
	}{
		{"wie funktioniert der embed cache", false},
		{"session handover von gestern", true},
		{"Welle 41 AUDIT ergebnisse", true},
		{"dream v3 performance letzte woche", true},
		{"was ist der aktuelle stand", false},
		{"baseline vergleich", true},
		{"", false},
	}
	for _, tc := range cases {
		names, factors := s.DampedTypesFor(tc.query)
		if tc.lift {
			if len(names) != 0 {
				t.Errorf("query %q: damped %v, want intent lift (empty arrays)", tc.query, names)
			}
			continue
		}
		if !reflect.DeepEqual(names, []string{"audit-trail"}) || !reflect.DeepEqual(factors, []float64{0.3}) {
			t.Errorf("query %q: (%v, %v), want ([audit-trail], [0.3])", tc.query, names, factors)
		}
	}
}

func TestGuardThresholds(t *testing.T) {
	s := builtinTestSet(t)
	if dup, review := s.GuardThresholds("knowledge"); dup != DefaultThresholdDuplicate || review != DefaultThresholdReview {
		t.Errorf("knowledge thresholds = (%v, %v), want defaults", dup, review)
	}
	// Unknown name → defaults (fail-safe, not zero).
	if dup, review := s.GuardThresholds("nonexistent"); dup != DefaultThresholdDuplicate || review != DefaultThresholdReview {
		t.Errorf("unknown-type thresholds = (%v, %v), want defaults", dup, review)
	}
	// Per-type override.
	thr := 0.95
	p, _ := DecodePolicy("strict-type", globalScope, false, false, []byte(`{"v":1,"guard":{"threshold_duplicate":0.95}}`))
	if p.Guard.ThresholdDuplicate == nil || *p.Guard.ThresholdDuplicate != thr {
		t.Fatalf("decoded threshold = %v, want %v", p.Guard.ThresholdDuplicate, thr)
	}
	custom, err := NewSet(append(builtinPolicies(), p))
	if err != nil {
		t.Fatalf("set with override: %v", err)
	}
	if dup, review := custom.GuardThresholds("strict-type"); dup != thr || review != DefaultThresholdReview {
		t.Errorf("override thresholds = (%v, %v), want (%v, %v)", dup, review, thr, DefaultThresholdReview)
	}
}

// TestClassifyMirrorsDecisionTree pins Set.Classify against the M035/Welle-44
// decision-tree semantics of store.ClassifyBlockAfterUpsert: is_meta flag
// (priority 10) → source prefix / title pattern (priority 20) → default.
func TestClassifyMirrorsDecisionTree(t *testing.T) {
	s := builtinTestSet(t)
	cases := []struct {
		name     string
		title    string
		metadata map[string]any
		want     string
		matched  bool
	}{
		{"is_meta flag wins", "Session 12 handover", map[string]any{"is_meta": true}, "system-meta", true},
		{"is_meta false is no flag", "plain block", map[string]any{"is_meta": false}, "knowledge", false},
		{"dream source prefix", "daily report", map[string]any{"source": "dream-synthesis"}, "audit-trail", true},
		{"title pattern", "Welle 41 Ergebnisse", nil, "audit-trail", true},
		{"title pattern case-insensitive", "SELF-AUDIT protokoll", nil, "audit-trail", true},
		{"no match falls to default", "pgvector tuning notes", map[string]any{"source": "claude-code"}, "knowledge", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, matched := s.Classify(tc.title, tc.metadata)
			if got != tc.want || matched != tc.matched {
				t.Errorf("Classify(%q, %v) = (%q, %v), want (%q, %v)",
					tc.title, tc.metadata, got, matched, tc.want, tc.matched)
			}
		})
	}
}

func TestNewSetRejectsBrokenDefaults(t *testing.T) {
	// No default at all.
	pols := builtinPolicies()
	for i := range pols {
		pols[i].IsDefault = false
	}
	if _, err := NewSet(pols); err == nil {
		t.Error("set without default accepted, want reject")
	}
	// Two defaults.
	pols = builtinPolicies()
	pols[1].IsDefault = true // reference next to knowledge
	if _, err := NewSet(pols); err == nil {
		t.Error("set with two defaults accepted, want reject")
	}
	// Empty set.
	if _, err := NewSet(nil); err == nil {
		t.Error("empty set accepted, want reject")
	}
}

// TestBuiltinPatternsDriveEngine replaces the pre-T4 TestBuiltinPatternsMatchRRF:
// the rrf list is retired (§4.4 #16), the builtin copy is the ONLY code-side
// list. This test pins that every builtin pattern actually fires through the
// shared engine paths (DampedTypesFor lift + Classify title rule).
func TestBuiltinPatternsDriveEngine(t *testing.T) {
	s := builtinTestSet(t)
	for _, probe := range auditPatterns {
		if names, _ := s.DampedTypesFor("xx " + probe + " yy"); len(names) != 0 {
			t.Errorf("pattern %q does not lift audit-trail damping via the engine", probe)
		}
		if name, matched := s.Classify("xx "+probe+" yy", nil); !matched || name != "audit-trail" {
			t.Errorf("pattern %q does not classify audit-trail via the engine (got %q, %v)", probe, name, matched)
		}
	}
}

// TestClassifySourceProperPrefix pins the old-tree edge case (T4 golden
// corpus, case source-dream-exact): a source that IS the bare prefix
// ("dream-", no payload) never matched (len(src) > 6) and must keep not
// matching under the registry engine.
func TestClassifySourceProperPrefix(t *testing.T) {
	s := builtinTestSet(t)
	if name, matched := s.Classify("Irgendwas", map[string]any{"source": "dream-"}); matched {
		t.Errorf("bare-prefix source classified as %q, want no match (old-tree parity)", name)
	}
	if name, matched := s.Classify("Irgendwas", map[string]any{"source": "dream-x"}); !matched || name != "audit-trail" {
		t.Errorf("proper-prefix source = (%q, %v), want (audit-trail, true)", name, matched)
	}
}
