package backends

import "testing"

// TestRoleConfiguredForIsTenantScoped (§F-§8): the "is this role in the
// routing table" answer comes from the same tenant view Chain resolves in. A
// tenant-private row carrying a role counts for its own tenant only; shared
// (_global or unscoped) rows count for everyone; an empty or reserved caller
// sees shared rows only. Disabled rows still count (configured ≠ eligible).
// RED against the pool-wide count the callers used before: tenant-b, _global
// and "" all reported rerank/dream-embed configured through tenant-a's rows.
func TestRoleConfiguredForIsTenantScoped(t *testing.T) {
	p := seedPool([]Backend{
		{ID: "g", Name: "shared-embed", Scope: GlobalScope, Roles: []string{RoleEmbed}, Enabled: true},
		{ID: "u", Name: "unscoped-digest", Scope: "", Roles: []string{RoleDigest}, Enabled: false},
		{ID: "a", Name: "tenant-a-embed", Scope: "tenant-a", Roles: []string{RoleEmbed, RoleDreamEmbed, RoleRerank}, Enabled: true},
	})
	cases := []struct {
		role, tenant string
		want         bool
	}{
		{RoleEmbed, "tenant-b", true},
		{RoleEmbed, "", true},
		{RoleDigest, "tenant-b", true}, // unscoped = shared; disabled still configured
		{RoleDigest, "", true},
		{RoleDreamEmbed, "tenant-a", true},
		{RoleRerank, "tenant-a", true},
		{RoleDreamEmbed, "tenant-b", false},
		{RoleRerank, "tenant-b", false},
		{RoleDreamEmbed, GlobalScope, false},
		{RoleDreamEmbed, "", false},
		{RoleDreamEmbed, "_unauthorized_", false},
		{RoleChat, "tenant-a", false},
	}
	for _, c := range cases {
		if got := p.RoleConfiguredFor(c.role, c.tenant); got != c.want {
			t.Errorf("RoleConfiguredFor(%q, %q) = %v, want %v", c.role, c.tenant, got, c.want)
		}
	}
}
