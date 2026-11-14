package backends

import "testing"

// TestTrustCeilingsMatchChain holds TrustCeilings against Chain: for every
// role, tenant view and sensitivity of the fixture the ceiling says "routes"
// exactly when Chain returns a backend. The fixture exercises each gate
// Chain has: a disabled row, a profile-disabled row, a row of another role,
// an unscoped row, tenant-private rows of several trusts, a row in a reserved
// scope (which no tenant can reach) and a row without a trust level.
func TestTrustCeilingsMatchChain(t *testing.T) {
	bs := []Backend{
		{ID: "g1", Name: "shared-nocred", Scope: GlobalScope, Roles: []string{RoleEmbed}, Enabled: true, Trust: TrustNoCredentials},
		{ID: "g2", Name: "shared-full-disabled", Scope: GlobalScope, Roles: []string{RoleEmbed}, Enabled: false, Trust: TrustFull},
		{ID: "g3", Name: "unscoped-public", Scope: "", Roles: []string{RoleEmbed}, Enabled: true, Trust: TrustPublic},
		{ID: "g4", Name: "shared-full-other-role", Scope: GlobalScope, Roles: []string{RoleDigest}, Enabled: true, Trust: TrustFull},
		{ID: "a", Name: "alpha-full", Scope: "alpha", Roles: []string{RoleEmbed}, Enabled: true, Trust: TrustFull},
		{ID: "b", Name: "beta-full-profiled", Scope: "beta", Roles: []string{RoleEmbed}, Enabled: true, Trust: TrustFull},
		{ID: "c", Name: "gamma-nonpersonal", Scope: "gamma", Roles: []string{RoleEmbed}, Enabled: true, Trust: TrustNonPersonal},
		{ID: "r", Name: "reserved-full", Scope: "_reserved", Roles: []string{RoleEmbed}, Enabled: true, Trust: TrustFull},
		{ID: "d", Name: "delta-dream-only", Scope: "delta", Roles: []string{RoleDreamEmbed}, Enabled: true, Trust: TrustPublic},
		{ID: "z", Name: "zero-trust", Scope: "zeta", Roles: []string{RoleEmbed}, Enabled: true},
	}
	p := NewPool(nil, nil)
	p.SeedSnapshotDisabledByForTest(bs, map[string]string{"b": "eject"})
	n := 0
	for _, role := range []string{RoleEmbed, RoleDreamEmbed, RoleDigest, RoleChat} {
		c := p.TrustCeilings(role)
		for _, tenant := range []string{"", GlobalScope, "alpha", "beta", "gamma", "delta", "zeta", "_reserved", "nobody"} {
			for _, s := range SensitivityLevels {
				_, err := p.Chain(role, s, tenant)
				if got, want := s.Rank() <= c.For(tenant), err == nil; got != want {
					t.Errorf("role %s tenant %q level %s: ceiling %d says %v, Chain says %v", role, tenant, s, c.For(tenant), got, want)
				}
				n++
			}
		}
	}
	if n != 144 {
		t.Errorf("checked %d combinations, want 144 (4 roles, 9 tenant views, 4 levels)", n)
	}
	// The fixture must tell the views apart, or the comparison above would
	// pass on a pool where every view has one answer.
	c := p.TrustCeilings(RoleEmbed)
	if c.Shared != TrustNoCredentials.Rank() || c.For("alpha") != TrustFull.Rank() ||
		c.For("beta") != c.Shared || c.For("gamma") != c.Shared || c.For("_reserved") != c.Shared {
		t.Errorf("embed ceilings %+v: want shared no-credentials, alpha full-trust, beta (profile-disabled), gamma (lower than shared) and _reserved at the shared ceiling", c)
	}
}
