package handler

import (
	"slices"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
)

// The two-write-worlds scope resolution (03-W5 §4.5) — pure, no DB. A drift here
// is a cross-tenant write-path bug, so it is pinned independently of the
// integration probes.

func TestMutationScope(t *testing.T) {
	op := &auth.AuthResult{IsValid: true, IsAdmin: true, HomeScope: "opscope"}
	if got := mutationScope(op); got != store.GlobalScope {
		t.Errorf("operator mutationScope = %q, want %q", got, store.GlobalScope)
	}
	ten := &auth.AuthResult{IsValid: true, HomeScope: "tenanta", TenantID: "tid-a", TenantRole: auth.RoleAdmin}
	if got := mutationScope(ten); got != "tenanta" {
		t.Errorf("tenant-admin mutationScope = %q, want %q (its OWN scope, never _global)", got, "tenanta")
	}
	if got := mutationScope(nil); got != "" {
		t.Errorf("nil mutationScope = %q, want \"\" (fail-closed)", got)
	}
}

func TestReadScopes(t *testing.T) {
	op := &auth.AuthResult{IsValid: true, IsAdmin: true, HomeScope: "opscope"}
	if got := readScopes(op); !slices.Equal(got, []string{store.GlobalScope}) {
		t.Errorf("operator readScopes = %v, want [_global]", got)
	}
	ten := &auth.AuthResult{IsValid: true, HomeScope: "tenanta", TenantID: "tid-a", TenantRole: auth.RoleAdmin}
	if got := readScopes(ten); !slices.Equal(got, []string{store.GlobalScope, "tenanta"}) {
		t.Errorf("tenant readScopes = %v, want [_global tenanta] (effective tenant>global)", got)
	}
}

func TestResolveScopes(t *testing.T) {
	op := &auth.AuthResult{IsValid: true, IsAdmin: true, HomeScope: "opscope"}
	if got := resolveScopes(op, false); !slices.Equal(got, []string{store.GlobalScope}) {
		t.Errorf("operator resolveScopes = %v, want [_global]", got)
	}
	ten := &auth.AuthResult{IsValid: true, HomeScope: "tenanta", TenantID: "tid-a", TenantRole: auth.RoleAdmin}
	if got := resolveScopes(ten, false); !slices.Equal(got, []string{"tenanta"}) {
		t.Errorf("tenant resolveScopes (no opt-in) = %v, want [tenanta] (strict isolation)", got)
	}
	if got := resolveScopes(ten, true); !slices.Equal(got, []string{"tenanta", store.GlobalScope}) {
		t.Errorf("tenant resolveScopes (opt-in) = %v, want [tenanta _global] (gated fallback)", got)
	}
}

func TestAppliedFromDB(t *testing.T) {
	for src, want := range map[string]bool{
		config.SourceSettings: true,  // operator _global override
		config.SourceTenant:   true,  // tenant-scope override
		"env":                 false, // override would not take effect
		"default":             false,
		"":                    false,
	} {
		if got := appliedFromDB(src); got != want {
			t.Errorf("appliedFromDB(%q) = %v, want %v", src, got, want)
		}
	}
}
