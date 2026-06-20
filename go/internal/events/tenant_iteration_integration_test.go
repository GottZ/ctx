//go:build integration

// Integration test for Multi-Tenant wave T13 (06-C6 background tenant
// iteration): backgroundTenantScopes derives — from the authoritative tenant
// register (context_tenants, Achse 01 / T05a) — the list of per-tenant
// config-resolution scopes the scheduler loops over, one SnapshotForTenant per
// scope.
//
// The authoritative source is the EXPLICIT register, NOT "SELECT DISTINCT
// home_scope FROM context_api_keys" (the design's other §4.5 example): the live
// corpus carries >1 active home_scope ({private, work}) but exactly ONE tenant,
// so only the register collapses to the 1-element loop the pausability claim
// (design §3.3) rests on. A home_scope iteration would double-run digest/daily
// synthesis. (T13-Lehre, verified against the live DB.)
//
// RED (without the scheduler.backgroundTenantScopes addition): the package fails
// to COMPILE — backgroundTenantScopes is undefined (the honest red for a
// new-symbol wave). GREEN: compiles and all sub-cases pass.
//
// Run with:
//
//	go test -tags=integration ./internal/events/ -run TestBackgroundTenantScopes_Integration -count=1 -v
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

func TestBackgroundTenantScopes_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	s := NewScheduler(pool, config.NewStore(&config.Config{}), backends.NewPool(nil, nil), StartupConfig{})

	// (1) Single-tenant transition: migration 059 seeds exactly the default
	// tenant, so the background iterates ONE scope — _global. The default
	// tenant's config IS the global config, so SnapshotForTenant(_global)
	// short-circuits to base. This is the pausability anchor: a 1-element loop
	// byte-identical to the pre-T13 single pass.
	t.Run("default_only_is_global", func(t *testing.T) {
		got := s.backgroundTenantScopes(ctx)
		if len(got) != 1 || got[0] != store.GlobalScope {
			t.Fatalf("backgroundTenantScopes(default-only) = %v, want [%q]", got, store.GlobalScope)
		}
	})

	// (2) An active non-default tenant contributes its own identity scope (its
	// first owned scope), alongside the default tenant's _global.
	var acmeID string
	t.Run("active_nondefault_contributes_its_scope", func(t *testing.T) {
		acme, err := store.CreateTenant(ctx, pool, "t13-acme", "Acme")
		if err != nil {
			t.Fatalf("CreateTenant: %v", err)
		}
		acmeID = acme.ID
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1, $2::uuid)`,
			"t13-acme-dept", acmeID); err != nil {
			t.Fatalf("map scope: %v", err)
		}
		got := s.backgroundTenantScopes(ctx)
		slices.Sort(got)
		want := []string{store.GlobalScope, "t13-acme-dept"}
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("backgroundTenantScopes(default+acme) = %v, want %v", got, want)
		}
	})

	// (3) test-tenant-bg-exclude (addendum §3.2): a suspended (or offboarding)
	// tenant is dropped from the background iteration — only the default
	// tenant's _global remains.
	t.Run("non_active_excluded", func(t *testing.T) {
		if _, err := store.UpdateTenant(ctx, pool, acmeID, "suspended", ""); err != nil {
			t.Fatalf("UpdateTenant(suspended): %v", err)
		}
		got := s.backgroundTenantScopes(ctx)
		if len(got) != 1 || got[0] != store.GlobalScope {
			t.Fatalf("backgroundTenantScopes(acme suspended) = %v, want [%q] (suspended excluded)", got, store.GlobalScope)
		}
	})
}
