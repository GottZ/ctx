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

// TestDelegates pins Delegates() to OWNER-ONLY — strictly narrower than
// administers() (owner+admin). An admin administers but must NOT delegate
// (no admin→owner self-elevation, no admin-touch of owner keys); member and
// any unknown/zero value are fail-closed. The owner≠admin split is the entire
// point of the predicate, so admin→false is the decisive cell.
func TestDelegates(t *testing.T) {
	cases := []struct {
		role Role
		want bool
	}{
		{RoleOwner, true},
		{RoleAdmin, false}, // administers, but does NOT delegate
		{RoleMember, false},
		{Role(""), false}, // Go zero value — fail-closed
		{Role("tenant-admin"), false},
		{Role("root"), false},
	}
	for _, c := range cases {
		if got := c.role.Delegates(); got != c.want {
			t.Errorf("Role(%q).Delegates() = %v, want %v", c.role, got, c.want)
		}
		// Sanity: delegates ⊆ administers (every delegator also administers).
		if c.role.Delegates() && !c.role.administers() {
			t.Errorf("Role(%q) delegates but does not administer — lattice violated", c.role)
		}
	}
}

// TestValidRole pins ValidRole to the 059 CHECK domain ('owner'|'admin'|
// 'member'). It is the Go-side 400 gate before the 23514 backstop, so it must
// accept exactly the three canonical values and reject everything else —
// including the empty string, case variants, and a stale 2-tier "tenant-admin"
// sketch — fail-closed.
func TestValidRole(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"owner", true},
		{"admin", true},
		{"member", true},
		{"", false},
		{"Owner", false}, // case-sensitive (CHECK is lowercase)
		{"OWNER", false},
		{"tenant-admin", false}, // stale 2-tier sketch
		{"root", false},
		{"server-admin", false}, // server tier is is_admin, not a tenant_role
		{" owner", false},       // no trimming — exact match
	}
	for _, c := range cases {
		if got := ValidRole(c.s); got != c.want {
			t.Errorf("ValidRole(%q) = %v, want %v", c.s, got, c.want)
		}
	}
	// Cross-check: ValidRole must accept exactly the constants the domain pins.
	for _, r := range []Role{RoleOwner, RoleAdmin, RoleMember} {
		if !ValidRole(string(r)) {
			t.Errorf("ValidRole(%q) = false, want true (059 CHECK domain constant)", r)
		}
	}
}
