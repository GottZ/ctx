package backends

import "testing"

// 04-W2 (T34): Chain() egress isolation. A tenant-private backend ('<tenant>'
// scope) is visible ONLY to its own tenant; '_global' (and an unscoped/test
// row) is shared. An empty or '_'-reserved caller tenant sees ONLY shared
// backends — never a same-named tenant-private one (fail-closed, design/04
// §5.7). These are negative probes: each one is red against an UNFILTERED
// Chain() (the pre-T34 state), which the VERIFY mutation re-proves.

func tenantBackends() []Backend {
	return []Backend{
		{ID: "g1", Name: "shared-gpu", Scope: GlobalScope, Trust: TrustFull, Roles: []string{RoleSynthesis}, Priority: 100, Enabled: true},
		// tenantB's private external backend at the TOP priority — without the
		// filter it would be the first link in tenantA's chain (R-LEAK7).
		{ID: "b1", Name: "tenantB-cloud", Scope: "tenantB", Trust: TrustNoCredentials, Roles: []string{RoleSynthesis}, Priority: 1000, Enabled: true},
		{ID: "a1", Name: "tenantA-cloud", Scope: "tenantA", Trust: TrustNoCredentials, Roles: []string{RoleSynthesis}, Priority: 500, Enabled: true},
	}
}

func names(chain []Backend) []string {
	out := make([]string, len(chain))
	for i, b := range chain {
		out[i] = b.Name
	}
	return out
}

func has(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func mustChain(t *testing.T, p *Pool, role string, req Sensitivity, g GamingState, tenant string) []string {
	t.Helper()
	chain, err := p.Chain(role, req, g, tenant)
	if err != nil {
		t.Fatalf("Chain(%s,%s,tenant=%q): %v", role, req, tenant, err)
	}
	return names(chain)
}

// §5.1 — the egress-leak probe. tenantA must never see tenantB's private
// external backend, even at priority 1000.
func TestChainTenantFiltersForeignPrivate(t *testing.T) {
	got := mustChain(t, seedPool(tenantBackends()), RoleSynthesis, SensPublic, GamingState{}, "tenantA")
	if has(got, "tenantB-cloud") {
		t.Fatalf("egress leak: tenantA chain contains foreign tenant backend: %v", got)
	}
	if !has(got, "shared-gpu") {
		t.Fatalf("shared _global backend missing for tenantA: %v", got)
	}
	if !has(got, "tenantA-cloud") {
		t.Fatalf("tenantA own private backend missing: %v", got)
	}
}

// A tenant sees its own private backend, in priority order, never a foreign one.
func TestChainTenantSeesOwnPrivate(t *testing.T) {
	got := mustChain(t, seedPool(tenantBackends()), RoleSynthesis, SensPublic, GamingState{}, "tenantB")
	if !has(got, "tenantB-cloud") {
		t.Fatalf("tenantB lost its own backend: %v", got)
	}
	if has(got, "tenantA-cloud") {
		t.Fatalf("tenantB saw tenantA's private backend: %v", got)
	}
	if !has(got, "shared-gpu") {
		t.Fatalf("tenantB lost the shared backend: %v", got)
	}
	if got[0] != "tenantB-cloud" {
		t.Fatalf("priority order broken (own prio-1000 should lead): %v", got)
	}
}

// §5.7 — a '_'-reserved / sentinel caller (e.g. __UNAUTHORIZED__) sees ONLY
// shared backends, never a same-scoped bait backend.
func TestChainSentinelTenantSeesOnlyShared(t *testing.T) {
	fix := append(tenantBackends(), Backend{
		ID: "u1", Name: "unauth-bait", Scope: "__UNAUTHORIZED__",
		Trust: TrustFull, Roles: []string{RoleSynthesis}, Priority: 2000, Enabled: true,
	})
	got := mustChain(t, seedPool(fix), RoleSynthesis, SensPublic, GamingState{}, "__UNAUTHORIZED__")
	if has(got, "unauth-bait") {
		t.Fatalf("sentinel caller matched a _-reserved backend: %v", got)
	}
	if has(got, "tenantA-cloud") || has(got, "tenantB-cloud") {
		t.Fatalf("sentinel caller saw a tenant-private backend: %v", got)
	}
	if !has(got, "shared-gpu") {
		t.Fatalf("sentinel caller lost the shared backend: %v", got)
	}
}

// An empty caller tenant (no resolved identity) sees ONLY shared backends.
func TestChainEmptyTenantSeesOnlyShared(t *testing.T) {
	got := mustChain(t, seedPool(tenantBackends()), RoleSynthesis, SensPublic, GamingState{}, "")
	if has(got, "tenantA-cloud") || has(got, "tenantB-cloud") {
		t.Fatalf("empty caller saw a tenant-private backend: %v", got)
	}
	if !has(got, "shared-gpu") {
		t.Fatalf("empty caller lost the shared backend: %v", got)
	}
}

// Pausability invariant: with only _global backends (today's live state) every
// tenant — and the sentinel/empty caller — gets the identical chain. T34 is
// behavior-neutral until the first tenant-private backend is inserted.
func TestChainSharedGlobalVisibleToEveryTenant(t *testing.T) {
	p := seedPool([]Backend{
		{ID: "g1", Name: "shared-gpu", Scope: GlobalScope, Trust: TrustFull, Roles: []string{RoleSynthesis}, Priority: 100, Enabled: true},
		{ID: "g2", Name: "shared-cpu", Scope: GlobalScope, Trust: TrustFull, Roles: []string{RoleSynthesis}, Priority: 10, Enabled: true},
	})
	for _, tn := range []string{"tenantA", "tenantB", "", "__UNAUTHORIZED__"} {
		got := mustChain(t, p, RoleSynthesis, SensPublic, GamingState{}, tn)
		if len(got) != 2 || got[0] != "shared-gpu" || got[1] != "shared-cpu" {
			t.Fatalf("tenant %q: all-_global chain should be [shared-gpu shared-cpu], got %v", tn, got)
		}
	}
}

// An unscoped (pre-062 / test-seeded) row carries Scope == "" and is treated as
// shared — the DB enforces NOT NULL DEFAULT '_global', so "" is test-only, but
// it must not vanish from any caller's chain (test robustness + backward compat).
func TestChainUnscopedRowIsShared(t *testing.T) {
	p := seedPool([]Backend{
		{ID: "z", Name: "legacy", Scope: "", Trust: TrustFull, Roles: []string{RoleSynthesis}, Priority: 100, Enabled: true},
	})
	for _, tn := range []string{"tenantA", "", "__UNAUTHORIZED__"} {
		if got := mustChain(t, p, RoleSynthesis, SensPublic, GamingState{}, tn); !has(got, "legacy") {
			t.Fatalf("tenant %q: unscoped row should be shared, got %v", tn, got)
		}
	}
}

// visibleTo is the pure predicate the filter case rests on.
func TestVisibleTo(t *testing.T) {
	cases := []struct {
		bScope, tenant string
		want           bool
	}{
		{GlobalScope, "tenantA", true},
		{GlobalScope, "", true},
		{GlobalScope, "__UNAUTHORIZED__", true},
		{"", "tenantA", true},  // unscoped row = shared
		{"", "", true},         // unscoped row, empty caller = still shared
		{"tenantA", "tenantA", true},
		{"tenantA", "tenantB", false},
		{"tenantB", "tenantA", false},
		{"tenantA", "", false},               // empty caller never matches a private scope
		{"tenantA", "__UNAUTHORIZED__", false}, // sentinel caller
		{"tenantA", "_anything", false},       // any _-reserved caller
		{"__UNAUTHORIZED__", "tenantA", false}, // _-reserved (non-global) backend never matches a real tenant
	}
	for _, c := range cases {
		if got := visibleTo(c.bScope, c.tenant); got != c.want {
			t.Errorf("visibleTo(%q,%q)=%v want %v", c.bScope, c.tenant, got, c.want)
		}
	}
}
