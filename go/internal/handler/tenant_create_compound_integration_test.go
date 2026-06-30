//go:build integration

// Integration tests for BE6-7 (design/03 §6, design/01 Cross-Doc #3): the COMPOUND
// atomic tenant-create that closes K10 — tenant row + initial auto-prefixed scope +
// owner key in ONE transaction — end to end through HandleManage against a real
// PG18 testcontainer (full 058-069 migration chain).
//
// What is proven here:
//   - the compound 200: tenant created, '<slug>:main' registered in
//     context_tenant_scopes, an owner key (tenant_role='owner') minted, its plaintext
//     returned ONCE in the flat response (TenantCreateResult contract);
//   - the bootstrapped owner key AUTHENTICATES via ctx_auth → tenant_role=owner and
//     read_scopes ⊇ the initial scope (the tenant is no longer inert);
//   - limit seeding is atomic (caps committed alongside the bootstrap);
//   - slug collision → 409;
//   - a LATE failure (the initial scope already globally registered) rolls the WHOLE
//     tx back: the tenant row from step (a) is gone and no orphan key remains — the
//     atomicity proof for a failure AFTER the tenant insert;
//   - tenant-create stays tierServerAdmin (a tenant-admin → 403).
//
// Driven THROUGH HandleManage (not the handler directly) so the tier gate is
// actually exercised. Reuses the be5* / tl* helpers (scope_create_handler_test.go,
// tenant_limits_integration_test.go — same integration build).
//
//	go test -tags=integration ./internal/handler/ -run TestTenantCreateCompound -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/testdb"
)

// tccField pulls one top-level string field out of a compound tenant-create body.
func tccField(t *testing.T, rec *httptest.ResponseRecorder, key string) string {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal %q: %v", rec.Body.String(), err)
	}
	s, _ := resp[key].(string)
	return s
}

func TestTenantCreateCompound_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil)
	ctx := context.Background()

	// The headline: a single tenant-create yields a working, authenticatable owner.
	t.Run("BootstrapsScopeAndAuthenticatableOwnerKey", func(t *testing.T) {
		rec := be5ScopeManageAs(t, h, adminAR(), map[string]any{
			"action": "tenant-create",
			"data":   map[string]any{"slug": "tcc-acme", "display_name": "Acme Corp"},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("compound create: status %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		// Flat compound response shape (frozen FE TenantCreateResult).
		if got := tccField(t, rec, "scope"); got != "tcc-acme:main" {
			t.Fatalf("scope = %q, want tcc-acme:main (body=%s)", got, rec.Body.String())
		}
		ownerKeyID := tccField(t, rec, "owner_key_id")
		plaintext := tccField(t, rec, "owner_key")
		if ownerKeyID == "" || plaintext == "" {
			t.Fatalf("owner_key_id=%q owner_key(len)=%d, want both non-empty (body=%s)", ownerKeyID, len(plaintext), rec.Body.String())
		}
		tn := tlTenant(t, rec) // reuse the limits-test helper to pull "tenant"
		tenantID, _ := tn["id"].(string)
		if tenantID == "" || tn["slug"] != "tcc-acme" {
			t.Fatalf("tenant = %+v, want non-empty id + slug tcc-acme", tn)
		}

		// (1) The initial scope is registered to THIS tenant.
		var scopeTenant string
		if err := pool.QueryRow(ctx,
			`SELECT tenant_id::text FROM context_tenant_scopes WHERE scope = $1`, "tcc-acme:main").Scan(&scopeTenant); err != nil {
			t.Fatalf("scope lookup: %v", err)
		}
		if scopeTenant != tenantID {
			t.Fatalf("scope owner = %s, want %s", scopeTenant, tenantID)
		}

		// (2) The owner key row carries tenant_role='owner' + the initial home_scope.
		var role, home string
		if err := pool.QueryRow(ctx,
			`SELECT tenant_role, home_scope FROM context_api_keys WHERE id = $1::uuid`, ownerKeyID).Scan(&role, &home); err != nil {
			t.Fatalf("owner key lookup: %v", err)
		}
		if role != "owner" || home != "tcc-acme:main" {
			t.Fatalf("owner key role/home = %q/%q, want owner/tcc-acme:main", role, home)
		}

		// (3) AUTH ROUNDTRIP — the bootstrapped key authenticates via ctx_auth, so the
		// tenant is no longer inert (K10 closed): role=owner, read_scopes ⊇ initial scope.
		ar, err := auth.Authenticate(ctx, pool, plaintext)
		if err != nil {
			t.Fatalf("Authenticate(owner plaintext): %v", err)
		}
		if !ar.IsValid {
			t.Fatalf("owner key did not authenticate (IsValid=false) — tenant still inert (K10)")
		}
		if ar.TenantRole != auth.RoleOwner {
			t.Fatalf("authenticated tenant_role = %q, want owner", ar.TenantRole)
		}
		if ar.TenantID != tenantID {
			t.Fatalf("authenticated tenant_id = %q, want %q", ar.TenantID, tenantID)
		}
		if !slices.Contains(ar.ReadScopes, "tcc-acme:main") {
			t.Fatalf("read_scopes = %v, want it to contain tcc-acme:main", ar.ReadScopes)
		}
	})

	// Limit seeding shares the bootstrap tx (atomic): the caps echo AND the owner
	// key + scope are still produced.
	t.Run("LimitSeedingIsAtomic", func(t *testing.T) {
		rec := be5ScopeManageAs(t, h, adminAR(), map[string]any{
			"action": "tenant-create",
			"data":   map[string]any{"slug": "tcc-seed", "display_name": "Seeded", "max_scopes": 3, "max_keys": 4},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("create with limits: status %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		tlAssertLimits(t, rec, 3, 4)
		if tccField(t, rec, "scope") != "tcc-seed:main" {
			t.Fatalf("seeded create missing initial scope (body=%s)", rec.Body.String())
		}
		if tccField(t, rec, "owner_key") == "" {
			t.Fatalf("seeded create missing owner_key plaintext (body=%s)", rec.Body.String())
		}
	})

	// Slug collision → 409 (caught at step (a), the early-rollback path).
	t.Run("SlugCollision409", func(t *testing.T) {
		body := map[string]any{
			"action": "tenant-create",
			"data":   map[string]any{"slug": "tcc-dup", "display_name": "First"},
		}
		if rec := be5ScopeManageAs(t, h, adminAR(), body); rec.Code != http.StatusOK {
			t.Fatalf("first create: status %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		rec := be5ScopeManageAs(t, h, adminAR(), body)
		if rec.Code != http.StatusConflict {
			t.Fatalf("duplicate slug: status %d, want 409 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	// THE ATOMICITY PROOF for a LATE failure: pre-register the global scope string
	// 'tcc-roll:main' under a DIFFERENT tenant; then tenant-create slug='tcc-roll'
	// inserts the tenant row (a), but AssignTenantScopeTx (c) hits the global scope
	// PK (23505) → the WHOLE tx rolls back. Proof: NO 'tcc-roll' tenant survives and
	// NO key was minted for it — no half-tenant.
	t.Run("ScopeCollisionRollsBackWholeTenant", func(t *testing.T) {
		other := be5SeedTenant(t, pool, "tcc-other")
		be5SeedScope(t, pool, "tcc-roll:main", other) // squat the would-be initial scope

		rec := be5ScopeManageAs(t, h, adminAR(), map[string]any{
			"action": "tenant-create",
			"data":   map[string]any{"slug": "tcc-roll", "display_name": "Rolled Back"},
		})
		if rec.Code != http.StatusConflict {
			t.Fatalf("scope-collision create: status %d, want 409 (body=%s)", rec.Code, rec.Body.String())
		}
		// (a)'s tenant row MUST be rolled back — no 'tcc-roll' tenant exists.
		var tenants int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_tenants WHERE slug = $1`, "tcc-roll").Scan(&tenants); err != nil {
			t.Fatalf("tenant count: %v", err)
		}
		if tenants != 0 {
			t.Fatalf("HALF-TENANT LEAK: %d 'tcc-roll' tenant(s) survived a rolled-back compound create", tenants)
		}
		// And no owner key was minted under the would-be initial scope (the squatted
		// scope belongs to 'tcc-other' and never carried a key).
		var keys int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_api_keys WHERE home_scope = $1`, "tcc-roll:main").Scan(&keys); err != nil {
			t.Fatalf("key count: %v", err)
		}
		if keys != 0 {
			t.Fatalf("ORPHAN KEY LEAK: %d key(s) on tcc-roll:main after rollback", keys)
		}
	})

	// tenant-create stays tierServerAdmin: a tenant-admin must NOT bootstrap a tenant.
	t.Run("TenantAdmin403_TierUnchanged", func(t *testing.T) {
		some := be5SeedTenant(t, pool, "tcc-tieradmin")
		rec := be5ScopeManageAs(t, h, be5TenantAdmin(some), map[string]any{
			"action": "tenant-create",
			"data":   map[string]any{"slug": "tcc-sneaky", "display_name": "Sneaky"},
		})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("tenant-admin tenant-create: status %d, want 403 (tier=server-admin; body=%s)", rec.Code, rec.Body.String())
		}
	})
}
