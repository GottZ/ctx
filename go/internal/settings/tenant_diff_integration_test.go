//go:build integration

// MT3-W4 / T30 sharp two-tenant differential: the WHOLE resolution chain
// (config.Store + the injected settings.TenantOverlay + the DB rows) proving
// that two tenants resolve to DIFFERENT generations from ONE process-global
// store — the property the existing single-tenant probes
// (TestTenantOverlay_Integration, TestStoreOverlayWiring_Integration) cannot
// show. A process-global "one generation wins" regression (the overlay not
// wired, or SnapshotForTenant collapsing to Snapshot) makes A and B equal and
// turns this test RED — see the schärfe note below the assertions.
//
// Run with:
//
//	go test -tags=integration ./internal/settings/ -run TestTenantDiff -count=1 -v
package settings

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestTenantDiff_TwoTenantsResolveDistinctGenerations is the W4 gate's core
// differential proof. It seeds:
//
//   - rerank.blend_weight: _global=0.5, A=0.6, B=0.8 (a tenant-overridable key
//     each tenant overrides to a DIFFERENT value) — the DIFFERENCE arm.
//   - rerank.max_docs: _global=25 ONLY (no tenant row, no cross-field
//     constraint) — the INHERITANCE arm: both A and B must see the _global
//     value, attributed to the _global settings source, NOT SourceTenant.
//
// Then it drives the real machinery: NewStore(base) + SetOverlay(TenantOverlay)
// and SnapshotForTenant for A and B off the SAME store.
func TestTenantDiff_TwoTenantsResolveDistinctGenerations(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	resetEnv(t)

	base, baseIssues := envBuild(t)
	if config.HasErrors(baseIssues) {
		t.Fatalf("env base must validate: %v", baseIssues)
	}

	const (
		diffKey    = "rerank.blend_weight" // tenant-overridable, [0,1], no cross-field constraint
		inheritKey = "rerank.max_docs"     // tenant-overridable int, no cross-field constraint
		tenantA    = "alpha"
		tenantB    = "bravo"
	)

	// Difference arm: a _global default plus a DIFFERENT tenant value each.
	upsertScopeIT(t, pool, diffKey, store.GlobalScope, `0.5`)
	upsertScopeIT(t, pool, diffKey, tenantA, `0.6`)
	upsertScopeIT(t, pool, diffKey, tenantB, `0.8`)
	// Inheritance arm: ONLY a _global row — neither tenant overrides it.
	upsertScopeIT(t, pool, inheritKey, store.GlobalScope, `25`)

	// Fixture guard: the base (env/default, no overlay) must differ from every
	// resolved value, so a pass cannot be "the store returned base for all".
	if base.Rerank.BlendWeight == 0.6 || base.Rerank.BlendWeight == 0.8 {
		t.Fatalf("fixture: base BlendWeight %v collides with a tenant value", base.Rerank.BlendWeight)
	}

	// ONE process-global store, ONE overlay — the multi-tenant invariant under
	// test is that this single store hands out per-tenant generations.
	st := config.NewStore(base)
	st.SetOverlay(TenantOverlay(pool))

	cfgA := st.SnapshotForTenant(ctx, tenantA)
	cfgB := st.SnapshotForTenant(ctx, tenantB)

	// 1. Difference arm: each tenant sees ITS OWN override, and the two differ.
	if cfgA.Rerank.BlendWeight != 0.6 {
		t.Errorf("tenant A: BlendWeight = %v, want 0.6 (its own override)", cfgA.Rerank.BlendWeight)
	}
	if cfgB.Rerank.BlendWeight != 0.8 {
		t.Errorf("tenant B: BlendWeight = %v, want 0.8 (its own override)", cfgB.Rerank.BlendWeight)
	}
	if cfgA.Rerank.BlendWeight == cfgB.Rerank.BlendWeight {
		t.Fatalf("two tenants must resolve to DISTINCT generations: A=%v B=%v "+
			"(a process-global 'one generation wins' regression makes these equal)",
			cfgA.Rerank.BlendWeight, cfgB.Rerank.BlendWeight)
	}

	// 2. Source attribution on the OVERRIDDEN key: SourceTenant for both.
	if cfgA.Source(diffKey) != config.SourceTenant {
		t.Errorf("tenant A: Source(%s) = %q, want %q", diffKey, cfgA.Source(diffKey), config.SourceTenant)
	}
	if cfgB.Source(diffKey) != config.SourceTenant {
		t.Errorf("tenant B: Source(%s) = %q, want %q", diffKey, cfgB.Source(diffKey), config.SourceTenant)
	}

	// 3. Inheritance arm: both tenants see the _global value of the inherit key,
	//    attributed to the _global settings source (NOT SourceTenant). This is
	//    the §10.2 footprint property — a tenant inherits keys it does not set.
	for name, cfg := range map[string]*config.Config{tenantA: cfgA, tenantB: cfgB} {
		if cfg.Rerank.MaxDocs != 25 {
			t.Errorf("tenant %s: MaxDocs = %v, want 25 (inherited _global)", name, cfg.Rerank.MaxDocs)
		}
		if got := cfg.Source(inheritKey); got != config.SourceSettings {
			t.Errorf("tenant %s: Source(%s) = %q, want %q (a _global-won key keeps the settings source)",
				name, inheritKey, got, config.SourceSettings)
		}
	}

	// Schärfe (proof the test really constrains the machinery): this test goes
	// RED under either mutation —
	//   (a) drop the SetOverlay line  ⇒ snapshotForScope returns base for every
	//       scope ⇒ both BlendWeights == base (≠ 0.6/0.8), assertions 1 fail;
	//   (b) reroute SnapshotForTenant to Snapshot() in store.go ⇒ both return the
	//       base generation ⇒ same failure.
	// Verified by file-copy mutation, not committed (see the T30 report).
}

// TestTenantDiff_PrecedenceShuffleRobustOverStore complements the unit-level
// shuffle test (TestToOverridesShuffleIndependence) at the STORE+DB level: the
// tenant value wins regardless of the physical row insert order, because the
// merge materializes scopePriority, not row order. Insert tenant-then-global
// here (the reverse of the natural _global-first order) and the tenant value
// must still win.
func TestTenantDiff_PrecedenceShuffleRobustOverStore(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	resetEnv(t)

	base, baseIssues := envBuild(t)
	if config.HasErrors(baseIssues) {
		t.Fatalf("env base must validate: %v", baseIssues)
	}

	const (
		key    = "rerank.blend_weight"
		tenant = "charlie"
	)
	// Reverse insert order: tenant row FIRST, then _global. A "last/first row
	// wins" merge would pick the wrong one; materialized precedence must not.
	upsertScopeIT(t, pool, key, tenant, `0.7`)
	upsertScopeIT(t, pool, key, store.GlobalScope, `0.3`)

	st := config.NewStore(base)
	st.SetOverlay(TenantOverlay(pool))

	cfg := st.SnapshotForTenant(ctx, tenant)
	if cfg.Rerank.BlendWeight != 0.7 || cfg.Source(key) != config.SourceTenant {
		t.Errorf("tenant must win regardless of insert order: got %v source %q, want 0.7/tenant",
			cfg.Rerank.BlendWeight, cfg.Source(key))
	}
}
