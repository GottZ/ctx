//go:build integration

// Integration test for Self-Service wave BE5-2 (store.AssignTenantScope).
//
// BE5-2 adds the transactional, race-gated scope-registration store primitive
// behind the self-service scope-create action: it inserts scope → tenant_id into
// context_tenant_scopes under a context_tenants-row FOR UPDATE lock, caps the
// per-tenant scope count against max_scopes inside that lock (no TOCTOU), and maps
// the PK/FK violations to typed errors. The store inserts the scope AS-IS — the
// auto-prefix policy (<slug>:<name>) is the handler's job (BE5-3), not tested here.
//
// RED (without the new code): the package fails to COMPILE — store.AssignTenantScope
// / store.ErrScopeExists / store.ErrScopeQuotaExceeded are undefined (ErrTenantNotFound
// already exists). GREEN (after tenant.go): compiles and all sub-cases pass.
//
// pgCode / defaultTenantID / seedAPIKey live in tenants_hybrid_integration_test.go
// and intPtr in rrf_mass_test.go (same store_test package) — reused, NOT redeclared.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestAssignTenantScope -count=1 -v
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

// freshTenant inserts a key-less tenant and returns its id. Each sub-case uses a
// distinct slug + scope strings because context_tenant_scopes.scope is a GLOBAL PK
// (one fresh container is shared across the sub-tests of this function).
func freshTenant(t *testing.T, ctx context.Context, pool *pgxpool.Pool, slug string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_tenants (slug, display_name) VALUES ($1, $2) RETURNING id::text`, slug, slug).Scan(&id); err != nil {
		t.Fatalf("insert tenant %q: %v", slug, err)
	}
	return id
}

func TestAssignTenantScope_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	newTenant := func(slug string) string { return freshTenant(t, ctx, pool, slug) }

	// (1) Inserting the SAME scope string twice → ErrScopeExists (23505 on the
	// global scope PK). The first assign succeeds (created=true), the second is the
	// 409 path. maxScopes=nil (unlimited) isolates the duplicate-vs-quota outcome.
	t.Run("duplicate_scope_returns_ErrScopeExists", func(t *testing.T) {
		tid := newTenant("be5w1-dup")
		created, err := store.AssignTenantScope(ctx, pool, tid, "be5w1-dup-scope", nil)
		if err != nil || !created {
			t.Fatalf("first assign = (created=%v, err=%v), want (true, nil)", created, err)
		}
		created, err = store.AssignTenantScope(ctx, pool, tid, "be5w1-dup-scope", nil)
		if !errors.Is(err, store.ErrScopeExists) {
			t.Fatalf("duplicate assign err = %v, want ErrScopeExists", err)
		}
		if created {
			t.Fatalf("duplicate assign created = true, want false (no insert happened)")
		}
	})

	// (2) An unknown tenant_id → ErrTenantNotFound: the FOR UPDATE finds no row.
	// A MALFORMED id (22P02 from the ::uuid cast) collapses to the SAME error — no
	// existence oracle that distinguishes "malformed" from "absent" (GetTenant
	// contract). No scope row is ever inserted on this path.
	t.Run("unknown_tenant_returns_ErrTenantNotFound", func(t *testing.T) {
		created, err := store.AssignTenantScope(ctx, pool,
			"11111111-2222-3333-4444-555566667777", "be5w1-orphan-scope", nil)
		if !errors.Is(err, store.ErrTenantNotFound) {
			t.Fatalf("unknown tenant err = %v, want ErrTenantNotFound", err)
		}
		if created {
			t.Fatalf("unknown tenant created = true, want false")
		}

		createdBad, errBad := store.AssignTenantScope(ctx, pool,
			"not-a-uuid", "be5w1-orphan-scope-2", nil)
		if !errors.Is(errBad, store.ErrTenantNotFound) {
			t.Fatalf("malformed id err = %v, want ErrTenantNotFound (22P02 no oracle)", errBad)
		}
		if createdBad {
			t.Fatalf("malformed id created = true, want false")
		}

		// Nothing leaked into the table.
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_tenant_scopes WHERE scope IN ('be5w1-orphan-scope','be5w1-orphan-scope-2')`).Scan(&n); err != nil {
			t.Fatalf("count orphan scopes: %v", err)
		}
		if n != 0 {
			t.Fatalf("orphan scopes inserted = %d, want 0", n)
		}
	})

	// (3) Race gate (the load-bearing S3 property): two goroutines assign DIFFERENT
	// scope names to the SAME tenant with max_scopes=1. The context_tenants-row
	// FOR UPDATE serialises them — exactly one commits (created=true), the other
	// sees the committed count=1 ≥ 1 → ErrScopeQuotaExceeded. Final scope count = 1
	// (a per-scope lock, or a missing lock, would let both through → 2).
	t.Run("race_max_scopes_1_one_winner", func(t *testing.T) {
		tid := newTenant("be5w1-race")
		const goroutines = 2
		names := []string{"be5w1-race-a", "be5w1-race-b"}
		createds := make([]bool, goroutines)
		errs := make([]error, goroutines)

		var wg sync.WaitGroup
		wg.Add(goroutines)
		for i := 0; i < goroutines; i++ {
			go func(i int) {
				defer wg.Done()
				createds[i], errs[i] = store.AssignTenantScope(ctx, pool, tid, names[i], intPtr(1))
			}(i)
		}
		wg.Wait()

		successes, quota := 0, 0
		for i := 0; i < goroutines; i++ {
			switch {
			case errs[i] == nil && createds[i]:
				successes++
			case errors.Is(errs[i], store.ErrScopeQuotaExceeded) && !createds[i]:
				quota++
			default:
				t.Fatalf("goroutine %d: unexpected outcome (created=%v, err=%v)", i, createds[i], errs[i])
			}
		}
		if successes != 1 || quota != 1 {
			t.Fatalf("race outcome: successes=%d quota=%d, want exactly 1 and 1", successes, quota)
		}

		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_tenant_scopes WHERE tenant_id = $1::uuid`, tid).Scan(&n); err != nil {
			t.Fatalf("final scope count: %v", err)
		}
		if n != 1 {
			t.Fatalf("final scope count = %d, want 1 (cap held under race)", n)
		}
	})

	// (4) Unlimited (maxScopes=nil) skips the cap entirely: two distinct scopes both
	// commit. A negative *maxScopes (e.g. -1, the bootstrap value) behaves the same —
	// asserted here so the "nil OR negative = unlimited" branch is both covered.
	t.Run("unlimited_nil_or_negative_both_commit", func(t *testing.T) {
		tid := newTenant("be5w1-unl")
		if created, err := store.AssignTenantScope(ctx, pool, tid, "be5w1-unl-a", nil); err != nil || !created {
			t.Fatalf("nil-maxScopes assign A = (created=%v, err=%v), want (true, nil)", created, err)
		}
		if created, err := store.AssignTenantScope(ctx, pool, tid, "be5w1-unl-b", intPtr(-1)); err != nil || !created {
			t.Fatalf("negative-maxScopes assign B = (created=%v, err=%v), want (true, nil)", created, err)
		}

		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_tenant_scopes WHERE tenant_id = $1::uuid`, tid).Scan(&n); err != nil {
			t.Fatalf("count unlimited scopes: %v", err)
		}
		if n != 2 {
			t.Fatalf("unlimited scope count = %d, want 2", n)
		}
	})
}
