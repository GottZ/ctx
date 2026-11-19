//go:build integration

package store_test

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestScopeOwnerConfigScopes pins the rule of events.backgroundTenantFor as
// ScopeOwnerConfigScopes states it: a scope of the default tenant resolves
// under _global, a scope of any other tenant under that tenant's first owned
// scope in scope order (whichever of its scopes is asked for), and a scope no
// tenant owns is absent.
func TestScopeOwnerConfigScopes(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	tenant, err := store.CreateTenant(ctx, pool, "sown", "sown")
	if err != nil {
		t.Fatal(err)
	}
	// Assigned out of order, so the first owned scope is not the first one
	// assigned.
	for _, s := range []string{"sown:zeta", "sown:alpha", "sown:mid"} {
		if _, err := store.AssignTenantScope(ctx, pool, tenant.ID, s, nil); err != nil {
			t.Fatalf("assign %s: %v", s, err)
		}
	}
	got, err := store.ScopeOwnerConfigScopes(ctx, pool, []string{"private", "shared", "sown:zeta", "sown:mid", "sown:alpha", "nobody"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"private":    store.GlobalScope,
		"shared":     store.GlobalScope,
		"sown:zeta":  "sown:alpha",
		"sown:mid":   "sown:alpha",
		"sown:alpha": "sown:alpha",
	}
	if !maps.Equal(got, want) {
		t.Errorf("ScopeOwnerConfigScopes = %v, want %v", got, want)
	}
	// The scheduler's form of the same rule: TenantScopes ORDER BY scope,
	// first entry.
	owned, err := store.TenantScopes(ctx, pool, tenant.ID)
	if err != nil || len(owned) == 0 || owned[0] != want["sown:zeta"] {
		t.Errorf("TenantScopes = %v (%v), want %q first", owned, err, want["sown:zeta"])
	}

	empty, err := store.ScopeOwnerConfigScopes(ctx, pool, nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("no scopes: %v, %v; want an empty map and no read", empty, err)
	}
}

// TestConfigScopeOwnedScopes: a non-default tenant's configuration scope (its
// first owned scope) maps to every scope it owns; another scope of the same
// tenant, a default-tenant scope and a scope nobody owns are no tenant's
// configuration scope.
func TestConfigScopeOwnedScopes(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	tenant, err := store.CreateTenant(ctx, pool, "cown", "cown")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"cown:zeta", "cown:alpha"} {
		if _, err := store.AssignTenantScope(ctx, pool, tenant.ID, s, nil); err != nil {
			t.Fatalf("assign %s: %v", s, err)
		}
	}
	got, err := store.ConfigScopeOwnedScopes(ctx, pool, []string{"cown:alpha", "cown:zeta", "private", "nobody"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !slices.Equal(got["cown:alpha"], []string{"cown:alpha", "cown:zeta"}) {
		t.Errorf("ConfigScopeOwnedScopes = %v, want only cown:alpha -> [cown:alpha cown:zeta]", got)
	}
}

// TestSettingScopesWithRow names the scopes holding a row on the key, and
// only those.
func TestSettingScopesWithRow(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertSetting(ctx, tx, "pool.scope_sensitivity_floor", "swr:one", []byte(`"{}"`), nil); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertSetting(ctx, tx, "rerank.enabled", "swr:two", []byte(`true`), nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := store.SettingScopesWithRow(ctx, pool, "pool.scope_sensitivity_floor", []string{"swr:one", "swr:two", "swr:none"})
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(got, map[string]bool{"swr:one": true}) {
		t.Errorf("SettingScopesWithRow = %v, want only swr:one", got)
	}
}
