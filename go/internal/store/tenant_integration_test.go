//go:build integration

// Integration test for Multi-Tenant wave T04 (store.TenantScopes + the cross-
// tenant read-isolation gate). T04 (Achse 01-T4) adds store/tenant.go:
//   - TenantScopes(tenantID) → the scopes OWNED by a tenant (Modell C lookup on
//     context_tenant_scopes). Distinct from a KEY's read_scopes (ctx_auth, 060):
//     this is the per-TENANT scope set, the data foundation the visibility axis
//     consumes. It does NOT change how visibility works today (design/01 §4.2) —
//     SearchBlocks already gates on `scope = ANY(read_scopes)`; T04 just supplies
//     the per-tenant scope list and proves it never resolves to a foreign scope.
//   - RequireScopes / ErrNoScopes — the fail-closed empty guard (unit-tested in
//     tenant_test.go; wiring it INTO the read paths is the later wave T07).
//
// RED (today, WITHOUT store/tenant.go): the package fails to COMPILE —
// store.TenantScopes / store.RequireScopes / store.ErrNoScopes are undefined.
// That is the honest RED for a pure-Go wave (the API does not exist yet).
// GREEN (after store/tenant.go): compiles and all sub-cases pass.
//
// pgCode, defaultTenantID, seedAPIKey are declared in
// tenants_hybrid_integration_test.go (same store_test package) and reused here —
// NOT redeclared.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestTenantScopes -count=1 -v
package store_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestTenantScopes_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// (1) The default tenant owns EXACTLY the three legacy scopes 059 mapped.
	// TenantScopes is the tenant's owned set (order-independent), NOT the wire-
	// ordered read_scopes — so we assert membership, not position.
	t.Run("default_tenant_owns_legacy_scopes", func(t *testing.T) {
		scopes, err := store.TenantScopes(ctx, pool, defaultTenantID)
		if err != nil {
			t.Fatalf("TenantScopes(default): %v", err)
		}
		want := map[string]bool{"private": true, "work": true, "shared": true}
		if len(scopes) != 3 {
			t.Fatalf("TenantScopes(default) = %v, want 3 {private,work,shared}", scopes)
		}
		for _, s := range scopes {
			if !want[s] {
				t.Errorf("TenantScopes(default) contains unexpected %q", s)
			}
		}
	})

	// (2) An unknown tenant owns NO scopes → []; the caller turns that into a
	// fail-closed error via RequireScopes (the choke point, design/01 §5.0/§5.4).
	t.Run("unknown_tenant_empty_then_requirescopes_rejects", func(t *testing.T) {
		scopes, err := store.TenantScopes(ctx, pool, "11111111-2222-3333-4444-555566667777")
		if err != nil {
			t.Fatalf("TenantScopes(unknown): %v", err)
		}
		if len(scopes) != 0 {
			t.Fatalf("TenantScopes(unknown) = %v, want [] (no scopes)", scopes)
		}
		if err := store.RequireScopes(scopes); err == nil {
			t.Fatal("RequireScopes(empty TenantScopes) = nil, want ErrNoScopes (fail-closed)")
		}

		// An empty tenantID ("no tenant", e.g. an unauthenticated sentinel) must
		// short-circuit to [] — NOT raise 22P02 (''::uuid) and NOT leak scopes.
		empty, err := store.TenantScopes(ctx, pool, "")
		if err != nil {
			t.Fatalf("TenantScopes(\"\") = err %v, want ([], nil) — fail-closed, no 22P02", err)
		}
		if len(empty) != 0 {
			t.Fatalf("TenantScopes(\"\") = %v, want []", empty)
		}
	})

	// (3) Cross-tenant read isolation (the HART T4-gate, design/01 §5.3): two
	// tenants A,B with their own scopes; TenantScopes(A) excludes B's scope, and a
	// block in B's scope is NOT surfaced by SearchBlocks gated on TenantScopes(A).
	// A sanity search gated on TenantScopes(B) DOES surface it — so the negative
	// is real (the block exists and is otherwise visible), not a false negative.
	t.Run("cross_tenant_read_isolation", func(t *testing.T) {
		const bBlockID = "00000000-0000-7000-8000-0000000004b1"
		var tenantA, tenantB string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_tenants (slug, display_name) VALUES ('t04-a','tenant A') RETURNING id::text`).Scan(&tenantA); err != nil {
			t.Fatalf("insert tenant A: %v", err)
		}
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_tenants (slug, display_name) VALUES ('t04-b','tenant B') RETURNING id::text`).Scan(&tenantB); err != nil {
			t.Fatalf("insert tenant B: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ('t04a-scope',$1::uuid),('t04b-scope',$2::uuid)`, tenantA, tenantB); err != nil {
			t.Fatalf("map scopes: %v", err)
		}
		// A block living in B's scope.
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (id, category, title, content, scope)
			 VALUES ($1,'learnings','t04 secret of B','b-only content','t04b-scope')`, bBlockID); err != nil {
			t.Fatalf("seed B block: %v", err)
		}

		aScopes, err := store.TenantScopes(ctx, pool, tenantA)
		if err != nil {
			t.Fatalf("TenantScopes(A): %v", err)
		}
		if len(aScopes) != 1 || aScopes[0] != "t04a-scope" {
			t.Fatalf("TenantScopes(A) = %v, want [t04a-scope]", aScopes)
		}
		for _, s := range aScopes {
			if s == "t04b-scope" {
				t.Fatalf("TenantScopes(A) leaks B's scope %q", s)
			}
		}

		// SearchBlocks gated on A's scopes must NOT surface B's block.
		gatedByA, err := store.SearchBlocks(ctx, pool, "", aScopes, "", nil, 50, true, nil)
		if err != nil {
			t.Fatalf("SearchBlocks(A scopes): %v", err)
		}
		for _, bp := range gatedByA {
			if bp.ID == bBlockID {
				t.Fatal("SearchBlocks gated on TenantScopes(A) surfaced B's block (cross-tenant read leak)")
			}
		}

		// Sanity: gated on B's scopes the block IS found — the negative is real.
		bScopes, err := store.TenantScopes(ctx, pool, tenantB)
		if err != nil {
			t.Fatalf("TenantScopes(B): %v", err)
		}
		gatedByB, err := store.SearchBlocks(ctx, pool, "", bScopes, "", nil, 50, true, nil)
		if err != nil {
			t.Fatalf("SearchBlocks(B scopes): %v", err)
		}
		found := false
		for _, bp := range gatedByB {
			if bp.ID == bBlockID {
				found = true
			}
		}
		if !found {
			t.Fatal("SearchBlocks gated on TenantScopes(B) did NOT find B's block — sanity failed (isolation negative would be a false negative)")
		}
	})
}
