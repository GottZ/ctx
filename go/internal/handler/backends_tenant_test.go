package handler

import (
	"encoding/json"
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

// TestBackendListHidesUnwritableKeyRefs (§B): backend-list gives api_key_ref
// only on rows the caller may write. A tenant-admin sees the _global rows but
// not the operator's secret names on them — /api/secrets lists it only its own
// names and the api_key_ref 422 hides _global names on purpose; its own rows
// keep the field (the edit dialog round-trips it). A server-admin sees every
// ref. RED before the filter: the tenant-admin listing carried
// "api_key_ref":"operator-key" on the _global row.
func TestBackendListHidesUnwritableKeyRefs(t *testing.T) {
	bp := backends.NewPool(nil, nil)
	bp.SeedSnapshotForTest([]backends.Backend{
		{ID: "g1", Name: "shared-cloud", Scope: backends.GlobalScope, APIKeyRef: "operator-key", Trust: backends.TrustNoCredentials, Locality: backends.LocalityExternal, Roles: []string{backends.RoleSynthesis}, Enabled: true},
		{ID: "a1", Name: "tenant-a-cloud", Scope: "tenant-a", APIKeyRef: "tenant-a-key", Trust: backends.TrustNoCredentials, Locality: backends.LocalityExternal, Roles: []string{backends.RoleSynthesis}, Enabled: true},
	})
	refs := func(ar *auth.AuthResult) map[string]any {
		t.Helper()
		rec := manageReqWithPool(t, ar, bp, map[string]any{"action": "backend-list"})
		if rec.Code != http.StatusOK {
			t.Fatalf("list: status %d", rec.Code)
		}
		var resp struct {
			Backends []map[string]any `json:"backends"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		out := map[string]any{}
		for _, b := range resp.Backends {
			ref, ok := b["api_key_ref"]
			if !ok {
				ref = nil
			}
			out[b["name"].(string)] = ref
		}
		return out
	}

	ta := refs(tenantAdminScopedAR("tenant-a"))
	if got, ok := ta["shared-cloud"]; !ok {
		t.Fatalf("tenant-a list lost the _global row: %v", ta)
	} else if got != nil {
		t.Errorf("tenant-a list discloses the operator's secret name on a _global row: api_key_ref=%v", got)
	}
	if got := ta["tenant-a-cloud"]; got != "tenant-a-key" {
		t.Errorf("tenant-a list dropped the ref of its own row: api_key_ref=%v", got)
	}
	if body := manageReqWithPool(t, tenantAdminScopedAR("tenant-a"), bp, map[string]any{"action": "backend-list"}).Body.String(); strings.Contains(body, "operator-key") {
		t.Errorf("operator secret name anywhere in the tenant-admin list: %s", body)
	}

	sa := refs(serverAdminScopedAR())
	if sa["shared-cloud"] != "operator-key" || sa["tenant-a-cloud"] != "tenant-a-key" {
		t.Errorf("server-admin list must carry every ref: %v", sa)
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
