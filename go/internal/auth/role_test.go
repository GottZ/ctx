package auth

import "testing"

// TestRoleDomain pins the Go Role constants to the canonical domain that the
// DB CHECK on context_api_keys.tenant_role carries (059:118-119, K4). The
// authoritative cross-check against the LIVE CHECK constraint lives in
// role_integration_test.go (TestRoleDomain_MatchesDBCheck); this unit guard
// catches a Go-side drift without a database. A stale 2-tier sketch
// ("tenant-admin") would fail here AND fail the CHECK probe.
func TestRoleDomain(t *testing.T) {
	if RoleOwner != "owner" || RoleAdmin != "admin" || RoleMember != "member" {
		t.Fatalf("Role domain drifted from DB CHECK (059): owner=%q admin=%q member=%q", RoleOwner, RoleAdmin, RoleMember)
	}
	// administers(): owner+admin carry tenant-admin authority, member does not,
	// and any unknown/zero value is fail-closed.
	for _, r := range []Role{RoleOwner, RoleAdmin} {
		if !r.administers() {
			t.Errorf("Role %q should administer", r)
		}
	}
	for _, r := range []Role{RoleMember, Role(""), Role("tenant-admin"), Role("root")} {
		if r.administers() {
			t.Errorf("Role %q must NOT administer (fail-closed)", r)
		}
	}
}

// TestIsServerAdmin: the M052 top tier is a valid key with the admin bit.
// nil receiver, invalid key, or a non-admin key are all fail-closed.
func TestIsServerAdmin(t *testing.T) {
	cases := []struct {
		name string
		ar   *AuthResult
		want bool
	}{
		{"nil receiver", nil, false},
		{"valid admin", &AuthResult{IsValid: true, IsAdmin: true}, true},
		{"valid non-admin", &AuthResult{IsValid: true, IsAdmin: false}, false},
		{"invalid but admin bit", &AuthResult{IsValid: false, IsAdmin: true}, false},
		{"zero value", &AuthResult{}, false},
	}
	for _, c := range cases {
		if got := c.ar.IsServerAdmin(); got != c.want {
			t.Errorf("%s: IsServerAdmin() = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestIsTenantAdminOf probes the full tier×tenant lattice. The decisive
// fail-closed cells: an EMPTY target tenant denies even a server-admin (the
// guard stands before the is_admin short-circuit, design/05 §4.2 Rev. 2); a
// member/unknown role never administers; a tenant-admin reaches only its OWN
// tenant; a caller without a tenant is denied.
func TestIsTenantAdminOf(t *testing.T) {
	const (
		tenantA = "00000000-0000-0000-0000-00000000000a"
		tenantB = "00000000-0000-0000-0000-00000000000b"
	)
	serverAdmin := &AuthResult{IsValid: true, IsAdmin: true, TenantID: tenantA, TenantRole: RoleMember}
	ownerA := &AuthResult{IsValid: true, TenantID: tenantA, TenantRole: RoleOwner}
	adminA := &AuthResult{IsValid: true, TenantID: tenantA, TenantRole: RoleAdmin}
	memberA := &AuthResult{IsValid: true, TenantID: tenantA, TenantRole: RoleMember}
	unknownA := &AuthResult{IsValid: true, TenantID: tenantA, TenantRole: Role("tenant-admin")}
	noTenant := &AuthResult{IsValid: true, TenantID: "", TenantRole: RoleAdmin}
	invalidAdmin := &AuthResult{IsValid: false, IsAdmin: true, TenantID: tenantA, TenantRole: RoleOwner}

	cases := []struct {
		name   string
		ar     *AuthResult
		target string
		want   bool
	}{
		{"nil receiver", nil, tenantA, false},
		{"invalid even if admin+owner", invalidAdmin, tenantA, false},

		// server-admin: every NON-EMPTY tenant, but empty target denies it too.
		{"server-admin → tenant A", serverAdmin, tenantA, true},
		{"server-admin → tenant B", serverAdmin, tenantB, true},
		{"server-admin → EMPTY target (guard before is_admin)", serverAdmin, "", false},

		// tenant-admins (owner|admin) administer ONLY their own tenant.
		{"owner A → A", ownerA, tenantA, true},
		{"admin A → A", adminA, tenantA, true},
		{"owner A → B (foreign)", ownerA, tenantB, false},
		{"admin A → B (foreign)", adminA, tenantB, false},
		{"owner A → empty target", ownerA, "", false},

		// member / unknown role / no caller-tenant: fail-closed.
		{"member A → A", memberA, tenantA, false},
		{"unknown role A → A", unknownA, tenantA, false},
		{"caller without tenant → A", noTenant, tenantA, false},
	}
	for _, c := range cases {
		if got := c.ar.IsTenantAdminOf(c.target); got != c.want {
			t.Errorf("%s: IsTenantAdminOf(%q) = %v, want %v", c.name, c.target, got, c.want)
		}
	}
}
