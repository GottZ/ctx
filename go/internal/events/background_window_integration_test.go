//go:build integration

// Integration test for Multi-Tenant wave T38 (04-W6): backgroundTenants resolves,
// per iterated tenant, the OWNED scope set (TenantScopes) the read window is
// clamped to — the data foundation of the entitlement-bounded background path
// (read_scopes ∩ owned). The pure clamp is unit-tested (intersectWindow); this
// pins that owned comes from the real register against a live schema.
//
// AMENDMENT #3 (design Z.342-348): the default tenant's owned MUST be the full
// {private,shared,work} (migration 059 backfill), NOT the DISTINCT home_scope of
// its keys ({private}) — so read_scopes ∩ owned stays byte-identical to the
// legacy global cadence.
//
// Run with:
//
//	go test -tags=integration ./internal/events/ -run TestBackgroundTenants_OwnedResolution_Integration -count=1 -v
package events

import (
	"context"
	"slices"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestBackgroundTenants_OwnedResolution_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	s := NewScheduler(pool, config.NewStore(&config.Config{}), backends.NewPool(nil, nil), StartupConfig{})

	// (1) default tenant: config scope _global, owned = the FULL 059 set
	// {private,shared,work} — the AMENDMENT-#3 invariance ceiling, NOT {private}.
	t.Run("default_owned_is_full_legacy_set", func(t *testing.T) {
		got := s.backgroundTenants(ctx)
		if len(got) != 1 {
			t.Fatalf("backgroundTenants(default-only) = %d entries, want 1", len(got))
		}
		bt := got[0]
		if bt.scope != store.GlobalScope {
			t.Fatalf("default bt.scope = %q, want %q", bt.scope, store.GlobalScope)
		}
		owned := append([]string(nil), bt.owned...)
		slices.Sort(owned)
		if !slices.Equal(owned, []string{"private", "shared", "work"}) {
			t.Fatalf("default bt.owned = %v, want [private shared work] (059 backfill, NOT DISTINCT home_scope)", owned)
		}
		// The clamp at the default tenant is byte-identical to the raw read_scopes.
		win := intersectWindow([]string{"private", "shared", "work"}, bt.owned)
		if !slices.Equal(win, []string{"private", "shared", "work"}) {
			t.Fatalf("default window = %v, want [private shared work] (byte-identical legacy cadence)", win)
		}
	})

	// (2) a non-default active tenant owning two scopes: scope = its first owned
	// (ORDER BY scope), owned = both — the window ceiling that intersects a
	// foreign read_scopes override away.
	t.Run("nondefault_owned_set", func(t *testing.T) {
		acme, err := store.CreateTenant(ctx, pool, "t38-acme", "Acme")
		if err != nil {
			t.Fatalf("CreateTenant: %v", err)
		}
		for _, sc := range []string{"t38-acme-b", "t38-acme-a"} { // insert out of order
			if _, err := pool.Exec(ctx,
				`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1, $2::uuid)`,
				sc, acme.ID); err != nil {
				t.Fatalf("map scope %q: %v", sc, err)
			}
		}

		tenants := s.backgroundTenants(ctx)
		var acmeBT *backgroundTenant
		for i := range tenants {
			if tenants[i].scope == "t38-acme-a" { // first owned, deterministic ORDER BY scope
				acmeBT = &tenants[i]
				break
			}
		}
		if acmeBT == nil {
			t.Fatalf("acme tenant absent from backgroundTenants")
		}
		owned := append([]string(nil), acmeBT.owned...)
		slices.Sort(owned)
		if !slices.Equal(owned, []string{"t38-acme-a", "t38-acme-b"}) {
			t.Fatalf("acme bt.owned = %v, want [t38-acme-a t38-acme-b]", owned)
		}
		// A read_scopes override claiming a FOREIGN scope intersects away.
		win := intersectWindow([]string{"t38-acme-a", "private"}, acmeBT.owned)
		if slices.Contains(win, "private") {
			t.Fatalf("acme window = %v, must NOT contain the foreign 'private' scope (cross-tenant leak)", win)
		}
		if !slices.Equal(win, []string{"t38-acme-a"}) {
			t.Fatalf("acme window = %v, want [t38-acme-a]", win)
		}
	})
}
