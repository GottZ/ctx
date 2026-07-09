//go:build integration

// Integration test for MT wave T24 (Achse 05-A7): tenant-gated api-key delete
// with a 404-no-oracle (Leak-Pfad L2, design/05 §5.2 + §4.3 #5). Today
// store.DeleteApiKey soft-deletes ANY key by id; in MT a non-server-admin must
// only be able to revoke keys of its OWN tenant — revoking a foreign tenant's
// key is a cross-tenant DoS, and a 403 (vs a silent "not found") would confirm
// that the foreign key exists. The constraint is enforced atomically inside the
// single UPDATE (no fetch-then-write, TOCTOU-free); a miss — wrong tenant,
// absent, already inactive, or a malformed id — collapses to found=false so the
// caller cannot tell them apart.
//
// pgCode / tgTenant / tgMapScope live in the same store_test package
// (tenants_hybrid_/tenant_grants_crud_integration_test.go) and are reused.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestDeleteApiKey -count=1 -v
package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// seedTenantKey inserts a minimal active key bound to an explicit tenant_id
// (store.CreateApiKey would land it in the default tenant via the 059 DEFAULT).
func seedTenantKey(t *testing.T, pool *pgxpool.Pool, keyHash, homeScope, tenantID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`WITH p AS (
		     INSERT INTO context_principals (display_name) VALUES ($2) RETURNING id
		 )
		 INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id, principal_id)
		 SELECT $1, $2, $3, $4::uuid, p.id FROM p RETURNING id::text`,
		keyHash, keyHash, homeScope, tenantID).Scan(&id); err != nil {
		t.Fatalf("seed tenant key %s: %v", keyHash, err)
	}
	return id
}

func keyActive(t *testing.T, pool *pgxpool.Pool, id string) bool {
	t.Helper()
	var active bool
	if err := pool.QueryRow(context.Background(),
		`SELECT active FROM context_api_keys WHERE id = $1::uuid`, id).Scan(&active); err != nil {
		t.Fatalf("read active(%s): %v", id, err)
	}
	return active
}

func TestDeleteApiKey_TenantScoped(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	tenantA := tgTenant(t, pool, "del-tenant-a")
	tenantB := tgTenant(t, pool, "del-tenant-b")
	tgMapScope(t, pool, "del-a", tenantA)
	tgMapScope(t, pool, "del-b", tenantB)

	// L2: a non-server-admin of tenant A must NOT revoke a tenant-B key.
	t.Run("non_server_admin_cannot_delete_foreign_tenant_key", func(t *testing.T) {
		keyB := seedTenantKey(t, pool, "del-keyB-1", "del-b", tenantB)
		deleted, err := store.DeleteApiKey(ctx, pool, keyB, tenantA, false, false)
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		if deleted {
			t.Fatal("tenant-A admin deleted a tenant-B key — L2 cross-tenant DoS")
		}
		if !keyActive(t, pool, keyB) {
			t.Fatal("tenant-B key was silently revoked by a foreign tenant admin")
		}
	})

	// A non-server-admin may revoke its OWN tenant's key.
	t.Run("non_server_admin_deletes_own_tenant_key", func(t *testing.T) {
		keyA := seedTenantKey(t, pool, "del-keyA-1", "del-a", tenantA)
		deleted, err := store.DeleteApiKey(ctx, pool, keyA, tenantA, false, false)
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		if !deleted {
			t.Fatal("tenant-A admin could not delete its OWN tenant's key")
		}
		if keyActive(t, pool, keyA) {
			t.Fatal("own-tenant key still active after delete")
		}
	})

	// A server-admin deletes across tenants exactly as before (behaviorally neutral).
	t.Run("server_admin_deletes_any_tenant_key", func(t *testing.T) {
		keyB := seedTenantKey(t, pool, "del-keyB-2", "del-b", tenantB)
		deleted, err := store.DeleteApiKey(ctx, pool, keyB, tenantA, true, true)
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		if !deleted {
			t.Fatal("server-admin could not delete a foreign-tenant key")
		}
		if keyActive(t, pool, keyB) {
			t.Fatal("server-admin delete did not deactivate the key")
		}
	})

	// No-oracle: a malformed id collapses to not-found (no error, no 500), the
	// same outcome an absent id yields — a malformed vs absent distinction would
	// be an oracle.
	t.Run("malformed_id_is_not_found_no_oracle", func(t *testing.T) {
		deleted, err := store.DeleteApiKey(ctx, pool, "not-a-uuid", tenantA, false, false)
		if err != nil {
			t.Fatalf("malformed id must collapse to not-found, got err: %v", err)
		}
		if deleted {
			t.Fatal("malformed id reported a delete")
		}
		// server-admin path collapses too (empty callerTenant is fine here).
		deleted2, err2 := store.DeleteApiKey(ctx, pool, "also-bad", "", true, true)
		if err2 != nil {
			t.Fatalf("malformed id (server-admin) err: %v", err2)
		}
		if deleted2 {
			t.Fatal("malformed id (server-admin) reported a delete")
		}
	})

	// A well-formed but absent id is a plain miss.
	t.Run("absent_id_is_not_found", func(t *testing.T) {
		deleted, err := store.DeleteApiKey(ctx, pool, "00000000-0000-0000-0000-00000000dead", tenantA, false, false)
		if err != nil {
			t.Fatalf("absent id err: %v", err)
		}
		if deleted {
			t.Fatal("absent id reported a delete")
		}
	})
}

// TestDeleteApiKey_OwnerGuards covers the BE6-4 hardening (design/04 §D-D): the
// Last-Owner + Owner-Protection riegel on the REAL revoke path. api-key-delete is
// tierTenantAdmin, so without these guards an owner could revoke its own last owner
// key (tenant left owner-less) and an admin could revoke all owner keys (owner
// disempowerment). The riegel mirrors UpdateApiKey with the patch fixed to
// active=false. seedKeyRole / keyRoleActive / freshTenant live in the same
// store_test package (api_keys_update_/assign_tenant_scope_integration_test.go).
func TestDeleteApiKey_OwnerGuards(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// N12d: revoking the SOLE active owner via api-key-delete → ErrLastOwner, the
	// key stays active (self-revoke-recovery: a tenant never loses its last owner).
	// callerMayManageOwner=true isolates the last-owner mechanic from owner-protection.
	t.Run("N12d_revoke_last_owner_returns_ErrLastOwner", func(t *testing.T) {
		tid := freshTenant(t, ctx, pool, "del-be6w4-lastowner")
		owner := seedKeyRole(t, pool, "del-be6w4-lastowner-o1", tid, "owner", true)

		deleted, err := store.DeleteApiKey(ctx, pool, owner, tid,
			false /*isServerAdmin*/, true /*callerMayManageOwner*/)
		if !errors.Is(err, store.ErrLastOwner) {
			t.Fatalf("revoke last owner err = %v, want ErrLastOwner", err)
		}
		if deleted {
			t.Fatal("revoke last owner reported deleted=true, want false (rolled back)")
		}
		if !keyActive(t, pool, owner) {
			t.Fatal("last owner key revoked despite ErrLastOwner — tenant left owner-less")
		}
	})

	// N19d: an admin (callerMayManageOwner=false) revoking a NON-last owner key →
	// ErrOwnerProtected (403; AM6(b)). Two owners exist, so it is unambiguously
	// owner-protection, not last-owner (which would be 409). The owner stays active.
	t.Run("N19d_admin_revoke_owner_returns_ErrOwnerProtected", func(t *testing.T) {
		tid := freshTenant(t, ctx, pool, "del-be6w4-ownerprot")
		o1 := seedKeyRole(t, pool, "del-be6w4-ownerprot-o1", tid, "owner", true)
		seedKeyRole(t, pool, "del-be6w4-ownerprot-o2", tid, "owner", true) // second owner → non-last

		deleted, err := store.DeleteApiKey(ctx, pool, o1, tid,
			false /*isServerAdmin*/, false /*callerMayManageOwner = admin*/)
		if !errors.Is(err, store.ErrOwnerProtected) {
			t.Fatalf("admin revoke owner err = %v, want ErrOwnerProtected", err)
		}
		if deleted {
			t.Fatal("admin revoke owner reported deleted=true, want false")
		}
		if !keyActive(t, pool, o1) {
			t.Fatal("admin neutralised an owner key via delete despite ErrOwnerProtected")
		}
	})

	// P-owner: an owner (callerMayManageOwner=true) revoking a NON-last owner key
	// succeeds — the guards block lockout/disempowerment, not a legitimate revoke.
	t.Run("Powner_revoke_non_last_owner_succeeds", func(t *testing.T) {
		tid := freshTenant(t, ctx, pool, "del-be6w4-okrevoke")
		o1 := seedKeyRole(t, pool, "del-be6w4-okrevoke-o1", tid, "owner", true)
		seedKeyRole(t, pool, "del-be6w4-okrevoke-o2", tid, "owner", true) // an owner remains after revoke

		deleted, err := store.DeleteApiKey(ctx, pool, o1, tid,
			false /*isServerAdmin*/, true /*owner caller*/)
		if err != nil || !deleted {
			t.Fatalf("owner revoke non-last owner = (deleted=%v, err=%v), want (true, nil)", deleted, err)
		}
		if keyActive(t, pool, o1) {
			t.Fatal("owner key still active after a permitted revoke")
		}
	})
}
