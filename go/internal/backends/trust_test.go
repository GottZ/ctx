package backends

import "testing"

// TestTrustMatrix probes ALL 16 explicit trust×sensitivity cells against the
// normative permission matrix (design 03 §2.3) plus the 17th cell: an
// empty/unknown sensitivity acts as credentials (fail-closed) — a zero value
// inside the security gate must never be a silent public downgrade.
func TestTrustMatrix(t *testing.T) {
	cases := []struct {
		trust Trust
		sens  Sensitivity
		want  bool
	}{
		// credentials row: full-trust only
		{TrustFull, SensCredentials, true},
		{TrustNoCredentials, SensCredentials, false},
		{TrustNonPersonal, SensCredentials, false},
		{TrustPublic, SensCredentials, false},
		// personal row: ≥ no-credentials
		{TrustFull, SensPersonal, true},
		{TrustNoCredentials, SensPersonal, true},
		{TrustNonPersonal, SensPersonal, false},
		{TrustPublic, SensPersonal, false},
		// internal row: ≥ non-personal
		{TrustFull, SensInternal, true},
		{TrustNoCredentials, SensInternal, true},
		{TrustNonPersonal, SensInternal, true},
		{TrustPublic, SensInternal, false},
		// public row: everyone
		{TrustFull, SensPublic, true},
		{TrustNoCredentials, SensPublic, true},
		{TrustNonPersonal, SensPublic, true},
		{TrustPublic, SensPublic, true},
	}
	for _, c := range cases {
		if got := c.trust.Allows(c.sens); got != c.want {
			t.Errorf("Allows(%s, %s) = %v, want %v", c.trust, c.sens, got, c.want)
		}
	}

	// Cell 17: empty/unknown sensitivity == credentials in Allows.
	for _, sens := range []Sensitivity{"", "totally-new-level"} {
		if TrustNoCredentials.Allows(sens) {
			t.Errorf("Allows(no-credentials, %q) = true — zero value leaked through the gate", sens)
		}
		if !TrustFull.Allows(sens) {
			t.Errorf("Allows(full-trust, %q) = false — unknown should rank as credentials, not above", sens)
		}
	}

	// Unknown TRUST admits nothing, not even public.
	if Trust("").Allows(SensPublic) || Trust("shiny").Allows(SensPublic) {
		t.Error("unknown trust admitted public content — must be fail-closed")
	}
}

// TestMaxSensitivity covers the operation fold including the 17th-cell rule.
func TestMaxSensitivity(t *testing.T) {
	cases := []struct {
		name  string
		parts []Sensitivity
		want  Sensitivity
	}{
		{"all public", []Sensitivity{SensPublic, SensPublic}, SensPublic},
		{"one personal dominates", []Sensitivity{SensPublic, SensPersonal, SensInternal}, SensPersonal},
		{"credentials dominates", []Sensitivity{SensPublic, SensCredentials}, SensCredentials},
		{"zero value ranks as credentials", []Sensitivity{SensPublic, ""}, SensCredentials},
		{"unknown ranks as credentials", []Sensitivity{SensInternal, "v2-extra"}, SensCredentials},
		{"empty parts list is credentials", nil, SensCredentials},
	}
	for _, c := range cases {
		if got := MaxSensitivity(c.parts...); got != c.want {
			t.Errorf("%s: MaxSensitivity(%v) = %s, want %s", c.name, c.parts, got, c.want)
		}
	}
}

func TestTrustRank(t *testing.T) {
	order := []Trust{TrustPublic, TrustNonPersonal, TrustNoCredentials, TrustFull}
	for i := 1; i < len(order); i++ {
		if order[i].Rank() <= order[i-1].Rank() {
			t.Errorf("Rank(%s)=%d not above Rank(%s)=%d", order[i], order[i].Rank(), order[i-1], order[i-1].Rank())
		}
	}
	if Trust("unknown").Rank() != -1 {
		t.Errorf("unknown trust rank = %d, want -1", Trust("unknown").Rank())
	}
}

func TestValidators(t *testing.T) {
	if !ValidTrust(TrustNoCredentials) || ValidTrust(Trust("nope")) || ValidTrust(Trust("")) {
		t.Error("ValidTrust mismatch")
	}
	if !ValidSensitivity(SensInternal) || ValidSensitivity(Sensitivity("nope")) || ValidSensitivity(Sensitivity("")) {
		t.Error("ValidSensitivity mismatch")
	}
}
