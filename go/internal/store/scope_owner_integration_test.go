//go:build integration

package store_test

import (
	"context"
	"maps"
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
