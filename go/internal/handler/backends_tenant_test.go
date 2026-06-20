package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
)

// MT T37 (04-W5): per-tenant backend admin. These probes pin the handler-level
// tenant cuts that DON'T need a DB — scope resolution, the backend-list
// visibility filter, and the update/delete 404-no-oracle pre-check. The
// store-layer scope gate (UpdateBackend/DeleteBackend WHERE scope = ANY) is
// proved end-to-end against real PG in backends_gate_integration_test.go.

func serverAdminScopedAR() *auth.AuthResult {
	return &auth.AuthResult{
		ApiKeyID: "srv-admin", HomeScope: "_global",
		IsValid: true, IsAdmin: true,
	}
}

func tenantAdminScopedAR(scope string) *auth.AuthResult {
	return &auth.AuthResult{
		ApiKeyID: "ta-" + scope, HomeScope: scope,
		ReadScopes: []string{scope, "shared"},
		IsValid:    true, IsAdmin: false,
		TenantID: "tid-" + scope, TenantRole: auth.RoleAdmin,
	}
}

// TestBackendCreateScope: a tenant-admin's new backend is ALWAYS pinned to its
// own scope (payload ignored); only a server-admin chooses, defaulting to
// _global. A blank server-admin choice normalizes to _global.
func TestBackendCreateScope(t *testing.T) {
	str := func(s string) *string { return &s }
	tenantA := tenantAdminScopedAR("tenant-a")
	server := serverAdminScopedAR()
	cases := []struct {
		name      string
		ar        *auth.AuthResult
		specScope *string
		want      string
	}{
		{"tenant-admin forced to own", tenantA, nil, "tenant-a"},
		{"tenant-admin ignores payload scope", tenantA, str("_global"), "tenant-a"},
		{"tenant-admin ignores foreign payload", tenantA, str("tenant-b"), "tenant-a"},
		{"server-admin explicit tenant", server, str("tenant-b"), "tenant-b"},
		{"server-admin explicit global", server, str("_global"), "_global"},
		{"server-admin default", server, nil, backends.GlobalScope},
		{"server-admin blank normalizes", server, str(""), backends.GlobalScope},
	}
	for _, c := range cases {
		if got := backendCreateScope(c.ar, c.specScope); got != c.want {
			t.Errorf("%s: backendCreateScope = %q, want %q", c.name, got, c.want)
		}
	}
}

func tenantPoolFixture() *backends.Pool {
	bp := backends.NewPool(nil, nil)
	bp.SeedSnapshotForTest([]backends.Backend{
		{ID: "g1", Name: "shared-gpu", Scope: backends.GlobalScope, Trust: backends.TrustFull, Roles: []string{backends.RoleSynthesis}, Enabled: true},
		{ID: "a1", Name: "tenant-a-cloud", Scope: "tenant-a", Trust: backends.TrustNoCredentials, Locality: backends.LocalityExternal, Roles: []string{backends.RoleSynthesis}, Enabled: true},
		{ID: "b1", Name: "tenant-b-cloud", Scope: "tenant-b", Trust: backends.TrustNoCredentials, Locality: backends.LocalityExternal, Roles: []string{backends.RoleSynthesis}, Enabled: true},
	})
	return bp
}

// TestBackendListTenantFiltered: a server-admin sees every row; a tenant-admin
// sees _global ∪ its own, never a foreign tenant-private backend (egress
// topology). Red against an unfiltered list (the pre-T37 state).
func TestBackendListTenantFiltered(t *testing.T) {
	bp := tenantPoolFixture()

	// server-admin: all three.
	rec := manageReqWithPool(t, serverAdminScopedAR(), bp, map[string]any{"action": "backend-list"})
	if rec.Code != http.StatusOK {
		t.Fatalf("server-admin list: status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, name := range []string{"shared-gpu", "tenant-a-cloud", "tenant-b-cloud"} {
		if !strings.Contains(body, name) {
			t.Errorf("server-admin list missing %q: %s", name, body)
		}
	}

	// tenant-a admin: _global + own, NOT tenant-b.
	rec = manageReqWithPool(t, tenantAdminScopedAR("tenant-a"), bp, map[string]any{"action": "backend-list"})
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant-a list: status %d", rec.Code)
	}
	body = rec.Body.String()
	if !strings.Contains(body, "shared-gpu") {
		t.Errorf("tenant-a list lost the shared backend: %s", body)
	}
	if !strings.Contains(body, "tenant-a-cloud") {
		t.Errorf("tenant-a list lost its own backend: %s", body)
	}
	if strings.Contains(body, "tenant-b-cloud") {
		t.Errorf("egress topology leak: tenant-a list discloses foreign backend: %s", body)
	}
}

// TestBackendUpdateForeign404: a tenant-admin updating a foreign OR a _global
// backend gets 404 BEFORE the validation path (no 422-vs-404 oracle, no store
// reach) — own backend passes the pre-check (and would reach the store, proved
// in the integration test). _global is visible but NOT writable by a tenant.
func TestBackendUpdateForeign404(t *testing.T) {
	bp := tenantPoolFixture()
	ta := tenantAdminScopedAR("tenant-a")

	// foreign tenant-private → 404
	rec := manageReqWithPool(t, ta, bp, map[string]any{
		"action": "backend-update", "id": "b1",
		"data": map[string]any{"enabled": false},
	})
	if rec.Code != http.StatusNotFound {
		t.Errorf("tenant-a update on foreign backend: status %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}

	// shared _global → visible but not writable → 404 (not 422/200)
	rec = manageReqWithPool(t, ta, bp, map[string]any{
		"action": "backend-update", "id": "g1",
		"data": map[string]any{"enabled": false},
	})
	if rec.Code != http.StatusNotFound {
		t.Errorf("tenant-a update on _global backend: status %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}

	// unknown id → 404 (unchanged)
	rec = manageReqWithPool(t, ta, bp, map[string]any{
		"action": "backend-update", "id": "missing",
		"data": map[string]any{"enabled": false},
	})
	if rec.Code != http.StatusNotFound {
		t.Errorf("tenant-a update on unknown id: status %d, want 404", rec.Code)
	}
}
