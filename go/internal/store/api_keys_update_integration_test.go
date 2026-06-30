//go:build integration

// Integration test for Self-Service wave BE6-3 (store.UpdateApiKey).
//
// BE6-3 adds the transactional tenant_role/active mutation primitive behind the
// api-key-update action. It resolves the target key under a tenant constraint
// (foreign/absent/malformed id → no-op, no oracle), then under a context_tenants-
// row FOR UPDATE lock enforces two role-aware guards before a COALESCE UPDATE:
//   - Owner-Protection (AM6(b)): a caller without callerMayManageOwner cannot
//     strip owner power from an owner key → ErrOwnerProtected.
//   - Last-Owner (AM6(a)): a patch that would drop the active-owner count below
//     one → ErrLastOwner, nothing written.
//
// The lock target is the SHARED context_tenants row (NOT a per-key lock), so two
// concurrent demotions of DIFFERENT owner keys of one tenant serialise: the
// second observes the first's committed demotion → exactly one succeeds, >= 1
// owner remains (a per-key lock would let both through → zero owners).
//
// Helpers freshTenant / intPtr live in the same store_test package and are
// reused; rolePtr / activePtr / seedKeyRole / keyRoleActive are local to this file.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestUpdateApiKey -count=1 -v
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

func rolePtr(s string) *string { return &s }
func activePtr(b bool) *bool   { return &b }

// seedKeyRole inserts one key bound to an explicit tenant with an explicit
// tenant_role + active flag (store.CreateApiKey only mints 'member' active keys).
// home_scope is the bare legacy scope 'private' (no FK on home_scope); key_hash
// must be globally unique, so each call passes a distinct hash.
func seedKeyRole(t *testing.T, pool *pgxpool.Pool, keyHash, tenantID, role string, active bool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id, tenant_role, active)
		 VALUES ($1, $2, 'private', $3::uuid, $4, $5) RETURNING id::text`,
		keyHash, keyHash, tenantID, role, active).Scan(&id); err != nil {
		t.Fatalf("seed key %s (role=%s active=%v): %v", keyHash, role, active, err)
	}
	return id
}

func keyRoleActive(t *testing.T, pool *pgxpool.Pool, id string) (string, bool) {
	t.Helper()
	var (
		role   string
		active bool
	)
	if err := pool.QueryRow(context.Background(),
		`SELECT tenant_role, active FROM context_api_keys WHERE id = $1::uuid`, id).Scan(&role, &active); err != nil {
		t.Fatalf("read role/active(%s): %v", id, err)
	}
	return role, active
}

func activeOwnerCount(t *testing.T, pool *pgxpool.Pool, tenantID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_api_keys
		  WHERE tenant_id = $1::uuid AND tenant_role = 'owner' AND active = true`, tenantID).Scan(&n); err != nil {
		t.Fatalf("count active owners(%s): %v", tenantID, err)
	}
	return n
}

func TestUpdateApiKey_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// N12a: an owner (callerMayManageOwner=true) demoting the SOLE active owner of
	// its tenant → ErrLastOwner, nothing written (self-revoke-recovery invariant).
	t.Run("N12a_demote_last_owner_returns_ErrLastOwner", func(t *testing.T) {
		tid := freshTenant(t, ctx, pool, "be6w3-lastdemote")
		owner := seedKeyRole(t, pool, "be6w3-lastdemote-o1", tid, "owner", true)

		changed, err := store.UpdateApiKey(ctx, pool, owner, tid,
			false /*isServerAdmin*/, true /*callerMayManageOwner*/, rolePtr("member"), nil)
		if !errors.Is(err, store.ErrLastOwner) {
			t.Fatalf("demote last owner err = %v, want ErrLastOwner", err)
		}
		if changed {
			t.Fatal("demote last owner reported changed=true, want false (rolled back)")
		}
		if role, active := keyRoleActive(t, pool, owner); role != "owner" || !active {
			t.Fatalf("last owner after blocked demote = (role=%q active=%v), want (owner true)", role, active)
		}
	})

	// N12b: deactivating the sole active owner → ErrLastOwner as well (the active
	// true→false transition is also an active-owner removal).
	t.Run("N12b_deactivate_last_owner_returns_ErrLastOwner", func(t *testing.T) {
		tid := freshTenant(t, ctx, pool, "be6w3-lastdeact")
		owner := seedKeyRole(t, pool, "be6w3-lastdeact-o1", tid, "owner", true)

		changed, err := store.UpdateApiKey(ctx, pool, owner, tid,
			false, true, nil, activePtr(false))
		if !errors.Is(err, store.ErrLastOwner) {
			t.Fatalf("deactivate last owner err = %v, want ErrLastOwner", err)
		}
		if changed {
			t.Fatal("deactivate last owner reported changed=true, want false")
		}
		if _, active := keyRoleActive(t, pool, owner); !active {
			t.Fatal("last owner deactivated despite ErrLastOwner — key no longer active")
		}
	})

	// N12-race (the load-bearing guard): TWO DIFFERENT active owner keys of ONE
	// tenant demoted in parallel. The shared context_tenants-row lock serialises
	// them → exactly one commits, the other sees the committed demotion and gets
	// ErrLastOwner. Final active-owner count = 1 (a per-key lock would yield 0).
	t.Run("N12race_two_owners_parallel_demote_one_survives", func(t *testing.T) {
		tid := freshTenant(t, ctx, pool, "be6w3-race")
		o1 := seedKeyRole(t, pool, "be6w3-race-o1", tid, "owner", true)
		o2 := seedKeyRole(t, pool, "be6w3-race-o2", tid, "owner", true)
		owners := []string{o1, o2}

		const goroutines = 2
		changeds := make([]bool, goroutines)
		errs := make([]error, goroutines)

		var wg sync.WaitGroup
		wg.Add(goroutines)
		for i := 0; i < goroutines; i++ {
			go func(i int) {
				defer wg.Done()
				// server-admin caller (callerMayManageOwner=true) isolates the
				// last-owner mechanic from owner-protection / tenant constraint.
				changeds[i], errs[i] = store.UpdateApiKey(ctx, pool, owners[i], "",
					true /*isServerAdmin*/, true, rolePtr("member"), nil)
			}(i)
		}
		wg.Wait()

		successes, lastOwner := 0, 0
		for i := 0; i < goroutines; i++ {
			switch {
			case errs[i] == nil && changeds[i]:
				successes++
			case errors.Is(errs[i], store.ErrLastOwner) && !changeds[i]:
				lastOwner++
			default:
				t.Fatalf("goroutine %d: unexpected outcome (changed=%v, err=%v)", i, changeds[i], errs[i])
			}
		}
		if successes != 1 || lastOwner != 1 {
			t.Fatalf("race outcome: successes=%d lastOwner=%d, want exactly 1 and 1", successes, lastOwner)
		}
		if n := activeOwnerCount(t, pool, tid); n != 1 {
			t.Fatalf("active owners after race = %d, want 1 (>= 1 owner always survives)", n)
		}
	})

	// N13: a non-server-admin of tenant A patching a tenant-B key → no row matches
	// the tenant constraint → changed=false, nil (no oracle). B's key is untouched.
	t.Run("N13_foreign_tenant_key_is_no_op", func(t *testing.T) {
		tidA := freshTenant(t, ctx, pool, "be6w3-foreign-a")
		tidB := freshTenant(t, ctx, pool, "be6w3-foreign-b")
		keyB := seedKeyRole(t, pool, "be6w3-foreign-bkey", tidB, "admin", true)

		changed, err := store.UpdateApiKey(ctx, pool, keyB, tidA,
			false /*isServerAdmin*/, false /*callerMayManageOwner*/, rolePtr("member"), activePtr(false))
		if err != nil {
			t.Fatalf("foreign-tenant patch err = %v, want nil (no oracle)", err)
		}
		if changed {
			t.Fatal("tenant-A caller patched a tenant-B key — cross-tenant write")
		}
		if role, active := keyRoleActive(t, pool, keyB); role != "admin" || !active {
			t.Fatalf("foreign key after blocked patch = (role=%q active=%v), want (admin true)", role, active)
		}
	})

	// N19: an admin (callerMayManageOwner=false) deactivating a NON-last owner key
	// → ErrOwnerProtected (R-OWNERKEY). Two owners exist, so it is unambiguously
	// owner-protection (403), not last-owner (409). The owner stays active.
	t.Run("N19_admin_deactivate_owner_returns_ErrOwnerProtected", func(t *testing.T) {
		tid := freshTenant(t, ctx, pool, "be6w3-ownerprot")
		o1 := seedKeyRole(t, pool, "be6w3-ownerprot-o1", tid, "owner", true)
		seedKeyRole(t, pool, "be6w3-ownerprot-o2", tid, "owner", true) // second owner → non-last

		changed, err := store.UpdateApiKey(ctx, pool, o1, tid,
			false /*isServerAdmin*/, false /*callerMayManageOwner = admin*/, nil, activePtr(false))
		if !errors.Is(err, store.ErrOwnerProtected) {
			t.Fatalf("admin deactivate owner err = %v, want ErrOwnerProtected", err)
		}
		if changed {
			t.Fatal("admin deactivate owner reported changed=true, want false")
		}
		if _, active := keyRoleActive(t, pool, o1); !active {
			t.Fatal("admin neutralised an owner key despite ErrOwnerProtected")
		}
	})

	// P1: an owner promoting a member → admin → changed=true, role persisted. The
	// member target is not an owner, so neither guard fires.
	t.Run("P1_owner_promotes_member_to_admin", func(t *testing.T) {
		tid := freshTenant(t, ctx, pool, "be6w3-promote")
		seedKeyRole(t, pool, "be6w3-promote-o1", tid, "owner", true) // keeps the tenant owned
		member := seedKeyRole(t, pool, "be6w3-promote-m1", tid, "member", true)

		changed, err := store.UpdateApiKey(ctx, pool, member, tid,
			false /*isServerAdmin*/, true /*owner caller*/, rolePtr("admin"), nil)
		if err != nil || !changed {
			t.Fatalf("promote member→admin = (changed=%v, err=%v), want (true, nil)", changed, err)
		}
		if role, active := keyRoleActive(t, pool, member); role != "admin" || !active {
			t.Fatalf("promoted key = (role=%q active=%v), want (admin true)", role, active)
		}
	})

	// P2: reactivating an inactive key (active false→true) → changed=true. The
	// false→true transition removes no owner power, so no guard blocks it.
	t.Run("P2_reactivate_key", func(t *testing.T) {
		tid := freshTenant(t, ctx, pool, "be6w3-reactivate")
		member := seedKeyRole(t, pool, "be6w3-reactivate-m1", tid, "member", false)

		changed, err := store.UpdateApiKey(ctx, pool, member, "",
			true /*isServerAdmin*/, true, nil, activePtr(true))
		if err != nil || !changed {
			t.Fatalf("reactivate key = (changed=%v, err=%v), want (true, nil)", changed, err)
		}
		if _, active := keyRoleActive(t, pool, member); !active {
			t.Fatal("key not active after reactivation")
		}
	})
}
