package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
)

// makeManageRequest builds a /api/manage POST with an authenticated context.
// It returns the recorded response.
func makeManageRequest(t *testing.T, body any) *httptest.ResponseRecorder {
	t.Helper()
	jsonBody, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	// Construct the handler with a nil pool — handlers that hit the DB will
	// crash on dereference, but the validation paths we test return before
	// any pool call. dreamController is also nil (not exercised here).
	h := NewManageHandler(nil, nil, nil, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")

	// Inject a valid ADMIN auth result so the handler passes the admin gate
	// (K2/G03) and the unauth-early-return — these tests probe the
	// validation layer BEHIND the gate. Non-admin 403 behavior is covered
	// in admin_gate_test.go.
	ar := &auth.AuthResult{
		ApiKeyID:      "test-key-id",
		HomeScope:     "private",
		AllowedScopes: []string{"shared"},
		ReadScopes:    []string{"private", "shared"},
		IsValid:       true,
		IsAdmin:       true,
	}
	ctx := context.WithValue(req.Context(), authResultKey, ar)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.HandleManage(rec, req)
	return rec
}

// SECURITY PROPERTY: api-key-create without home_scope is rejected (400).
// v2.0.0 breaking change — home_scope must be explicit; no implicit
// 'private' default. Multi-tenant correctness relies on this.
func TestApiKeyCreate_MissingHomeScope(t *testing.T) {
	rec := makeManageRequest(t, map[string]any{
		"action": "api-key-create",
		"data":   map[string]any{"label": "test-key"},
	})

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if ok, _ := resp["success"].(bool); ok {
		t.Errorf("success = true, want false")
	}
	errMsg, _ := resp["error"].(string)
	if !strings.Contains(errMsg, "home_scope") {
		t.Errorf("error = %q, want it to mention 'home_scope'", errMsg)
	}
}

// SECURITY PROPERTY: api-key-create with empty home_scope string is rejected.
// Distinct from missing-field, because an explicit "" should not be coerced
// into a default scope.
func TestApiKeyCreate_EmptyHomeScope(t *testing.T) {
	rec := makeManageRequest(t, map[string]any{
		"action": "api-key-create",
		"data": map[string]any{
			"label":      "test-key",
			"home_scope": "",
		},
	})

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	errMsg, _ := resp["error"].(string)
	if !strings.Contains(errMsg, "home_scope") {
		t.Errorf("error = %q, want it to mention 'home_scope'", errMsg)
	}
}

// SECURITY PROPERTY: api-key-create with missing label is rejected (400).
// Both label and home_scope are required; label is validated first.
func TestApiKeyCreate_MissingLabel(t *testing.T) {
	rec := makeManageRequest(t, map[string]any{
		"action": "api-key-create",
		"data":   map[string]any{"home_scope": "crag"},
	})

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	errMsg, _ := resp["error"].(string)
	if !strings.Contains(errMsg, "label") {
		t.Errorf("error = %q, want it to mention 'label'", errMsg)
	}
}

// SECURITY PROPERTY: api-key-create with malformed data field is rejected.
// Defends against JSON parse panics in handler dispatch.
func TestApiKeyCreate_MalformedData(t *testing.T) {
	// req.Data is json.RawMessage; sending a non-object value (e.g. a string)
	// causes Unmarshal into apiKeyCreateRequest to fail.
	body := []byte(`{"action":"api-key-create","data":"not-an-object"}`)

	h := NewManageHandler(nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	ar := &auth.AuthResult{ApiKeyID: "id", IsValid: true, IsAdmin: true}
	ctx := context.WithValue(req.Context(), authResultKey, ar)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.HandleManage(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestResolveApiKeyListParams pins T23 (05-A6) handler logic without a DB: the
// active_only default (the named behavior change, design 05 §6.2) and the L1
// tenant scoping (server-admin → all tenants; non-server-admin → own only).
func TestResolveApiKeyListParams(t *testing.T) {
	serverAdmin := &auth.AuthResult{IsValid: true, IsAdmin: true, TenantID: "tenant-sa"}
	tenantAdmin := &auth.AuthResult{IsValid: true, IsAdmin: false, TenantID: "tenant-x"}

	t.Run("absent active_only defaults to active-only; server-admin lists all", func(t *testing.T) {
		filter, activeOnly := resolveApiKeyListParams(nil, serverAdmin)
		if !activeOnly {
			t.Error("default must be active-only (named behavior change)")
		}
		if filter != "" {
			t.Errorf("server-admin filter = %q, want empty (all tenants)", filter)
		}
	})
	t.Run("explicit active_only=false restores the full view", func(t *testing.T) {
		filter, activeOnly := resolveApiKeyListParams(json.RawMessage(`{"active_only":false}`), serverAdmin)
		if activeOnly {
			t.Error("active_only=false must disable the active filter")
		}
		if filter != "" {
			t.Error("server-admin still lists all tenants under active_only=false")
		}
	})
	t.Run("non-server-admin is scoped to its own tenant", func(t *testing.T) {
		filter, activeOnly := resolveApiKeyListParams(nil, tenantAdmin)
		if filter != "tenant-x" {
			t.Errorf("filter = %q, want own tenant (L1 isolation)", filter)
		}
		if !activeOnly {
			t.Error("default active-only applies to tenant-admins too")
		}
	})
}

// SECURITY PROPERTY: Action 'api-key-delete' requires an id field.
// Without it, the handler returns 400 — no DB call, no nil-pool panic.
func TestApiKeyDelete_MissingID(t *testing.T) {
	rec := makeManageRequest(t, map[string]any{
		"action": "api-key-delete",
		"data":   map[string]any{},
	})

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}
