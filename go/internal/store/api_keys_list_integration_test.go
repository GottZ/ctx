//go:build integration

// Integration test for MT wave T23 (Achse 05-A6): tenant-scoped api-key list
// with an explicit active filter (Leak-Pfad L1, design/05 §5.2 + §6.2). Today
// store.ListApiKeys returns EVERY key of EVERY tenant, including soft-deleted
// (inactive) ones — a tenant-admin of A would enumerate tenant-B's keys
// (existence + activity = reconnaissance), and the list grows monotonically
// with revoked keys. ListApiKeys(tenantFilter, activeOnly) scopes the result to
// one tenant for a non-server-admin (server-admin passes "" = all tenants) and
// hides soft-deleted keys unless activeOnly=false is asked for explicitly.
//
// tgTenant / tgMapScope / seedTenantKey live in the same store_test package
// (tenant_grants_crud_/api_keys_delete_integration_test.go) and are reused;
// keyA2 is made inactive through the real soft-delete path.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestListApiKeys -count=1 -v
package store_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestListApiKeys_TenantScopedActive(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	tenantA := tgTenant(t, pool, "list-tenant-a")
	tenantB := tgTenant(t, pool, "list-tenant-b")
	tgMapScope(t, pool, "list-a", tenantA)
	tgMapScope(t, pool, "list-b", tenantB)

	keyA1 := seedTenantKey(t, pool, "list-keyA1", "list-a", tenantA) // active
	keyA2 := seedTenantKey(t, pool, "list-keyA2", "list-a", tenantA) // → inactive below
	keyB1 := seedTenantKey(t, pool, "list-keyB1", "list-b", tenantB) // active
	// Make keyA2 inactive through the real soft-delete (server-admin path).
	if _, err := store.DeleteApiKey(ctx, pool, keyA2, "", true); err != nil {
		t.Fatalf("seed inactive key: %v", err)
	}

	idSet := func(keys []store.ApiKey) map[string]bool {
		m := make(map[string]bool, len(keys))
		for _, k := range keys {
			m[k.ID] = true
		}
		return m
	}

	// non-server-admin (tenant A), default active-only: only A's ACTIVE key.
	t.Run("tenant_scoped_active_only", func(t *testing.T) {
		got, err := store.ListApiKeys(ctx, pool, tenantA, true)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		m := idSet(got)
		if !m[keyA1] {
			t.Error("own active key A1 missing from tenant-A list")
		}
		if m[keyA2] {
			t.Error("soft-deleted key A2 returned under active-only")
		}
		if m[keyB1] {
			t.Error("L1: foreign-tenant key B1 leaked into tenant-A list")
		}
	})

	// tenant A, active_only=false: both A keys (incl. inactive), still no B.
	t.Run("tenant_scoped_include_inactive", func(t *testing.T) {
		got, err := store.ListApiKeys(ctx, pool, tenantA, false)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		m := idSet(got)
		if !m[keyA1] || !m[keyA2] {
			t.Error("active_only=false must include tenant A's inactive key")
		}
		if m[keyB1] {
			t.Error("L1: foreign-tenant key leaked under include-inactive")
		}
	})

	// server-admin (empty filter), default active-only: all tenants' ACTIVE keys.
	t.Run("server_admin_all_tenants_active_only", func(t *testing.T) {
		got, err := store.ListApiKeys(ctx, pool, "", true)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		m := idSet(got)
		if !m[keyA1] || !m[keyB1] {
			t.Error("server-admin must see every tenant's active keys")
		}
		if m[keyA2] {
			t.Error("active-only must hide the soft-deleted key even for server-admin")
		}
	})

	// server-admin, active_only=false: the full set incl. inactive (today's view).
	t.Run("server_admin_all_tenants_include_inactive", func(t *testing.T) {
		got, err := store.ListApiKeys(ctx, pool, "", false)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		m := idSet(got)
		if !m[keyA1] || !m[keyA2] || !m[keyB1] {
			t.Error("full view must include every seeded key")
		}
	})
}
