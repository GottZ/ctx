//go:build integration

// Integration tests for BE5-3 (Masterplan K1; design/04 §D-A, design/01 §3): the
// self-service scope-create + scope-list manage actions, end to end through
// HandleManage (gate → dispatch → handler → store) against a real PG18
// testcontainer.
//
// What is proven here (the §7 Negativ-Proben-Matrix subset for this wave):
//   - N16  member → 403 (the TIER proof, negative direction: a member of its own
//          tenant cannot provision a scope — requireTenantAdmin fires before store)
//   - N16+ tenant-admin → 200 (the TIER proof, positive direction: the tenant-admin
//          tier ADMITS scope-create — the Masterplan K10 trap, a scope-create
//          mis-tiered into tierServerAdmin would make this go red)
//   - N1   two tenants, the SAME name → two DISTINCT '<slug>:<name>' scopes, no
//          cross-tenant 409 (S1: the server-side prefix makes collision impossible)
//   - N3   off-charset names (':' / whitespace / '_' / uppercase / edge-hyphen /
//          over the 24-char cap) → 400 (the S1-bearing name gate)
//   - N7   over max_scopes → 429 (S3 transactional cap)
//   - scope-list returns ONLY the caller's own scopes, never a foreign tenant's
//
// Driven THROUGH HandleManage (not the handler directly) on purpose: the whole
// point of N16/N16+ is that enforceActionTier classifies scope-create as
// tierTenantAdmin. Calling the handler directly would bypass exactly the gate
// under test.
//
//	go test -tags=integration ./internal/handler/ -run 'TestScopeCreate|TestScopeList' -count=1 -v
package handler

import (
	"bytes"
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

// be5ScopeManageAs drives HandleManage with a real handler + pool and the given
// AuthResult injected, so the action-tier gate (enforceActionTier) is actually
// exercised — unlike the direct-handler probes of the mint-gate test.
func be5ScopeManageAs(t *testing.T, h *ManageHandler, ar *auth.AuthResult, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	h.HandleManage(rec, req)
	return rec
}

// be5SeedTenant registers a tenant (slug must satisfy slugPattern) and returns
// its UUID. The slug doubles as display_name; the rest are 059 defaults.
func be5SeedTenant(t *testing.T, pool *pgxpool.Pool, slug string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_tenants (slug, display_name) VALUES ($1, $2) RETURNING id::text`,
		slug, slug).Scan(&id); err != nil {
		t.Fatalf("seed tenant %q: %v", slug, err)
	}
	return id
}

// be5SeedScope maps one scope to a tenant directly (context_tenant_scopes).
func be5SeedScope(t *testing.T, pool *pgxpool.Pool, scope, tenantID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1, $2::uuid)`,
		scope, tenantID); err != nil {
		t.Fatalf("seed scope %q→%s: %v", scope, tenantID, err)
	}
}

// be5SetMaxScopes stamps the structural cap on the typed 069 column.
func be5SetMaxScopes(t *testing.T, pool *pgxpool.Pool, tenantID string, max int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_tenants SET max_scopes = $2 WHERE id = $1::uuid`,
		tenantID, max); err != nil {
		t.Fatalf("set max_scopes=%d on %s: %v", max, tenantID, err)
	}
}

// be5AssertScope asserts a 200 scope-create response echoes the server-built
// scope + binding tenant.
func be5AssertScope(t *testing.T, rec *httptest.ResponseRecorder, wantScope, wantTenant string) {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal %q: %v", rec.Body.String(), err)
	}
	if resp["scope"] != wantScope {
		t.Errorf("scope = %v, want %q (body=%s)", resp["scope"], wantScope, rec.Body.String())
	}
	if resp["tenant_id"] != wantTenant {
		t.Errorf("tenant_id = %v, want %q", resp["tenant_id"], wantTenant)
	}
}

func be5TenantAdmin(tenantID string) *auth.AuthResult {
	return &auth.AuthResult{IsValid: true, IsAdmin: false, TenantID: tenantID, TenantRole: auth.RoleAdmin}
}

func be5Member(tenantID string) *auth.AuthResult {
	return &auth.AuthResult{IsValid: true, IsAdmin: false, TenantID: tenantID, TenantRole: auth.RoleMember}
}

func TestScopeCreate_Handler_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil)

	tenantA := be5SeedTenant(t, pool, "be5sc-acme")
	tenantB := be5SeedTenant(t, pool, "be5sc-globex")
	adminA := be5TenantAdmin(tenantA)
	adminB := be5TenantAdmin(tenantB)

	// N1 + N16+: a tenant-admin reaches a 200 (the tier ADMITS it), and two
	// tenants choosing the SAME bare name get two DISTINCT prefixed scopes —
	// no cross-tenant collision/409 (S1).
	t.Run("N1_N16plus_TenantAdmin200_DistinctPrefix", func(t *testing.T) {
		recA := be5ScopeManageAs(t, h, adminA, map[string]any{
			"action": "scope-create", "data": map[string]any{"name": "private"},
		})
		if recA.Code != http.StatusOK {
			t.Fatalf("tenant-admin A scope-create: status %d, want 200 (tier admits; body=%s)", recA.Code, recA.Body.String())
		}
		be5AssertScope(t, recA, "be5sc-acme:private", tenantA)

		recB := be5ScopeManageAs(t, h, adminB, map[string]any{
			"action": "scope-create", "data": map[string]any{"name": "private"},
		})
		if recB.Code != http.StatusOK {
			t.Fatalf("tenant-admin B scope-create same name: status %d, want 200 (no cross-tenant 409; body=%s)", recB.Code, recB.Body.String())
		}
		be5AssertScope(t, recB, "be5sc-globex:private", tenantB)
	})

	// N16: a member of its OWN tenant is 403 — the tier proof's negative
	// direction. requireTenantAdmin fires at enforceActionTier, before the store.
	t.Run("N16_Member403", func(t *testing.T) {
		rec := be5ScopeManageAs(t, h, be5Member(tenantA), map[string]any{
			"action": "scope-create", "data": map[string]any{"name": "blocked"},
		})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("member scope-create: status %d, want 403 (tier gate; body=%s)", rec.Code, rec.Body.String())
		}
	})

	// N3: off-charset names are 400 (the S1-bearing name gate). ':' is the
	// collision-bearer; whitespace/'_'/uppercase/edge-hyphen/over-cap all rejected.
	t.Run("N3_NameCharset400", func(t *testing.T) {
		for _, name := range []string{
			"a:b",                   // ':' — the prefix separator, S1 vector
			"a b",                   // internal whitespace
			"_x",                    // '_' (system-reserved namespace)
			"Acme",                  // uppercase
			"-x",                    // leading hyphen
			"x-",                    // trailing hyphen
			strings.Repeat("a", 25), // over the 24-char ceiling
		} {
			rec := be5ScopeManageAs(t, h, adminA, map[string]any{
				"action": "scope-create", "data": map[string]any{"name": name},
			})
			if rec.Code != http.StatusBadRequest {
				t.Errorf("name=%q: status %d, want 400 (body=%s)", name, rec.Code, rec.Body.String())
			}
		}
	})

	// A duplicate (same name → same scope string) is a 409, not a silent re-assign.
	t.Run("Duplicate409", func(t *testing.T) {
		body := map[string]any{"action": "scope-create", "data": map[string]any{"name": "dup"}}
		if rec := be5ScopeManageAs(t, h, adminA, body); rec.Code != http.StatusOK {
			t.Fatalf("first dup create: status %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		if rec := be5ScopeManageAs(t, h, adminA, body); rec.Code != http.StatusConflict {
			t.Fatalf("second dup create: status %d, want 409", rec.Code)
		}
	})
}

// TestScopeCreate_Quota_Integration is the N7 cap proof, isolated in its own
// tenant so the scope count is deterministic: max_scopes=1 admits the first
// scope and 429s the second.
func TestScopeCreate_Quota_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil)

	tid := be5SeedTenant(t, pool, "be5sc-capped")
	be5SetMaxScopes(t, pool, tid, 1)
	admin := be5TenantAdmin(tid)

	if rec := be5ScopeManageAs(t, h, admin, map[string]any{
		"action": "scope-create", "data": map[string]any{"name": "one"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("first scope (cap=1): status %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	// N7: the second scope exceeds max_scopes=1 → 429 (the transactional cap).
	if rec := be5ScopeManageAs(t, h, admin, map[string]any{
		"action": "scope-create", "data": map[string]any{"name": "two"},
	}); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second scope (cap=1): status %d, want 429 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestScopeList_Integration proves scope-list returns ONLY the caller's own
// tenant scopes (server-filtered on ar.TenantID — a foreign tenant's scope is
// never enumerable), and that a member is 403 at the tier gate.
func TestScopeList_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil)

	tenantA := be5SeedTenant(t, pool, "be5sl-acme")
	tenantB := be5SeedTenant(t, pool, "be5sl-globex")
	be5SeedScope(t, pool, "be5sl-acme:owned", tenantA)
	be5SeedScope(t, pool, "be5sl-globex:foreign", tenantB)

	rec := be5ScopeManageAs(t, h, be5TenantAdmin(tenantA), map[string]any{"action": "scope-list"})
	if rec.Code != http.StatusOK {
		t.Fatalf("scope-list: status %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "be5sl-acme:owned") {
		t.Errorf("scope-list missing the caller's own scope: %s", body)
	}
	if strings.Contains(body, "be5sl-globex:foreign") {
		t.Errorf("scope-list LEAKED a foreign-tenant scope (S2 violation): %s", body)
	}

	// A member cannot enumerate either — the tier gate is symmetric with create.
	if rec := be5ScopeManageAs(t, h, be5Member(tenantA), map[string]any{"action": "scope-list"}); rec.Code != http.StatusForbidden {
		t.Fatalf("member scope-list: status %d, want 403 (tier gate)", rec.Code)
	}
}
