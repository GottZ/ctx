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
	// Eleven builtins since M143: the four M035 enum classes + issue/comment
	// (Welle I-C) + checkpoint (ID-anchored evidence, out of every pipeline) +
	// the two tool-evidence axes (query-anchored evidence, damped instead of
	// excluded — that difference IS why they are not checkpoint) + the two
	// derived knowledge layers insight/catalog (blocks written ABOUT other
	// blocks; excluded until the E-4 visibility switch, out of every autonomous
	// pipeline permanently).
	want := []string{"audit-trail", "catalog", "checkpoint", "comment", "insight", "issue", "knowledge", "reference", "system-meta", "tool-evidence", "tool-overview"}
	if got := s.Names(); !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
	if s.Default().Name != "knowledge" {
		t.Errorf("Default() = %q, want knowledge", s.Default().Name)
	}
	// Retrieval-visible = full-pass|damped|aggregate. issue is full-pass; comment
	// is aggregate-to-parent (I-E flip) — hence VISIBLE (it ranks in RRF, then
	// folds onto its parent issue). system-meta + checkpoint stay excluded
	// (checkpoint evidence resolves over exact IDs only, M107); the two M136
	// tool types are damped and therefore VISIBLE — a query-anchored evidence
	// block that never ranks would be pointless. insight + catalog are excluded
	// too and therefore absent: M143 seeds them that way on purpose (K7/E-4,
	// excluded until the pilots), which is what makes that wave deployable —
	// p_types_visible stays byte-identical and ctx_rrf sees no change.
	if got := s.VisibleTypes(); !reflect.DeepEqual(got, []string{"audit-trail", "comment", "issue", "knowledge", "reference", "tool-evidence", "tool-overview"}) {
		t.Errorf("VisibleTypes() = %v (system-meta + checkpoint must be excluded; comment is aggregate-visible, the tool types damped-visible)", got)
	}
	// guard.check: the 4 builtins + issue; comment, checkpoint and the two M136
	// tool types are OUT (guard.check=false — consecutive evidence blocks of one
	// session are near-duplicates by construction, the default archive lane
	// broke ID chains, M107 / M136).
	if got := s.GuardCheckTypes(); !reflect.DeepEqual(got, []string{"audit-trail", "issue", "knowledge", "reference", "system-meta"}) {
		t.Errorf("GuardCheckTypes() = %v, want 4 builtins + issue (comment + checkpoint + tool types out)", got)
	}
	if got := s.GuardCandidateTypes(); !reflect.DeepEqual(got, []string{"audit-trail", "issue", "knowledge", "reference", "system-meta"}) {
		t.Errorf("GuardCandidateTypes() = %v, want 4 builtins + issue (comment + checkpoint + tool types out)", got)
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
	// The aggregate-to-parent set is now {comment} (I-E flip, migration 085): the
	// T11 fold consumer + the parent_id write path (I-D) are both live, so comment
	// carries retrieval=aggregate-to-parent and parent.mode=required/comment-of.
	if got := s.AggregateTypes(); !reflect.DeepEqual(got, []string{"comment"}) {
		t.Errorf("AggregateTypes() = %v, want [comment] (I-E flip)", got)
	}
	if got := s.ParentMode("comment"); got != ParentModeRequired {
		t.Errorf("ParentMode(comment) = %q, want required (I-E flip)", got)
	}
	// issue carries the structural-link write allowlist (§4.1).
	if p, ok := s.Resolve("issue"); !ok || !reflect.DeepEqual(p.StructuralLinkClasses, []string{"references", "duplicate-of"}) {
		t.Errorf("issue StructuralLinkClasses = %v, want [references duplicate-of]", p.StructuralLinkClasses)
	}
}

// dampedFactorFor returns the damping factor DampedTypesFor reports for one
// type, and whether the type is damped for that query at all (false = intent
// lift). Since M136 the arrays carry three damped builtins, so an exact
// DeepEqual against the whole array would pin unrelated types; the audit-trail
// contract below is about audit-trail alone.
func dampedFactorFor(s *Set, query, typeName string) (float64, bool) {
	names, factors := s.DampedTypesFor(query)
	for i, n := range names {
		if n == typeName {
			return factors[i], true
		}
	}
	return 0, false
}

// TestDampedTypesForAuditTrailGolden pins the generalized damping against
// FIXED expectations captured from rrf.AuditTrailFactor before T4 retired it
// (lift ⇔ the old factor was 1.0). The old function is gone — these literals
// are the frozen contract. The WHICH-queries-lift half is frozen since T4; the
// factor literal moved 0.3 → 0.6 with migration 146 (wave C2-1, measured in
// W01-M1) and is deliberately spelled out rather than read from
// auditTrailDamping: a pin that tracks the constant cannot catch the constant
// moving.
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
		factor, damped := dampedFactorFor(s, tc.query, "audit-trail")
		if tc.lift {
			if damped {
				names, _ := s.DampedTypesFor(tc.query)
				t.Errorf("query %q: damped %v, want audit-trail lifted out", tc.query, names)
			}
			continue
		}
		if !damped || factor != 0.6 {
			names, factors := s.DampedTypesFor(tc.query)
			t.Errorf("query %q: (%v, %v), want audit-trail damped at 0.6", tc.query, names, factors)
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
		// checkpoint (M107): the stable writer title prefix classifies both
		// manifest and part rows; is_meta (priority 10) still wins over it.
		{"checkpoint manifest title", "Compaction source 20260712_205012_837f2c 1816f6b3ce6fc7e8 5e6b1698beab7814 manifest", nil, "checkpoint", true},
		{"checkpoint part title", "Compaction source candidate-e2e-x 00ff 00ff part 001 of 002", nil, "checkpoint", true},
		{"is_meta beats checkpoint pattern", "Compaction source irrelevant", map[string]any{"is_meta": true}, "system-meta", true},
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
		// Only audit-trail's own lift is asserted: since M136 the damping
		// arrays carry two more builtins that these patterns do not address.
		if _, damped := dampedFactorFor(s, "xx "+probe+" yy", "audit-trail"); damped {
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

// GB5: StructuralClasses is the sorted distinct union of every type's
// structural_link_classes — the link_class partition vocabulary.
func TestSetStructuralClasses(t *testing.T) {
	mk := func(name string, def bool, classes ...string) Policy {
		p := Policy{Name: name, IsDefault: def}
		p.Retrieval.Kind = RetrievalFullPass
		p.StructuralLinkClasses = classes
		return p
	}
	s, err := NewSet([]Policy{
		mk("knowledge", true),
		mk("issue", false, "references", "duplicate-of"),
		mk("audit-trail", false, "references"),
	})
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	got := s.StructuralClasses()
	want := []string{"duplicate-of", "references"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("StructuralClasses = %v, want %v (sorted distinct union)", got, want)
	}

	// Split precedence (design/01 §4.6(b)): a reserved dream name never routes
	// into the structural vocabulary — even if it somehow bypassed the
	// DecodePolicy guard (this Set-level filter is the second defense line;
	// GB5 review: previously untested, a filter removal stayed green).
	s2, err := NewSet([]Policy{
		mk("knowledge", true),
		mk("legacy", false, "topical", "references"),
	})
	if err != nil {
		t.Fatalf("NewSet (reserved): %v", err)
	}
	for _, c := range s2.StructuralClasses() {
		if c == "topical" {
			t.Fatal("reserved dream name routed into the structural vocabulary — Set filter dead")
		}
	}
	if got := s2.StructuralClasses(); len(got) != 1 || got[0] != "references" {
		t.Errorf("StructuralClasses = %v, want [references] (reserved filtered)", got)
	}
}
