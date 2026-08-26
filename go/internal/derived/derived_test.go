package derived

import "testing"

// TestStratumOf pins the one type-name-to-level mapping in the tree.
//
// The level-2 decision is deliberate and documented in the package doc: no
// type name maps to 2. A super-catalogue is written as type "catalog" carrying
// provenance.stratum = 2, and Validate takes the writer's own level as a
// parameter. Deriving the level from the type name would put it into the type
// registry — a table an operator can edit with SQL — which is exactly the way
// of breaking I2 that §4.5.1 rejects.
func TestStratumOf(t *testing.T) {
	cases := map[string]Stratum{
		TypeInsight:       StratumDerived,
		TypeCatalog:       StratumDerived,
		"session-insight": StratumSource, // K3: the name that was NOT chosen
		"super-catalog":   StratumSource, // no type name carries level 2
		"tool-evidence":   StratumSource,
		"decision":        StratumSource,
		"":                StratumSource,
	}
	for name, want := range cases {
		if got := StratumOf(name); got != want {
			t.Errorf("StratumOf(%q) = %d, want %d", name, got, want)
		}
	}
}

// TestIsDerivedType — the boolean form must agree with StratumOf, always.
func TestIsDerivedType(t *testing.T) {
	for _, name := range []string{TypeInsight, TypeCatalog, "decision", "note", ""} {
		if got, want := IsDerivedType(name), StratumOf(name) > StratumSource; got != want {
			t.Errorf("IsDerivedType(%q) = %v, StratumOf says %v", name, got, want)
		}
	}
}

// TestGateConstants pins the four gate constants against §4.4.1. They are
// constants and not config keys on purpose: a knob here switches the gate off
// without anything turning red.
func TestGateConstants(t *testing.T) {
	if MinQuoteRunes != 32 {
		t.Errorf("MinQuoteRunes = %d, want 32", MinQuoteRunes)
	}
	if MinKeepRatio != 0.34 {
		t.Errorf("MinKeepRatio = %v, want 0.34", MinKeepRatio)
	}
	if MinClaimsKept != 3 {
		t.Errorf("MinClaimsKept = %d, want 3", MinClaimsKept)
	}
	if MinSourceCount != 3 {
		t.Errorf("MinSourceCount = %d, want 3", MinSourceCount)
	}
	if ContractVersion != 1 {
		t.Errorf("ContractVersion = %d, want 1", ContractVersion)
	}
}

// TestSensitivityLiterals — derived may not import internal/backends (leaf
// package), so the four level strings are repeated here. This test is what
// makes the repetition safe: a rename in backends/trust.go:20-23 has to break
// something.
func TestSensitivityLiterals(t *testing.T) {
	want := map[string]int{"public": 0, "internal": 1, "personal": 2, "credentials": 3}
	if len(sensitivityRank) != len(want) {
		t.Fatalf("sensitivityRank carries %d levels, want %d", len(sensitivityRank), len(want))
	}
	for level, rank := range want {
		if got, ok := sensitivityRank[level]; !ok || got != rank {
			t.Errorf("sensitivityRank[%q] = %d (present=%v), want %d", level, got, ok, rank)
		}
	}
	if SensitivityCredentials != "credentials" || SensitivityPersonal != "personal" ||
		SensitivityInternal != "internal" || SensitivityPublic != "public" {
		t.Error("a sensitivity literal drifted from backends/trust.go:20-23")
	}
}
