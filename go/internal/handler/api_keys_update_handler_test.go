//go:build integration

// Integration tests for BE6-6 (design/04 §D-C, design/03 §5): the api-key-update
// action — tenant_role delegation + active (de)activation — end to end through
// HandleManage (gate → dispatch → handler → store) against a real PG18
// testcontainer.
//
// Driven THROUGH HandleManage (not the handler directly) so the action-tier gate
// (api-key-update == tierTenantAdmin) is actually exercised: a member is rejected
// at the tier (N17), a tenant-admin clears it and reaches the per-resource
// escalation guards. The §7 Negativ-Proben-Matrix subset proven here:
//
//   - N10  tenant-admin (role=admin) promotes a key to owner → 403 (AM4: role
//     delegation is owner-only; the HANDLER guard fires before the store)
//   - N11  invalid tenant_role value → 400 (the CHECK-domain Go gate, ValidRole)
//   - N17  member → 403 (the TIER proof: api-key-update is tierTenantAdmin)
//   - N12  owner demotes the LAST active owner → 409 (AM6(a), ErrLastOwner)
//   - N19  admin deactivates a (non-last) owner key → 403 (AM6(b), ErrOwnerProtected)
//   - N13  cross-tenant id → 200 {success:false,"key not found"} (no oracle, L2);
//     the foreign key is left untouched
//   - P1   owner promotes member→admin → 200, stored role == admin, response.key
//     carries the new role (frozen FE contract)
//   - P2   owner reactivates an inactive key → 200, stored active == true
//
// Helpers (be5*/be6*) are shared with scope_create_handler_test.go +
// api_key_create_tenant_test.go (same package, same build tag).
//
//	go test -tags=integration ./internal/handler/ -run 'TestApiKeyUpdate' -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/testdb"
)

// be6Owner builds a tenant-OWNER AuthResult (delegates roles, manages owner keys).
func be6Owner(tenantID string) *auth.AuthResult {
	return &auth.AuthResult{IsValid: true, IsAdmin: false, TenantID: tenantID, TenantRole: auth.RoleOwner}
}

// be6SeedKey inserts an api key bound to tenantID with an explicit role/active
// directly (bypassing the handler), so a test can stage a precise owner-set. The
// key_hash is a throwaway unique value (these keys are never authenticated — the
// caller's authority comes from an injected AuthResult).
func be6SeedKey(t *testing.T, pool *pgxpool.Pool, tenantID, homeScope, role string, active bool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_api_keys (label, key_hash, home_scope, allowed_scopes, active, tenant_id, tenant_role)
		 VALUES ($1, gen_random_uuid()::text, $2, '{}', $3, $4::uuid, $5)
		 RETURNING id::text`,
		role+"-key", homeScope, active, tenantID, role).Scan(&id); err != nil {
		t.Fatalf("seed %s key (active=%v) for tenant %s: %v", role, active, tenantID, err)
	}
	return id
}

// be6KeyActive reads the stored active flag of a key (server-side truth).
func be6KeyActive(t *testing.T, pool *pgxpool.Pool, keyID string) bool {
	t.Helper()
	var active bool
	if err := pool.QueryRow(context.Background(),
		`SELECT active FROM context_api_keys WHERE id = $1::uuid`, keyID).Scan(&active); err != nil {
		t.Fatalf("read key active: %v", err)
	}
	return active
}

// be6RespKeyRole pulls response.key.tenant_role out of a 200 api-key-update body
// (the frozen FE contract ApiKeyUpdateResult.key).
func be6RespKeyRole(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var resp struct {
		Success bool `json:"success"`
		Key     struct {
			TenantRole string `json:"tenant_role"`
		} `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal %q: %v", rec.Body.String(), err)
	}
	if !resp.Success {
		t.Fatalf("response not success: %s", rec.Body.String())
	}
	return resp.Key.TenantRole
}

func TestApiKeyUpdate_Handler_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, nil)

	tenantA := be5SeedTenant(t, pool, "be6ku-acme")
	tenantB := be5SeedTenant(t, pool, "be6ku-globex")
	scopeA := "be6ku-acme:keys"
	scopeB := "be6ku-globex:keys"
	be5SeedScope(t, pool, scopeA, tenantA)
	be5SeedScope(t, pool, scopeB, tenantB)

	// N10: a tenant-admin (role=admin) promotes a key to owner → 403. The AM4
	// escalation guard fires in the HANDLER (before the store): role delegation is
	// an owner-only power, so a non-owner admin can never self-elevate to owner.
	t.Run("N10_AdminPromotesToOwner403", func(t *testing.T) {
		keyID := be6SeedKey(t, pool, tenantA, scopeA, "admin", true)
		rec := be5ScopeManageAs(t, h, be5TenantAdmin(tenantA), map[string]any{
			"action": "api-key-update",
			"data":   map[string]any{"id": keyID, "tenant_role": "owner"},
		})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("admin→owner: status %d, want 403 (body=%s)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "role change requires owner") {
			t.Errorf("N10 403 is not the AM4 delegation gate (body=%s)", rec.Body.String())
		}
		if got := be6KeyRole(t, pool, keyID); got != "admin" {
			t.Errorf("key role mutated to %q despite 403, want admin unchanged", got)
		}
	})

	// N11: an out-of-domain tenant_role value → 400 (the ValidRole Go gate in front
	// of the 059 CHECK backstop). Driven as an OWNER so the ONLY rejection under
	// test is the bad value, not the delegation gate.
	t.Run("N11_InvalidRole400", func(t *testing.T) {
		keyID := be6SeedKey(t, pool, tenantA, scopeA, "member", true)
		rec := be5ScopeManageAs(t, h, be6Owner(tenantA), map[string]any{
			"action": "api-key-update",
			"data":   map[string]any{"id": keyID, "tenant_role": "superuser"},
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid role: status %d, want 400 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	// N17: a member of its own tenant → 403 at the tier gate (requireTenantAdmin),
	// before the handler runs. This is the proof that api-key-update is wired into
	// tierTenantAdmin (a mis-tier to tierServerAdmin would also 403 a member, but
	// would break P1/P2 where a tenant-admin/owner must pass — so the matrix as a
	// whole pins the tier).
	t.Run("N17_Member403_Tier", func(t *testing.T) {
		keyID := be6SeedKey(t, pool, tenantA, scopeA, "member", true)
		off := false
		rec := be5ScopeManageAs(t, h, be5Member(tenantA), map[string]any{
			"action": "api-key-update",
			"data":   map[string]any{"id": keyID, "active": off},
		})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("member api-key-update: status %d, want 403 (tier gate; body=%s)", rec.Code, rec.Body.String())
		}
	})

	// N13: a non-server-admin targeting a key in ANOTHER tenant resolves to zero
	// rows under the tenant constraint → 200 {success:false,"key not found"}: no
	// existence oracle (L2), and the foreign key is left untouched.
	t.Run("N13_CrossTenant_NoOracle", func(t *testing.T) {
		foreignKey := be6SeedKey(t, pool, tenantB, scopeB, "member", true)
		off := false
		rec := be5ScopeManageAs(t, h, be6Owner(tenantA), map[string]any{
			"action": "api-key-update",
			"data":   map[string]any{"id": foreignKey, "active": off},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("cross-tenant update: status %d, want 200 (no-oracle; body=%s)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "key not found") {
			t.Errorf("N13 body is not the no-oracle miss: %s", rec.Body.String())
		}
		if !be6KeyActive(t, pool, foreignKey) {
			t.Errorf("N13 mutated a foreign-tenant key (cross-tenant write!)")
		}
	})

	// P1: an owner promotes a member key to admin → 200, the stored role is admin
	// (server-side truth, not just the echo), and the response carries the re-read
	// key with the new role (frozen FE contract ApiKeyUpdateResult.key).
	t.Run("P1_OwnerPromotesMemberToAdmin200", func(t *testing.T) {
		keyID := be6SeedKey(t, pool, tenantA, scopeA, "member", true)
		rec := be5ScopeManageAs(t, h, be6Owner(tenantA), map[string]any{
			"action": "api-key-update",
			"data":   map[string]any{"id": keyID, "tenant_role": "admin"},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("owner promote member→admin: status %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		if got := be6RespKeyRole(t, rec); got != "admin" {
			t.Errorf("response key.tenant_role = %q, want admin (FE contract)", got)
		}
		if got := be6KeyRole(t, pool, keyID); got != "admin" {
			t.Errorf("stored tenant_role = %q, want admin", got)
		}
	})

	// P2: an owner reactivates an inactive key (active false→true) → 200, stored
	// active is true. The re-read uses activeOnly=false, so the row is returned.
	t.Run("P2_OwnerReactivates200", func(t *testing.T) {
		keyID := be6SeedKey(t, pool, tenantA, scopeA, "member", false)
		on := true
		rec := be5ScopeManageAs(t, h, be6Owner(tenantA), map[string]any{
			"action": "api-key-update",
			"data":   map[string]any{"id": keyID, "active": on},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("owner reactivate: status %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		if !be6KeyActive(t, pool, keyID) {
			t.Errorf("stored active = false after reactivate, want true")
		}
	})
}

// TestApiKeyUpdate_LastOwner_Integration (N12) is isolated in its own tenant so
// the owner count is deterministic: exactly ONE active owner. An owner demoting
// that sole owner off 'owner' would orphan the tenant → ErrLastOwner → 409, and
// the role is unchanged (the Self-Revoke-Recovery invariant, D3).
func TestApiKeyUpdate_LastOwner_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, nil)

	tid := be5SeedTenant(t, pool, "be6ku-soleowner")
	scope := "be6ku-soleowner:keys"
	be5SeedScope(t, pool, scope, tid)
	ownerKey := be6SeedKey(t, pool, tid, scope, "owner", true)

	rec := be5ScopeManageAs(t, h, be6Owner(tid), map[string]any{
		"action": "api-key-update",
		"data":   map[string]any{"id": ownerKey, "tenant_role": "member"},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("demote last owner: status %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "last active owner") {
		t.Errorf("N12 409 is not the last-owner guard (body=%s)", rec.Body.String())
	}
	if got := be6KeyRole(t, pool, ownerKey); got != "owner" {
		t.Errorf("last owner demoted to %q despite 409, want owner unchanged", got)
	}
}

// TestApiKeyUpdate_OwnerProtection_Integration (N19) stages TWO active owners (so
// the target is NOT the last owner) and has an ADMIN try to deactivate one. The
// store's Owner-Protection (R-OWNERKEY, checked BEFORE last-owner) blocks it →
// 403 ErrOwnerProtected, and the owner key stays active: an admin cannot
// neutralise the tenant's owner.
func TestApiKeyUpdate_OwnerProtection_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, nil)

	tid := be5SeedTenant(t, pool, "be6ku-twoowners")
	scope := "be6ku-twoowners:keys"
	be5SeedScope(t, pool, scope, tid)
	owner1 := be6SeedKey(t, pool, tid, scope, "owner", true)
	_ = be6SeedKey(t, pool, tid, scope, "owner", true) // second active owner → owner1 is NOT last

	off := false
	rec := be5ScopeManageAs(t, h, be5TenantAdmin(tid), map[string]any{
		"action": "api-key-update",
		"data":   map[string]any{"id": owner1, "active": off},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin deactivates owner key: status %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "owner key may only be managed") {
		t.Errorf("N19 403 is not the owner-protection guard (body=%s)", rec.Body.String())
	}
	if !be6KeyActive(t, pool, owner1) {
		t.Errorf("admin deactivated an owner key despite 403 (owner-protection breached)")
	}
}
