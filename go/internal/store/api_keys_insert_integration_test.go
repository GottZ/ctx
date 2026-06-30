//go:build integration

// Integration coverage for the BE6-1 insertApiKeyTx refactor (design/04 §D-B,
// design/03 §2): CreateApiKey is now a thin 6-arg wrapper over the shared
// insertApiKeyTx primitive, whose INSERT/RETURNING additively carry tenant_role
// (additive vs v4.0.1, no migration — the 059 column). This pins the two
// invariants the refactor must not break, end-to-end through the exported
// wrapper (the unexported primitive cannot be reached from store_test):
//
//  1. tenant_role flows to the DB and back: CreateApiKey pins 'member', so the
//     returned record AND the persisted row both read 'member' (previously the
//     RETURNING omitted the column → TenantRole == "").
//  2. R-LEAK5 survives the extraction: a foreign tenant's key with nil
//     allowed_scopes still gets an EMPTY set (no implicit {shared}), while the
//     default tenant still inherits {shared}.
//
// tgTenant / tgMapScope live in the same store_test package
// (tenant_grants_crud_integration_test.go) and are reused.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestCreateApiKey_RoleAndLeak5 -count=1 -v
package store_test

import (
	"context"
	"slices"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestCreateApiKey_RoleAndLeak5(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	dbRole := func(t *testing.T, id string) string {
		t.Helper()
		var got string
		if err := pool.QueryRow(ctx,
			`SELECT tenant_role FROM context_api_keys WHERE id = $1::uuid`, id).Scan(&got); err != nil {
			t.Fatalf("read tenant_role(%s): %v", id, err)
		}
		return got
	}

	// 1. Default tenant: CreateApiKey pins role 'member' (returned + persisted),
	//    and nil allowed_scopes still inherits {shared} (R-LEAK5 default arm).
	t.Run("default_tenant_member_role_and_shared", func(t *testing.T) {
		key, _, err := store.CreateApiKey(ctx, pool, "be61-default", "private", nil, store.DefaultTenantID)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if key.TenantRole != "member" {
			t.Errorf("returned TenantRole = %q, want 'member' (RETURNING now carries the column)", key.TenantRole)
		}
		if got := dbRole(t, key.ID); got != "member" {
			t.Errorf("persisted tenant_role = %q, want 'member'", got)
		}
		if !slices.Equal(key.AllowedScopes, []string{"shared"}) {
			t.Errorf("default-tenant allowed_scopes = %v, want {shared} (R-LEAK5 unchanged)", key.AllowedScopes)
		}
	})

	// 2. Foreign tenant: still 'member', but nil allowed_scopes → EMPTY set
	//    (R-LEAK5 foreign arm: no implicit cross-tenant {shared} read).
	t.Run("foreign_tenant_member_role_no_shared", func(t *testing.T) {
		tenantB := tgTenant(t, pool, "be61-tenant-b")
		tgMapScope(t, pool, "be61-b", tenantB)

		key, _, err := store.CreateApiKey(ctx, pool, "be61-foreign", "be61-b", nil, tenantB)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if key.TenantRole != "member" {
			t.Errorf("returned TenantRole = %q, want 'member'", key.TenantRole)
		}
		if got := dbRole(t, key.ID); got != "member" {
			t.Errorf("persisted tenant_role = %q, want 'member'", got)
		}
		if len(key.AllowedScopes) != 0 {
			t.Errorf("R-LEAK5: foreign-tenant key inherited %v, want empty (no implicit shared)", key.AllowedScopes)
		}
	})
}
