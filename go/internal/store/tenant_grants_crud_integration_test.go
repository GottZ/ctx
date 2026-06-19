//go:build integration

// Integration test for MT wave T17 (Achse 02-V4): cross-tenant grant management —
// the store CRUD behind the admin manage-actions (CreateTenantGrant /
// ListTenantGrants / DeleteTenantGrant) plus the END-TO-END friend-tenant effect:
// a grant created through the store widens the grantee's read_scopes at its NEXT
// ctx_auth (060, the resolver unions context_tenant_grants), and a delete removes
// it (design/02 §V4 gate "read wirkt sofort — nächster Auth resolved neu").
//
// T14 (tenant_grants_integration_test.go) already pins the 061 TABLE (FK/uq via
// raw SQL); this pins the STORE layer (typed errors) + the resolver COMPOSITION.
// pgCode/defaultTenantID/seedAPIKey are declared in tenants_hybrid_integration_
// test.go (same store_test package) and reused — not redeclared.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestTenantGrant -count=1 -v
package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func tgTenant(t *testing.T, pool *pgxpool.Pool, slug string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_tenants (slug, display_name) VALUES ($1,$2) RETURNING id::text`, slug, slug).Scan(&id); err != nil {
		t.Fatalf("insert tenant %s: %v", slug, err)
	}
	return id
}

func tgMapScope(t *testing.T, pool *pgxpool.Pool, scope, tenantID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1,$2::uuid)`, scope, tenantID); err != nil {
		t.Fatalf("map scope %s: %v", scope, err)
	}
}

func TestTenantGrantCRUD_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Two tenants: A owns scope "tg-a", B owns "tg-b". B will be granted read on A's.
	tenantA := tgTenant(t, pool, "tg-tenant-a")
	tenantB := tgTenant(t, pool, "tg-tenant-b")
	tgMapScope(t, pool, "tg-a", tenantA)
	tgMapScope(t, pool, "tg-b", tenantB)

	// An admin key id for created_by provenance (any valid api_keys.id).
	adminKey, _, err := store.CreateApiKey(ctx, pool, "tg-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("create admin key: %v", err)
	}

	t.Run("create_list_delete_happy_path", func(t *testing.T) {
		g, err := store.CreateTenantGrant(ctx, pool, tenantB, "tg-a", adminKey.ID)
		if err != nil {
			t.Fatalf("create grant: %v", err)
		}
		if g.ID == "" || g.GranteeTenant != tenantB || g.GrantedScope != "tg-a" {
			t.Fatalf("grant = %+v, want grantee B / scope tg-a", g)
		}
		if g.CreatedBy == nil || *g.CreatedBy != adminKey.ID {
			t.Fatalf("created_by = %v, want admin key id %s (provenance)", g.CreatedBy, adminKey.ID)
		}
		all, err := store.ListTenantGrants(ctx, pool, "")
		if err != nil || len(all) != 1 || all[0].ID != g.ID {
			t.Fatalf("list all = %+v (err %v), want exactly [g]", all, err)
		}
		if byB, _ := store.ListTenantGrants(ctx, pool, tenantB); len(byB) != 1 {
			t.Fatalf("list(grantee=B) = %d, want 1", len(byB))
		}
		if byA, _ := store.ListTenantGrants(ctx, pool, tenantA); len(byA) != 0 {
			t.Fatalf("list(grantee=A) = %d, want 0 (A is the owner, not a grantee)", len(byA))
		}
		if err := store.DeleteTenantGrant(ctx, pool, g.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if after, _ := store.ListTenantGrants(ctx, pool, ""); len(after) != 0 {
			t.Fatalf("after delete list = %d, want 0", len(after))
		}
	})

	t.Run("duplicate_is_conflict", func(t *testing.T) {
		if _, err := store.CreateTenantGrant(ctx, pool, tenantB, "tg-a", adminKey.ID); err != nil {
			t.Fatalf("first create: %v", err)
		}
		defer func() { _, _ = pool.Exec(ctx, `DELETE FROM context_tenant_grants WHERE grantee_tenant=$1::uuid`, tenantB) }()
		_, err := store.CreateTenantGrant(ctx, pool, tenantB, "tg-a", adminKey.ID)
		if !errors.Is(err, store.ErrGrantExists) {
			t.Fatalf("duplicate grant err = %v, want ErrGrantExists (uq_tenant_grant)", err)
		}
	})

	t.Run("unknown_grantee_and_scope_fail_closed", func(t *testing.T) {
		// unregistered grantee tenant → 23503 → ErrGrantUnknownTarget
		if _, err := store.CreateTenantGrant(ctx, pool, "11111111-2222-3333-4444-555566667777", "tg-a", adminKey.ID); !errors.Is(err, store.ErrGrantUnknownTarget) {
			t.Errorf("unregistered grantee err = %v, want ErrGrantUnknownTarget", err)
		}
		// '_global' system scope → granted_scope FK rejects it (not in tenant_scopes)
		// → ErrGrantUnknownTarget. The fail-closed backstop behind the handler check.
		if _, err := store.CreateTenantGrant(ctx, pool, tenantB, "_global", adminKey.ID); !errors.Is(err, store.ErrGrantUnknownTarget) {
			t.Errorf("'_global' scope err = %v, want ErrGrantUnknownTarget (FK fail-closed)", err)
		}
		// unregistered (non-system) scope → same FK rejection
		if _, err := store.CreateTenantGrant(ctx, pool, tenantB, "tg-never-registered", adminKey.ID); !errors.Is(err, store.ErrGrantUnknownTarget) {
			t.Errorf("unregistered scope err = %v, want ErrGrantUnknownTarget", err)
		}
		// malformed grantee uuid → 22P02 → ErrGrantUnknownTarget (no 500)
		if _, err := store.CreateTenantGrant(ctx, pool, "not-a-uuid", "tg-a", adminKey.ID); !errors.Is(err, store.ErrGrantUnknownTarget) {
			t.Errorf("malformed grantee err = %v, want ErrGrantUnknownTarget", err)
		}
	})

	t.Run("delete_miss_and_malformed_are_not_found", func(t *testing.T) {
		if err := store.DeleteTenantGrant(ctx, pool, "99999999-8888-7777-6666-555544443333"); !errors.Is(err, store.ErrGrantNotFound) {
			t.Errorf("delete absent err = %v, want ErrGrantNotFound", err)
		}
		if err := store.DeleteTenantGrant(ctx, pool, "not-a-uuid"); !errors.Is(err, store.ErrGrantNotFound) {
			t.Errorf("delete malformed id err = %v, want ErrGrantNotFound (no malformed-vs-absent oracle)", err)
		}
	})

	// E2E (§V4 gate): the grant widens the grantee's read_scopes at the NEXT auth;
	// the delete removes it. Drives the resolver through ctx_auth, not just the table.
	t.Run("grant_widens_read_scopes_then_delete_removes", func(t *testing.T) {
		key, plaintext, err := store.CreateApiKey(ctx, pool, "tg-b-key", "tg-b", []string{}, "")
		if err != nil {
			t.Fatalf("create B key: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE context_api_keys SET tenant_id=$1::uuid WHERE id=$2::uuid`, tenantB, key.ID); err != nil {
			t.Fatalf("pin key to tenant B: %v", err)
		}
		readScopes := func() []string {
			t.Helper()
			var rs []string
			if err := pool.QueryRow(ctx, `SELECT read_scopes FROM ctx_auth($1)`, plaintext).Scan(&rs); err != nil {
				t.Fatalf("ctx_auth: %v", err)
			}
			return rs
		}
		if before := readScopes(); slices.Contains(before, "tg-a") {
			t.Fatalf("before grant read_scopes=%v already contains tg-a", before)
		}
		g, err := store.CreateTenantGrant(ctx, pool, tenantB, "tg-a", adminKey.ID)
		if err != nil {
			t.Fatalf("create grant: %v", err)
		}
		if after := readScopes(); !slices.Contains(after, "tg-a") {
			t.Fatalf("after grant read_scopes=%v missing tg-a (resolver ignored the grant)", after)
		}
		if err := store.DeleteTenantGrant(ctx, pool, g.ID); err != nil {
			t.Fatalf("delete grant: %v", err)
		}
		if after := readScopes(); slices.Contains(after, "tg-a") {
			t.Fatalf("after delete read_scopes=%v still contains tg-a", after)
		}
	})
}
