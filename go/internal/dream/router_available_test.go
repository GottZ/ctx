package dream

import (
	"io"
	"log/slog"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// TestRouterAvailableMatchesChain holds the dream loop's probe against the
// chain it stands for (issue #495, review of the linked PR): available(role)
// reads TrustCeilings without a log line and must answer exactly what
// Chain(role, SensPublic, tenant) answers. The rows are those of
// backends.TestTrustCeilingsMatchChain with the dream role; the second pool
// keeps only rows of public trust, the lowest that serves at all — a probe
// that compared the ceiling with ">" instead of ">=" would stall Dream on it
// while Chain serves.
func TestRouterAvailableMatchesChain(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) // Chain logs every empty chain
	defer slog.SetDefault(prev)

	dream := []string{backends.RoleDream}
	full := []backends.Backend{
		{ID: "g1", Name: "shared-nocred", Scope: backends.GlobalScope, Roles: dream, Enabled: true, Trust: backends.TrustNoCredentials},
		{ID: "g2", Name: "shared-full-disabled", Scope: backends.GlobalScope, Roles: dream, Enabled: false, Trust: backends.TrustFull},
		{ID: "g3", Name: "unscoped-public", Scope: "", Roles: dream, Enabled: true, Trust: backends.TrustPublic},
		{ID: "g4", Name: "shared-full-other-role", Scope: backends.GlobalScope, Roles: []string{backends.RoleDigest}, Enabled: true, Trust: backends.TrustFull},
		{ID: "a", Name: "alpha-full", Scope: "alpha", Roles: dream, Enabled: true, Trust: backends.TrustFull},
		{ID: "b", Name: "beta-full-profiled", Scope: "beta", Roles: dream, Enabled: true, Trust: backends.TrustFull},
		{ID: "c", Name: "gamma-nonpersonal", Scope: "gamma", Roles: dream, Enabled: true, Trust: backends.TrustNonPersonal},
		{ID: "r", Name: "reserved-full", Scope: "_reserved", Roles: dream, Enabled: true, Trust: backends.TrustFull},
		{ID: "d", Name: "delta-public", Scope: "delta", Roles: dream, Enabled: true, Trust: backends.TrustPublic},
		{ID: "z", Name: "zero-trust", Scope: "zeta", Roles: dream, Enabled: true},
	}
	publicOnly := []backends.Backend{full[2], full[8], full[9]} // unscoped-public, delta-public, zero-trust
	tenants := []string{"", backends.GlobalScope, "alpha", "beta", "gamma", "delta", "zeta", "_reserved", "nobody"}

	for _, pool := range []struct {
		name string
		rows []backends.Backend
	}{{"full", full}, {"public_only", publicOnly}, {"empty", nil}} {
		p := backends.NewPool(nil, nil)
		p.SeedSnapshotDisabledByForTest(pool.rows, map[string]string{"b": "eject"})
		served := 0
		for _, tenant := range tenants {
			_, err := p.Chain(backends.RoleDream, backends.SensPublic, tenant)
			got, want := (&Router{Pool: p, Tenant: tenant}).available(backends.RoleDream), err == nil
			if got != want {
				t.Errorf("pool %s, tenant %q: available = %v, Chain serves = %v", pool.name, tenant, got, want)
			}
			if want {
				served++
			}
		}
		// The fixture must make the probe answer yes somewhere, or the
		// comparison above would pass on a probe that always says no.
		if pool.name != "empty" && served == 0 {
			t.Errorf("pool %s: no tenant view serves — the fixture proves nothing", pool.name)
		}
	}
}
