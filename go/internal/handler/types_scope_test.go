package handler

import (
	"slices"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
)

// TestTypeNamespaceIsRequestTenantScope (#112): the namespace a tenant-admin's
// type write lands in, and the namespaces its type reads see, must be the key
// the block-type registry resolves that caller's requests by —
// RequestTenantScope, i.e. the HomeScope, not the tenant UUID. With the two
// keys disjoint, a type created through PUT /api/types never reached
// SnapshotForRequest: the title rule did not classify and an explicit
// "type" answered 422 unknown_type. DB-free twin of
// cmd/ctxd TestTenantTypeReachesRegistry_Integration.
func TestTypeNamespaceIsRequestTenantScope(t *testing.T) {
	const tenantUUID = "01a1a729-9a30-7b57-8fbf-e9e158594dcb"
	owner := &auth.AuthResult{
		IsValid: true, HomeScope: "acme:main", ReadScopes: []string{"acme:main"},
		TenantID: tenantUUID, TenantRole: auth.RoleOwner,
	}

	t.Run("write_scope_is_registry_scope", func(t *testing.T) {
		got, want := typeWriteScope(owner), RequestTenantScope(ctxWithAuthResult(owner))
		if got != want || got != "acme:main" {
			t.Errorf("typeWriteScope = %q, RequestTenantScope = %q, want both %q", got, want, "acme:main")
		}
	})
	t.Run("visible_scopes_carry_registry_scope", func(t *testing.T) {
		if got, want := typeVisibleScopes(owner), []string{store.GlobalScope, "acme:main"}; !slices.Equal(got, want) {
			t.Errorf("typeVisibleScopes = %q, want %q", got, want)
		}
	})
	t.Run("write_gate_follows_home_scope", func(t *testing.T) {
		for _, c := range []struct {
			scope string
			want  bool
		}{
			{"acme:main", true},
			{tenantUUID, false},  // the old key is no longer a write target
			{"acme:other", false}, // another scope of the same tenant is not this key's namespace
			{"beta:main", false},
			{store.GlobalScope, false},
			{"", false},
		} {
			if got := canWriteTypeScope(owner, c.scope); got != c.want {
				t.Errorf("canWriteTypeScope(owner, %q) = %v, want %v", c.scope, got, c.want)
			}
		}
	})
	t.Run("member_cannot_write", func(t *testing.T) {
		member := *owner
		member.TenantRole = auth.RoleMember
		if canWriteTypeScope(&member, "acme:main") {
			t.Error("canWriteTypeScope(member, own home scope) = true, want false")
		}
	})
	t.Run("server_admin_unchanged", func(t *testing.T) {
		admin := &auth.AuthResult{
			IsValid: true, IsAdmin: true, HomeScope: "private",
			TenantID: "00000000-0000-0000-0000-0000000d3fa0", TenantRole: auth.RoleOwner,
		}
		if got := typeWriteScope(admin); got != store.GlobalScope {
			t.Errorf("typeWriteScope(server-admin) = %q, want %q", got, store.GlobalScope)
		}
		if !canWriteTypeScope(admin, store.GlobalScope) || !canWriteTypeScope(admin, "private") {
			t.Error("server-admin must keep write access to '_global' and to the tenant row it can see")
		}
	})
	t.Run("reserved_or_empty_home_scope_fails_closed", func(t *testing.T) {
		for _, hs := range []string{"", store.GlobalScope, "_internal"} {
			ar := *owner
			ar.HomeScope = hs
			if got := typeWriteScope(&ar); got != "" {
				t.Errorf("HomeScope %q: typeWriteScope = %q, want \"\" (fail-closed)", hs, got)
			}
			if got, want := typeVisibleScopes(&ar), []string{store.GlobalScope}; !slices.Equal(got, want) {
				t.Errorf("HomeScope %q: typeVisibleScopes = %q, want %q", hs, got, want)
			}
		}
	})
}
