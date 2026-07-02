//go:build integration

// Integration tests for BE6-5 (design/04 §D-B, S2/AM2/AM3): the tenant_id
// binding + key-quota on api-key-create, end to end through HandleManage
// (gate → dispatch → handler → store) against a real PG18 testcontainer.
//
// What is proven here (the §7 Negativ-Proben-Matrix subset for this wave):
//   - Regression: a tenant-admin minting in its OWN tenant → 200, tenant_role
//     == "member" echoed (SEC-6 response field + AM3 default role)
//   - N4   non-server-admin sets data.tenant_id → 403 "server-admin only" (AM2):
//          the foreign-tenant channel is dead for a tenant-admin, before any bind
//   - N5   home_scope outside the caller's own tenant → 403 (the existing self-mint
//          T22, unchanged)
//   - N6   server-admin targets tenant B with A's scope → 403 (foreign-target T22):
//          no cross-tenant write-key; the legit counterpart (B's scope) → 200/bound-to-B
//   - N8   over max_keys → 429 (S3 transactional, active-only cap)
//   - AM3  a role field in the payload is IGNORED → the minted key is 'member'
//          (apiKeyCreateRequest has no role field — no self-elevation via create)
//
// Driven THROUGH HandleManage (not the handler directly) so the action-tier gate
// (api-key-create == tierTenantAdmin, UNCHANGED by BE6-5) is actually exercised:
// a tenant-admin clears the gate and reaches the binding logic, where the S2 gates
// fire. Helpers (be5*) are shared with scope_create_handler_test.go (same package,
// same build tag).
//
//	go test -tags=integration ./internal/handler/ -run 'TestApiKeyCreate_Tenant|TestApiKeyCreate_KeyQuota' -count=1 -v
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

// be6ServerAdmin builds a server-admin AuthResult. Its home tenant is irrelevant
// when it targets a tenant via data.tenant_id (bindingTenant = the target), but a
// non-empty tenant_id is set to mirror ctx_auth's guarantee for valid keys.
func be6ServerAdmin(tenantID string) *auth.AuthResult {
	return &auth.AuthResult{IsValid: true, IsAdmin: true, TenantID: tenantID, TenantRole: auth.RoleMember}
}

// be6SetMaxKeys stamps the structural key cap on the typed 069 column.
func be6SetMaxKeys(t *testing.T, pool *pgxpool.Pool, tenantID string, max int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_tenants SET max_keys = $2 WHERE id = $1::uuid`,
		tenantID, max); err != nil {
		t.Fatalf("set max_keys=%d on %s: %v", max, tenantID, err)
	}
}

// be6AssertRole asserts a 200 api-key-create response carries tenant_role (the
// SEC-6 response field) equal to wantRole.
func be6AssertRole(t *testing.T, rec *httptest.ResponseRecorder, wantRole string) {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal %q: %v", rec.Body.String(), err)
	}
	role, ok := resp["tenant_role"]
	if !ok {
		t.Fatalf("response missing tenant_role (SEC-6): %s", rec.Body.String())
	}
	if role != wantRole {
		t.Errorf("tenant_role = %v, want %q (body=%s)", role, wantRole, rec.Body.String())
	}
}

// be6KeyTenant reads the bound tenant_id of a minted key (server-side truth, not
// the echoed response).
func be6KeyTenant(t *testing.T, pool *pgxpool.Pool, keyID any) string {
	t.Helper()
	var tenant string
	if err := pool.QueryRow(context.Background(),
		`SELECT tenant_id::text FROM context_api_keys WHERE id = $1::uuid`, keyID).Scan(&tenant); err != nil {
		t.Fatalf("read minted key tenant: %v", err)
	}
	return tenant
}

// be6KeyRole reads the stored tenant_role of a minted key.
func be6KeyRole(t *testing.T, pool *pgxpool.Pool, keyID any) string {
	t.Helper()
	var role string
	if err := pool.QueryRow(context.Background(),
		`SELECT tenant_role FROM context_api_keys WHERE id = $1::uuid`, keyID).Scan(&role); err != nil {
		t.Fatalf("read minted key role: %v", err)
	}
	return role
}

func respID(t *testing.T, rec *httptest.ResponseRecorder) any {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal %q: %v", rec.Body.String(), err)
	}
	return resp["id"]
}

func TestApiKeyCreate_TenantBinding_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, nil)

	tenantA := be5SeedTenant(t, pool, "be6kc-acme")
	tenantB := be5SeedTenant(t, pool, "be6kc-globex")
	scopeA := "be6kc-acme:keys"
	scopeB := "be6kc-globex:keys"
	be5SeedScope(t, pool, scopeA, tenantA)
	be5SeedScope(t, pool, scopeB, tenantB)
	adminA := be5TenantAdmin(tenantA)
	serverAdmin := be6ServerAdmin(tenantA)

	// Regression: a tenant-admin minting in its OWN tenant with its own scope →
	// 200, and the response carries tenant_role == "member" (SEC-6 field present +
	// AM3 default). The CreateApiKey→MintKeyWithQuota swap must keep the happy path.
	t.Run("Regression_OwnTenantMint200_RoleMember", func(t *testing.T) {
		rec := be5ScopeManageAs(t, h, adminA, map[string]any{
			"action": "api-key-create",
			"data":   map[string]any{"label": "own", "home_scope": scopeA},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("own-tenant mint: status %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		be6AssertRole(t, rec, "member")
		if got := be6KeyTenant(t, pool, respID(t, rec)); got != tenantA {
			t.Errorf("minted key bound to %s, want tenantA %s", got, tenantA)
		}
	})

	// N4: a non-server-admin that sets data.tenant_id is rejected 403 BEFORE the
	// field can bind anything (AM2). Probed with an OWN, otherwise-valid scope so
	// the only thing under test is the tenant_id channel — a silent-ignore
	// regression would 200 (or T22-403 for a foreign scope), this pins the AM2 403.
	t.Run("N4_NonAdminTenantID403", func(t *testing.T) {
		rec := be5ScopeManageAs(t, h, adminA, map[string]any{
			"action": "api-key-create",
			"data":   map[string]any{"label": "x", "home_scope": scopeA, "tenant_id": tenantB},
		})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("non-admin tenant_id: status %d, want 403 (AM2; body=%s)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "server-admin only") {
			t.Errorf("N4 403 is not the AM2 tenant_id gate (body=%s)", rec.Body.String())
		}
	})

	// N5: home_scope outside the caller's own tenant → 403 (the existing self-mint
	// T22, unchanged by BE6-5). adminA cannot mint with B's scope.
	t.Run("N5_ForeignHomeScope403", func(t *testing.T) {
		rec := be5ScopeManageAs(t, h, adminA, map[string]any{
			"action": "api-key-create",
			"data":   map[string]any{"label": "x", "home_scope": scopeB},
		})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("foreign home_scope mint: status %d, want 403 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	// N6: a server-admin targeting tenant B must pick B's scopes. Targeting B with
	// A's scope → 403 (foreign-target T22) — no cross-tenant write-key.
	t.Run("N6_ServerAdminForeignTargetScope403", func(t *testing.T) {
		rec := be5ScopeManageAs(t, h, serverAdmin, map[string]any{
			"action": "api-key-create",
			"data":   map[string]any{"label": "x", "home_scope": scopeA, "tenant_id": tenantB},
		})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("server-admin foreign-target scope: status %d, want 403 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	// N6+: the legitimate counterpart — server-admin mints FOR tenant B with B's
	// own scope → 200, the key bound to B (the foreign-target path admits the
	// valid case; limits are read for the TARGET, not ar.TenantID).
	t.Run("N6plus_ServerAdminForTenantB200", func(t *testing.T) {
		rec := be5ScopeManageAs(t, h, serverAdmin, map[string]any{
			"action": "api-key-create",
			"data":   map[string]any{"label": "forB", "home_scope": scopeB, "tenant_id": tenantB},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("server-admin mint for B: status %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		be6AssertRole(t, rec, "member")
		if got := be6KeyTenant(t, pool, respID(t, rec)); got != tenantB {
			t.Errorf("minted key bound to %s, want tenantB %s (S2: bindingTenant is the TARGET)", got, tenantB)
		}
	})

	// AM3: a tenant_role field smuggled into the payload is IGNORED —
	// apiKeyCreateRequest has no such field, MintKeyWithQuota is called with a
	// fixed "member", so the minted key is 'member' (no self-elevation via create).
	t.Run("AM3_RoleFieldIgnored_Member", func(t *testing.T) {
		rec := be5ScopeManageAs(t, h, adminA, map[string]any{
			"action": "api-key-create",
			"data":   map[string]any{"label": "noelev", "home_scope": scopeA, "tenant_role": "owner"},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("mint with bogus tenant_role: status %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		be6AssertRole(t, rec, "member")
		// The stored role, not just the echoed one — defense against an echo-only fix.
		if got := be6KeyRole(t, pool, respID(t, rec)); got != "member" {
			t.Errorf("stored tenant_role = %q, want member (AM3: create never elevates)", got)
		}
	})
}

// TestApiKeyCreate_KeyQuota_Integration is the N8 cap proof, isolated in its own
// tenant so the active-key count is deterministic: max_keys=1 admits the first
// mint and 429s the second (the transactional, active-only cap of MintKeyWithQuota).
func TestApiKeyCreate_KeyQuota_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, nil)

	tid := be5SeedTenant(t, pool, "be6kc-capped")
	scope := "be6kc-capped:keys"
	be5SeedScope(t, pool, scope, tid)
	be6SetMaxKeys(t, pool, tid, 1)
	admin := be5TenantAdmin(tid)

	// First mint fits under max_keys=1 → 200.
	if rec := be5ScopeManageAs(t, h, admin, map[string]any{
		"action": "api-key-create",
		"data":   map[string]any{"label": "one", "home_scope": scope},
	}); rec.Code != http.StatusOK {
		t.Fatalf("first key (cap=1): status %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	// N8: the second mint exceeds max_keys=1 → 429.
	if rec := be5ScopeManageAs(t, h, admin, map[string]any{
		"action": "api-key-create",
		"data":   map[string]any{"label": "two", "home_scope": scope},
	}); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second key (cap=1): status %d, want 429 (body=%s)", rec.Code, rec.Body.String())
	}
}
