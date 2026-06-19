//go:build integration

// Integration test for MT wave T22 (05-A5, Leak-Pfad L3, design/05 §5.2): the
// api-key mint gate. A non-server-admin may mint keys ONLY for scopes its own
// tenant owns (context_tenant_scopes, Modell C); a foreign scope is 403. A
// server-admin mints any scope, byte-identical to today.
//
// The gate is exercised by calling handleApiKeyCreate DIRECTLY, bypassing the
// action-tier admin gate (requireAdminAction) that still rejects EVERY
// non-server-admin until T25/05-A8. That is by design (design/05 §5.4
// pausability): A5 builds the scope gate dormant; A8 later lets a tenant-admin
// reach the handler. Probing the handler directly isolates exactly the A5 cut —
// otherwise the action gate would 403 a tenant-admin for the WRONG reason and the
// test would falsely pass.
//
// RED (pre-T22): handleApiKeyCreate ignored scope ownership — a tenant-admin of A
// minting B's scope reached store.CreateApiKey and got 200 (the L3 leak: a key
// with read access to a foreign corpus). GREEN: 403 before any mint. The
// verify-lens mutation that drops the gate turns the foreign cases 200 → red.
//
//	go test -tags=integration ./internal/handler/ -run TestApiKeyCreate_MintGate -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/testdb"
)

// mintGateTenant registers a tenant and returns its UUID.
func mintGateTenant(t *testing.T, pool *pgxpool.Pool, slug string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_tenants (slug, display_name) VALUES ($1, $2) RETURNING id::text`,
		slug, slug).Scan(&id); err != nil {
		t.Fatalf("create tenant %q: %v", slug, err)
	}
	return id
}

// mintGateMapScope maps one scope to a tenant (context_tenant_scopes, Modell C:
// one scope ⇒ one tenant).
func mintGateMapScope(t *testing.T, pool *pgxpool.Pool, scope, tenantID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1, $2::uuid)`,
		scope, tenantID); err != nil {
		t.Fatalf("map scope %q→%s: %v", scope, tenantID, err)
	}
}

// mintReq calls handleApiKeyCreate directly with the given AuthResult injected,
// bypassing the action-tier gate (see file header).
func mintReq(t *testing.T, h *ManageHandler, ar *auth.AuthResult, homeScope string, allowed []string) *httptest.ResponseRecorder {
	t.Helper()
	data := map[string]any{"label": "t22-key", "home_scope": homeScope}
	if allowed != nil {
		data["allowed_scopes"] = allowed
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/manage", nil)
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	h.handleApiKeyCreate(rec, req, manageRequest{Action: "api-key-create", Data: raw})
	return rec
}

func TestApiKeyCreate_MintGate_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)

	tenantA := mintGateTenant(t, pool, "t22-tenant-a")
	tenantB := mintGateTenant(t, pool, "t22-tenant-b")
	mintGateMapScope(t, pool, "t22-a", tenantA)
	mintGateMapScope(t, pool, "t22-b", tenantB)

	h := NewManageHandler(pool, nil, nil, nil, nil, nil)

	tenantAdminA := &auth.AuthResult{IsValid: true, IsAdmin: false, TenantID: tenantA, TenantRole: auth.RoleAdmin}
	// server-admin's own tenant is irrelevant — it skips the gate entirely.
	serverAdmin := &auth.AuthResult{IsValid: true, IsAdmin: true, TenantID: tenantA, TenantRole: auth.RoleMember}

	// L3, THE canonical negative probe: tenant-admin of A minting a scope owned
	// by B → 403, no key minted. (Mutation: drop the gate → store.CreateApiKey
	// runs → 200 → this case goes red.)
	t.Run("TenantAdmin_ForeignScope403", func(t *testing.T) {
		rec := mintReq(t, h, tenantAdminA, "t22-b", nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("foreign home_scope mint: status %d, want 403 (L3 leak; body=%s)", rec.Code, rec.Body.String())
		}
	})

	// A foreign scope hidden in allowed_scopes is caught too (the gate checks the
	// full requested set, not just home_scope).
	t.Run("TenantAdmin_ForeignAllowedScope403", func(t *testing.T) {
		rec := mintReq(t, h, tenantAdminA, "t22-a", []string{"t22-b"})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("foreign allowed_scope mint: status %d, want 403 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	// Own scope → 200: the gate never blocks legitimate same-tenant minting.
	t.Run("TenantAdmin_OwnScope200", func(t *testing.T) {
		rec := mintReq(t, h, tenantAdminA, "t22-a", []string{"t22-a"})
		if rec.Code != http.StatusOK {
			t.Fatalf("own-scope mint: status %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	// An unowned scope (mapped to NO tenant) is also rejected for a
	// non-server-admin — a tenant-admin cannot claim a fresh scope (fail-closed;
	// new partitions are a server-admin / tenant-lifecycle act).
	t.Run("TenantAdmin_UnownedScope403", func(t *testing.T) {
		rec := mintReq(t, h, tenantAdminA, "t22-unmapped", nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("unowned-scope mint: status %d, want 403 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	// server-admin mints ANY scope, byte-identical to today (§5.4 pausability) —
	// the gate is skipped entirely for IsServerAdmin().
	t.Run("ServerAdmin_ForeignScope200", func(t *testing.T) {
		rec := mintReq(t, h, serverAdmin, "t22-b", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("server-admin foreign mint: status %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
	})
}
