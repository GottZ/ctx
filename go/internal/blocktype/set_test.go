package blocktype

import (
	"reflect"
	"testing"

	"github.com/GottZ/ctx/internal/rrf"
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
	want := []string{"audit-trail", "knowledge", "reference", "system-meta"}
	if got := s.Names(); !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
	if s.Default().Name != "knowledge" {
		t.Errorf("Default() = %q, want knowledge", s.Default().Name)
	}
	// system-meta is excluded from retrieval but present everywhere the M035
	// behaviour keeps it (guard, digest, overview) — byte-equivalence contract.
	if got := s.VisibleTypes(); !reflect.DeepEqual(got, []string{"audit-trail", "knowledge", "reference"}) {
		t.Errorf("VisibleTypes() = %v (system-meta must be excluded)", got)
	}
	if got := s.GuardCheckTypes(); len(got) != 4 {
		t.Errorf("GuardCheckTypes() = %v, want all 4", got)
	}
	if got := s.GuardCandidateTypes(); len(got) != 4 {
		t.Errorf("GuardCandidateTypes() = %v, want all 4", got)
	}
	if got := s.DreamLinkableTypes(); !reflect.DeepEqual(got, []string{"audit-trail", "knowledge", "reference"}) {
		t.Errorf("DreamLinkableTypes() = %v (system-meta = NOT is_meta must be out)", got)
	}
	if got := s.DigestTypes(); len(got) != 4 {
		t.Errorf("DigestTypes() = %v, want all 4 (today's non-filter)", got)
	}
	if got := s.OverviewTypes(); len(got) != 4 {
		t.Errorf("OverviewTypes() = %v, want all 4 (today's non-filter)", got)
	}
	if got := s.AggregateTypes(); len(got) != 0 {
		t.Errorf("AggregateTypes() = %v, want empty before T11", got)
	}
}

// TestDampedTypesForMirrorsAuditTrailFactor pins the generalized damping
// against the LIVE rrf implementation it replaces in T5 — for every probe
// query both sides must agree (lift ⇔ factor 1.0).
func TestDampedTypesForMirrorsAuditTrailFactor(t *testing.T) {
	s := builtinTestSet(t)
	queries := []string{
		"wie funktioniert der embed cache",
		"session handover von gestern",
		"Welle 41 AUDIT ergebnisse",
		"dream v3 performance letzte woche",
		"was ist der aktuelle stand",
		"baseline vergleich",
	}
	for _, q := range queries {
		names, factors := s.DampedTypesFor(q)
		wantLift := rrf.AuditTrailFactor(q) == 1.0
		if wantLift {
			if len(names) != 0 {
				t.Errorf("query %q: damped %v, want lift (rrf says intent)", q, names)
			}
			continue
		}
		if !reflect.DeepEqual(names, []string{"audit-trail"}) || !reflect.DeepEqual(factors, []float64{0.3}) {
			t.Errorf("query %q: (%v, %v), want ([audit-trail], [0.3])", q, names, factors)
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

// TestBuiltinPatternsMatchRRF pins the deliberate T3-window copy of the
// audit patterns against the live rrf list (§4.4 #16 retires the rrf copy in
// T4/T5; until then a drift in either list goes red here).
func TestBuiltinPatternsMatchRRF(t *testing.T) {
	for _, probe := range auditPatterns {
		if !rrf.HasAuditTrailIntent("xx " + probe + " yy") {
			t.Errorf("pattern %q not recognized by rrf.HasAuditTrailIntent — lists drifted", probe)
		}
	}
	// Counter-direction: a query with no builtin pattern must not be intent
	// in rrf either (spot probe, not exhaustive by construction).
	if rrf.HasAuditTrailIntent("embed cache tuning") {
		t.Error("rrf matches a query no builtin pattern covers — lists drifted")
	}
}
