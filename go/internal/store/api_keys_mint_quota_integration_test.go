//go:build integration

// Integration test for Self-Service wave BE6-2 (store.MintKeyWithQuota +
// store.MintOwnerKey).
//
// BE6-2 adds the two mint primitives behind the self-service key-create + owner
// bootstrap paths:
//   - MintKeyWithQuota: transactional, race-gated, quota-capped key mint. It locks
//     the owning context_tenants row FOR UPDATE, counts the tenant's ACTIVE keys
//     under that lock (active-only — S3b, so revoked keys do not permanently burn a
//     max_keys slot), and inserts on the same tx (no TOCTOU). cnt >= *maxKeys →
//     ErrKeyQuotaExceeded; nil/negative maxKeys = unlimited. Self-service calls it
//     with role="member".
//   - MintOwnerKey: NO quota check, NO own transaction — the single sanctioned
//     role='owner' path. q may be a *pgxpool.Pool OR a pgx.Tx, so the tenant-create
//     bootstrap can compose it into its own CreateTenant transaction.
//
// RED (without the new code): the package fails to COMPILE — store.MintKeyWithQuota
// / store.MintOwnerKey / store.ErrKeyQuotaExceeded are undefined. GREEN (after
// api_keys.go): compiles and all sub-cases pass.
//
// Helpers freshTenant (assign_tenant_scope_integration_test.go), seedKeyRole
// (api_keys_update_integration_test.go) and intPtr (rrf_mass_test.go) live in the
// same store_test package and are REUSED, not redeclared; activeKeyCount is local.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run 'TestMintKeyWithQuota|TestMintOwnerKey' -count=1 -v
package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// activeKeyCount returns the number of ACTIVE api keys bound to tenantID — the
// exact quantity MintKeyWithQuota caps against (active-only, S3b).
func activeKeyCount(t *testing.T, pool *pgxpool.Pool, tenantID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_api_keys WHERE tenant_id = $1::uuid AND active = true`, tenantID).Scan(&n); err != nil {
		t.Fatalf("count active keys(%s): %v", tenantID, err)
	}
	return n
}

func TestMintKeyWithQuota_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// N8: minting up to the cap succeeds; the next mint over max_keys →
	// ErrKeyQuotaExceeded. The cap accumulates through the REAL mint path (each
	// success raises the active count the next call observes under the lock).
	t.Run("N8_over_max_keys_returns_ErrKeyQuotaExceeded", func(t *testing.T) {
		tid := freshTenant(t, ctx, pool, "be6w2-cap")
		const max = 2
		for i := 0; i < max; i++ {
			if _, _, err := store.MintKeyWithQuota(ctx, pool,
				"be6w2-cap-k", "private", nil, tid, "member", intPtr(max)); err != nil {
				t.Fatalf("mint %d/%d under cap = %v, want nil", i+1, max, err)
			}
		}
		if _, _, err := store.MintKeyWithQuota(ctx, pool,
			"be6w2-cap-over", "private", nil, tid, "member", intPtr(max)); !errors.Is(err, store.ErrKeyQuotaExceeded) {
			t.Fatalf("mint over cap err = %v, want ErrKeyQuotaExceeded", err)
		}
		if n := activeKeyCount(t, pool, tid); n != max {
			t.Fatalf("active keys after capped mint = %d, want %d (no row leaked past the cap)", n, max)
		}
	})

	// N9-race (the load-bearing S3 property): two goroutines mint into the SAME
	// tenant with max_keys=1. The context_tenants-row FOR UPDATE serialises them —
	// exactly one commits, the other sees the committed count=1 ≥ 1 →
	// ErrKeyQuotaExceeded. Final active-key count = 1 (a per-key/no lock would let
	// both through → 2).
	t.Run("N9_race_max_keys_1_one_winner", func(t *testing.T) {
		tid := freshTenant(t, ctx, pool, "be6w2-race")
		const goroutines = 2
		errs := make([]error, goroutines)

		var wg sync.WaitGroup
		wg.Add(goroutines)
		for i := 0; i < goroutines; i++ {
			go func(i int) {
				defer wg.Done()
				_, _, errs[i] = store.MintKeyWithQuota(ctx, pool,
					"be6w2-race-k", "private", nil, tid, "member", intPtr(1))
			}(i)
		}
		wg.Wait()

		successes, quota := 0, 0
		for i := 0; i < goroutines; i++ {
			switch {
			case errs[i] == nil:
				successes++
			case errors.Is(errs[i], store.ErrKeyQuotaExceeded):
				quota++
			default:
				t.Fatalf("goroutine %d: unexpected err = %v", i, errs[i])
			}
		}
		if successes != 1 || quota != 1 {
			t.Fatalf("race outcome: successes=%d quota=%d, want exactly 1 and 1", successes, quota)
		}
		if n := activeKeyCount(t, pool, tid); n != 1 {
			t.Fatalf("active keys after race = %d, want 1 (cap held under race)", n)
		}
	})

	// N8r (reclaim — the active-only count, S3b): mint to the cap of 1, the second
	// mint is blocked, then REVOKE the first key (active=false via UPDATE, the real
	// soft-delete shape). A subsequent mint now SUCCEEDS because the revoked key no
	// longer counts — a create→revoke→create rotation stays under the cap.
	t.Run("N8r_revoke_reclaims_budget", func(t *testing.T) {
		tid := freshTenant(t, ctx, pool, "be6w2-reclaim")
		first, _, err := store.MintKeyWithQuota(ctx, pool,
			"be6w2-reclaim-k1", "private", nil, tid, "member", intPtr(1))
		if err != nil {
			t.Fatalf("first mint = %v, want nil", err)
		}
		if _, _, err := store.MintKeyWithQuota(ctx, pool,
			"be6w2-reclaim-k2", "private", nil, tid, "member", intPtr(1)); !errors.Is(err, store.ErrKeyQuotaExceeded) {
			t.Fatalf("second mint at cap err = %v, want ErrKeyQuotaExceeded", err)
		}
		// Revoke the first key (soft-delete: active=false) — frees its slot.
		if _, err := pool.Exec(ctx,
			`UPDATE context_api_keys SET active = false WHERE id = $1::uuid`, first.ID); err != nil {
			t.Fatalf("revoke first key: %v", err)
		}
		if _, _, err := store.MintKeyWithQuota(ctx, pool,
			"be6w2-reclaim-k3", "private", nil, tid, "member", intPtr(1)); err != nil {
			t.Fatalf("mint after revoke = %v, want nil (active-only count reclaims the slot)", err)
		}
		if n := activeKeyCount(t, pool, tid); n != 1 {
			t.Fatalf("active keys after reclaim = %d, want 1 (revoked one excluded)", n)
		}
	})

	// Unknown / malformed tenant id → ErrTenantNotFound (the FOR UPDATE finds no
	// row), no key inserted — the no-oracle 404 contract, mirroring AssignTenantScope.
	t.Run("unknown_tenant_returns_ErrTenantNotFound", func(t *testing.T) {
		if _, _, err := store.MintKeyWithQuota(ctx, pool,
			"be6w2-orphan", "private", nil,
			"11111111-2222-3333-4444-555566667777", "member", intPtr(5)); !errors.Is(err, store.ErrTenantNotFound) {
			t.Fatalf("unknown tenant err = %v, want ErrTenantNotFound", err)
		}
		if _, _, err := store.MintKeyWithQuota(ctx, pool,
			"be6w2-orphan2", "private", nil,
			"not-a-uuid", "member", intPtr(5)); !errors.Is(err, store.ErrTenantNotFound) {
			t.Fatalf("malformed tenant id err = %v, want ErrTenantNotFound (22P02 no oracle)", err)
		}
	})

	// Unlimited (maxKeys=nil) and negative both skip the cap: many keys all commit.
	t.Run("unlimited_nil_or_negative_skips_cap", func(t *testing.T) {
		tid := freshTenant(t, ctx, pool, "be6w2-unl")
		if _, _, err := store.MintKeyWithQuota(ctx, pool, "be6w2-unl-a", "private", nil, tid, "member", nil); err != nil {
			t.Fatalf("nil-maxKeys mint = %v, want nil", err)
		}
		if _, _, err := store.MintKeyWithQuota(ctx, pool, "be6w2-unl-b", "private", nil, tid, "member", intPtr(-1)); err != nil {
			t.Fatalf("negative-maxKeys mint = %v, want nil", err)
		}
		if n := activeKeyCount(t, pool, tid); n != 2 {
			t.Fatalf("active keys under unlimited = %d, want 2", n)
		}
	})
}

func TestMintOwnerKey_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// MintOwnerKey mints role='owner' and is CAP-FREE: even on a tenant whose
	// max_keys=0 (the value that would wedge MintKeyWithQuota on the first key) the
	// owner bootstrap succeeds — it never consults the cap.
	t.Run("mints_owner_ignoring_max_keys_zero", func(t *testing.T) {
		tid := freshTenant(t, ctx, pool, "be6w2-owner")
		if _, err := pool.Exec(ctx,
			`UPDATE context_tenants SET max_keys = 0 WHERE id = $1::uuid`, tid); err != nil {
			t.Fatalf("set max_keys=0: %v", err)
		}
		key, plaintext, err := store.MintOwnerKey(ctx, pool, "be6w2-owner-k", "private", nil, tid)
		if err != nil {
			t.Fatalf("MintOwnerKey on max_keys=0 tenant = %v, want nil (cap-free)", err)
		}
		if key.TenantRole != "owner" {
			t.Fatalf("MintOwnerKey role = %q, want owner", key.TenantRole)
		}
		if plaintext == "" {
			t.Fatal("MintOwnerKey returned empty plaintext")
		}
		var role string
		if err := pool.QueryRow(ctx,
			`SELECT tenant_role FROM context_api_keys WHERE id = $1::uuid`, key.ID).Scan(&role); err != nil {
			t.Fatalf("read persisted role: %v", err)
		}
		if role != "owner" {
			t.Fatalf("persisted role = %q, want owner", role)
		}
	})

	// Composable in a caller-owned transaction: MintOwnerKey takes a rowQuerier, so
	// the bootstrap can mint the owner key on the SAME tx as the tenant create — the
	// row is visible only after the caller commits (atomicity), and a rollback mints
	// nothing.
	t.Run("composes_into_caller_transaction", func(t *testing.T) {
		tid := freshTenant(t, ctx, pool, "be6w2-owner-tx")

		// (a) Rollback → nothing persists.
		txRoll, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin rollback tx: %v", err)
		}
		rolled, _, err := store.MintOwnerKey(ctx, txRoll, "be6w2-owner-rolled", "private", nil, tid)
		if err != nil {
			t.Fatalf("MintOwnerKey in tx = %v, want nil", err)
		}
		if err := txRoll.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM context_api_keys WHERE id = $1::uuid)`, rolled.ID).Scan(&exists); err != nil {
			t.Fatalf("check rolled-back key: %v", err)
		}
		if exists {
			t.Fatal("owner key survived a rolled-back tx — MintOwnerKey is not composable")
		}

		// (b) Commit → the owner key persists with role='owner'.
		txOK, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin commit tx: %v", err)
		}
		committed, _, err := store.MintOwnerKey(ctx, txOK, "be6w2-owner-committed", "private", nil, tid)
		if err != nil {
			t.Fatalf("MintOwnerKey in commit tx = %v, want nil", err)
		}
		if err := txOK.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		var role string
		if err := pool.QueryRow(ctx,
			`SELECT tenant_role FROM context_api_keys WHERE id = $1::uuid`, committed.ID).Scan(&role); err != nil {
			t.Fatalf("read committed owner key = %v (want it to persist)", err)
		}
		if role != "owner" {
			t.Fatalf("committed owner role = %q, want owner", role)
		}
	})
}
