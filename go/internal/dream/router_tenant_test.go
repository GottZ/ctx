package dream

import (
	"slices"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// T38 (04-W6) §4.4-(b): the background dream router threads the iterated
// tenant into every Pool.Chain it resolves (available / chat / EmbedChain),
// so a per-tenant dream run sees only '_global' ∪ that tenant's backends — its
// OWN private backend becomes reachable, a FOREIGN tenant-private one never is.

func tenantEmbedRow(name, scope string, prio int) backends.Backend {
	return backends.Backend{
		ID: "embed-" + name, Name: name, Host: "http://" + name + ".example",
		Protocol: backends.ProtocolOllama, Trust: backends.TrustFull, Locality: "lan",
		Roles:    []string{backends.RoleEmbed},
		ModelMap: map[string]backends.ModelSpec{"default": {Model: "model-" + name}},
		Priority: prio, Enabled: true, Scope: scope,
	}
}

func tenantDreamRow(name, scope string, prio int) backends.Backend {
	b := tenantEmbedRow(name, scope, prio)
	b.Roles = []string{backends.RoleDream}
	return b
}

// chainNames resolves the embed chain through the router's REAL EmbedChain (one
// of the three Pool.Chain call-sites) and returns the backend names in attempt
// order — empty on an empty/error chain.
func chainNames(t *testing.T, r *Router) []string {
	t.Helper()
	chain, _, err := r.EmbedChain(backends.SensPublic)
	if err != nil {
		return nil
	}
	names := make([]string, len(chain))
	for i := range chain {
		names[i] = chain[i].Name
	}
	return names
}

// TestRouterEmbedChainIsTenantScoped pins the EmbedChain egress gate. The pool
// holds a shared '_global' embed (prio 50), the iterated tenant-a's private
// embed (prio 100) and a FOREIGN tenant-c private embed (prio 1000, highest).
//
//   - Tenant "tenant-a": the chain's top is tenant-a's own backend (now visible
//     and outranking the shared one); the foreign tenant-c backend is absent.
//     RED arm — the pre-T38 hardcoded "" (Router.Tenant zero) — would make
//     tenant-a's backend invisible, so the top would be the shared backend.
//   - Tenant "_global" (the default tenant / fail-closed reserved scope): only
//     the shared backend is visible; NEITHER private backend appears.
func TestRouterEmbedChainIsTenantScoped(t *testing.T) {
	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{
		tenantEmbedRow("shared-embed", backends.GlobalScope, 50),
		tenantEmbedRow("tenant-a-embed", "tenant-a", 100),
		tenantEmbedRow("tenant-c-embed", "tenant-c", 1000), // foreign, highest priority
	})

	// Iterated tenant-a: own backend reachable + top, foreign absent.
	ra := &Router{Pool: p, Tenant: "tenant-a"}
	got := chainNames(t, ra)
	if len(got) == 0 || got[0] != "tenant-a-embed" {
		t.Fatalf("tenant-a embed chain = %v, want tenant-a-embed first (a pre-T38 \"\" tenant would see shared-embed first)", got)
	}
	if slices.Contains(got, "tenant-c-embed") {
		t.Fatalf("tenant-a embed chain = %v, must NOT contain the foreign tenant-c backend (egress isolation)", got)
	}

	// Default / reserved '_global' tenant: shared-only, no private backend.
	rg := &Router{Pool: p, Tenant: backends.GlobalScope}
	gotG := chainNames(t, rg)
	if !slices.Equal(gotG, []string{"shared-embed"}) {
		t.Fatalf("_global embed chain = %v, want only [shared-embed] (fail-closed shared-only view)", gotG)
	}
}

// TestRouterEmbedChainRoleModeIsTenantScoped (§F-§8): the bootstrap split
// (dream-embed when configured, embed otherwise) is decided in the iterated
// tenant's view. The shared row carries embed only; tenant-a's private row
// carries embed AND dream-embed. tenant-a resolves dream-embed (its own row);
// every other view — a foreign tenant, the _global default tenant, the empty
// fail-closed tenant — stays on embed and keeps a chain. RED against the
// pool-wide RoleConfigured: those three flipped to dream-embed and got
// ErrNoEligibleBackend (the background embed backfill parked every block).
func TestRouterEmbedChainRoleModeIsTenantScoped(t *testing.T) {
	p := backends.NewPool(nil, nil)
	ta := tenantEmbedRow("tenant-a-embed", "tenant-a", 100)
	ta.Roles = []string{backends.RoleEmbed, backends.RoleDreamEmbed}
	p.SeedSnapshotForTest([]backends.Backend{
		tenantEmbedRow("shared-embed", backends.GlobalScope, 50), // roles: embed only
		ta,
	})
	for _, tenant := range []string{"tenant-b", backends.GlobalScope, ""} {
		chain, role, err := (&Router{Pool: p, Tenant: tenant}).EmbedChain(backends.SensPublic)
		if err != nil || role != backends.RoleEmbed {
			t.Errorf("tenant %q: role=%s err=%v — another tenant's dream-embed row switched this tenant's embed role", tenant, role, err)
			continue
		}
		if len(chain) != 1 || chain[0].Name != "shared-embed" {
			t.Errorf("tenant %q: chain = %v, want [shared-embed]", tenant, chain)
		}
	}
	chain, role, err := (&Router{Pool: p, Tenant: "tenant-a"}).EmbedChain(backends.SensPublic)
	if err != nil || role != backends.RoleDreamEmbed || len(chain) != 1 || chain[0].Name != "tenant-a-embed" {
		t.Errorf("tenant-a: role=%s err=%v chain=%v, want dream-embed on its own row", role, err, chain)
	}
}

// TestRouterAvailableIsTenantScoped pins the second Chain call-site (available):
// a dream role served ONLY by tenant-a's private backend is available to the
// tenant-a router but NOT to the shared-only '_global' router. RED arm — the
// pre-T38 hardcoded "" — would report unavailable even for tenant-a.
func TestRouterAvailableIsTenantScoped(t *testing.T) {
	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{
		tenantDreamRow("tenant-a-dream", "tenant-a", 100), // the only dream backend, tenant-private
	})

	if ra := (&Router{Pool: p, Tenant: "tenant-a"}); !ra.available(backends.RoleDream) {
		t.Errorf("tenant-a router: dream role must be available via its own private backend")
	}
	if rg := (&Router{Pool: p, Tenant: backends.GlobalScope}); rg.available(backends.RoleDream) {
		t.Errorf("_global router: tenant-a's private dream backend must be invisible (shared-only view)")
	}
	if re := (&Router{Pool: p, Tenant: ""}); re.available(backends.RoleDream) {
		t.Errorf("empty-tenant router: a tenant-private backend must never be visible (fail-closed)")
	}
}
