package handler

// Unit tests for the re-auth scope-tag recomputation (workflow W9, §4.5). Pure
// function — no DB, runs under -short. The DB/HTTP-driven revoke + grant-revoke
// gates over the real HandleProjectEvents entrypoint live in the integration
// suite (project_events_w9_integration_test.go).

import (
	"slices"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
)

func TestReauthTagsUnfiltered(t *testing.T) {
	// Unfiltered sub: tags follow the fresh read set verbatim (grant-revoke drops
	// a scope; the fan-out only ever emits project-scope frames anyway).
	fresh := &auth.AuthResult{ReadScopes: []string{"a:repo", "b:repo"}}
	got := reauthTags(fresh, "")
	if !slices.Equal(got, []string{"a:repo", "b:repo"}) {
		t.Fatalf("unfiltered tags = %v, want the full read set", got)
	}
	// Grant to b:repo revoked mid-stream (tenant_id/role unchanged).
	fresh2 := &auth.AuthResult{ReadScopes: []string{"a:repo"}}
	got2 := reauthTags(fresh2, "")
	if slices.Contains(got2, "b:repo") {
		t.Fatalf("revoked scope still tagged: %v — grant-revoke not nachgeführt", got2)
	}
}

func TestReauthTagsFilteredGrantRevoke(t *testing.T) {
	// Filtered to b:repo: stays tagged while readable, drops to empty when the
	// grant is revoked (identity unchanged — the pure-identity compare would be
	// blind to this; §4.5 verschärfte gate).
	fresh := &auth.AuthResult{ReadScopes: []string{"a:repo", "b:repo"}}
	if got := reauthTags(fresh, "b:repo"); !slices.Equal(got, []string{"b:repo"}) {
		t.Fatalf("filtered readable tags = %v, want [b:repo]", got)
	}
	revoked := &auth.AuthResult{ReadScopes: []string{"a:repo"}}
	if got := reauthTags(revoked, "b:repo"); len(got) != 0 {
		t.Fatalf("filtered revoked tags = %v, want empty (no frames)", got)
	}
}

func TestReauthTagsFilteredServerAdmin(t *testing.T) {
	// A server-admin filtered to one project keeps that scope even without an
	// explicit grant (admin reads all, §4.6).
	admin := &auth.AuthResult{IsValid: true, IsAdmin: true, ReadScopes: nil}
	if got := reauthTags(admin, "b:repo"); !slices.Equal(got, []string{"b:repo"}) {
		t.Fatalf("admin filtered tags = %v, want [b:repo]", got)
	}
}
