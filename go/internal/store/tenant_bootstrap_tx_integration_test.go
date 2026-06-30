//go:build integration

// Integration test for the BE6-7 store tx-variants (K8): CreateTenantTx /
// AssignTenantScopeTx / SetTenantLimitsTx compose with MintOwnerKey into ONE
// caller-owned transaction so the compound tenant-create bootstrap is atomic. This
// pins the tx primitives in ISOLATION (the handler wave drives them end-to-end);
// the existing tenant_crud / assign_tenant_scope tests already cover the pool
// wrappers, which now wrap these same Tx cores.
//
// RED (without the tx-variants): the package fails to COMPILE — store.CreateTenantTx
// / AssignTenantScopeTx / SetTenantLimitsTx are undefined. GREEN: compiles and both
// sub-cases pass.
//
//	go test -tags=integration ./internal/store/ -run TestTenantBootstrapTx -count=1 -v
package store_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestTenantBootstrapTx_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// (1) The atomicity primitive: compose all three tx-variants + MintOwnerKey in
	// ONE tx and roll back EXPLICITLY (no commit). Nothing may survive — the property
	// the compound handler relies on when any step fails (no half-created tenant).
	t.Run("explicit_rollback_discards_all", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		tn, err := store.CreateTenantTx(ctx, tx, "txb-acme", "Acme")
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("CreateTenantTx: %v", err)
		}
		capFree := -1
		if _, err := store.AssignTenantScopeTx(ctx, tx, tn.ID, "txb-acme:main", &capFree); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("AssignTenantScopeTx: %v", err)
		}
		if _, _, err := store.MintOwnerKey(ctx, tx, "txb owner", "txb-acme:main", []string{"txb-acme:main"}, tn.ID); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("MintOwnerKey: %v", err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		// Nothing the tx touched may survive the rollback.
		for _, c := range []struct {
			what, sql, arg string
		}{
			{"tenant", `SELECT count(*) FROM context_tenants WHERE slug = $1`, "txb-acme"},
			{"scope", `SELECT count(*) FROM context_tenant_scopes WHERE scope = $1`, "txb-acme:main"},
			{"owner key", `SELECT count(*) FROM context_api_keys WHERE home_scope = $1`, "txb-acme:main"},
		} {
			var n int
			if err := pool.QueryRow(ctx, c.sql, c.arg).Scan(&n); err != nil {
				t.Fatalf("%s count: %v", c.what, err)
			}
			if n != 0 {
				t.Fatalf("%s survived rollback: count=%d (atomicity broken)", c.what, n)
			}
		}
	})

	// (2) The commit path + HARD INVARIANT (b): the cap-free (-1) bootstrap scope is
	// registered EVEN when max_scopes is seeded to 0 first — otherwise the very first
	// scope would 429 and the tenant would be un-bootstrappable. All four rows commit
	// together; the owner key carries tenant_role='owner'.
	t.Run("commit_persists_all_capfree_ignores_zero_cap", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		tn, err := store.CreateTenantTx(ctx, tx, "txb-globex", "Globex")
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("CreateTenantTx: %v", err)
		}
		zero := 0
		if err := store.SetTenantLimitsTx(ctx, tx, tn.ID, &zero, &zero); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("SetTenantLimitsTx(0,0): %v", err)
		}
		// max_scopes=0 is now in effect; the cap-free assign MUST still succeed.
		capFree := -1
		created, err := store.AssignTenantScopeTx(ctx, tx, tn.ID, "txb-globex:main", &capFree)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("cap-free AssignTenantScopeTx under max_scopes=0: %v (invariant b broken)", err)
		}
		if !created {
			_ = tx.Rollback(ctx)
			t.Fatal("cap-free assign reported created=false")
		}
		key, plaintext, err := store.MintOwnerKey(ctx, tx, "txb owner", "txb-globex:main", []string{"txb-globex:main"}, tn.ID)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("MintOwnerKey: %v", err)
		}
		if key.TenantRole != "owner" || plaintext == "" {
			_ = tx.Rollback(ctx)
			t.Fatalf("owner key role=%q plaintext-len=%d, want owner + non-empty", key.TenantRole, len(plaintext))
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		// All persist post-commit; spot-check the owner role landed.
		var role string
		if err := pool.QueryRow(ctx, `SELECT tenant_role FROM context_api_keys WHERE id = $1::uuid`, key.ID).Scan(&role); err != nil {
			t.Fatalf("persisted owner key lookup: %v", err)
		}
		if role != "owner" {
			t.Fatalf("persisted tenant_role = %q, want owner", role)
		}
	})
}
