package config

import "testing"

// TestTenantDevmodeKeyContract pins the C6-C registry contract of the tenant
// devmode umbrella flag: it exists, it is a bool defaulting to false (sealing
// stays the default — E4 is not weakened by merely shipping the switch), it is
// hot (an operator flips it without a restart) and it is tenant-overridable —
// the ONLY tenancy class that reaches a tenant generation at all, because
// settings.toOverrides drops a tenant-scope row on a global-only key before it
// ever reaches config.Build.
func TestTenantDevmodeKeyContract(t *testing.T) {
	info, ok := KeyByName("tenant.devmode")
	if !ok {
		t.Fatalf("tenant.devmode is not a registry key")
	}
	if info.Type != "bool" {
		t.Errorf("tenant.devmode type = %q, want bool", info.Type)
	}
	if info.Default != false {
		t.Errorf("tenant.devmode default = %v, want false (sealing is the default)", info.Default)
	}
	if info.EnvVar != "" {
		// Settings-only ON PURPOSE: an env var would let the privacy default
		// fall through a compose edit that leaves no audit row behind.
		t.Errorf("tenant.devmode env = %q, want none (settings-only, so every flip is audited)", info.EnvVar)
	}
	if info.Mutability != "hot" {
		t.Errorf("tenant.devmode mut = %q, want hot", info.Mutability)
	}
	if IsGlobalOnly("tenant.devmode") {
		t.Errorf("tenant.devmode must be tenant-overridable: a global-only key never reaches a tenant generation")
	}
	if info.Desc == "" {
		t.Errorf("tenant.devmode carries no description")
	}
}

// TestTenantDevmodeBuild pins both generations the write path can see: without
// an override the flag is OFF (sealing ships as the default, E4 unchanged), and
// a settings override is what turns it on — the value the tenant overlay
// resolves and hands to SynthesisSettings/dream.Router.
func TestTenantDevmodeBuild(t *testing.T) {
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password")

	base, issues := Build(nil, nil)
	if HasErrors(issues) {
		t.Fatalf("default build has errors: %+v", issues)
	}
	if base.Tenant.Devmode {
		t.Errorf("Tenant.Devmode default = true, want false")
	}
	if got := base.SynthesisSettings().Devmode; got {
		t.Errorf("SynthesisSettings().Devmode default = %v, want false", got)
	}

	on, issues := Build([]Override{{Key: "tenant.devmode", Value: "true", Source: SourceSettings}}, nil)
	if HasErrors(issues) {
		t.Fatalf("override build has errors: %+v", issues)
	}
	if !on.Tenant.Devmode {
		t.Errorf("Tenant.Devmode with override = false, want true")
	}
	if got := on.SynthesisSettings().Devmode; !got {
		t.Errorf("SynthesisSettings().Devmode with override = %v, want true "+
			"(the query path reads the flag through this derivation, not from the struct field)", got)
	}
}
